package redirect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// CacheKeyVersion is bumped only when the Snapshot encoding changes
// incompatibly. Including it in the key means an upgrade cannot read a stale
// payload written by the previous version, which would otherwise deserialize
// into a plausible-looking wrong answer rather than failing.
const CacheKeyVersion = "v1"

// Result is a resolved alias plus how it was resolved. The source drives the
// cache-hit-ratio metric, which is the leading indicator for the latency SLO:
// a falling ratio predicts an SLO breach before p99 moves.
type Result struct {
	Snapshot *Snapshot
	Source   Source
}

type Source string

const (
	SourceMemory   Source = "memory"
	SourceRedis    Source = "redis"
	SourceDatabase Source = "database"
	SourceNegative Source = "negative"
)

type Options struct {
	TTL         time.Duration
	NegativeTTL time.Duration
	// RedisTimeout is how long the hot path waits for the cache before giving
	// up and going to Postgres. Short by design: a stalled Redis should cost a
	// few milliseconds, not the request.
	RedisTimeout time.Duration

	// InvalidateBudget bounds a whole invalidation — every attempt and every
	// pause between them — rather than each attempt separately. A per-attempt
	// budget multiplies: three attempts at RedisTimeout each meant an operator
	// raising RedisTimeout raised the worst case on their own form submission
	// by three times as much (M26.6, D26).
	InvalidateBudget time.Duration

	// DBTimeout bounds the Postgres fallback. Zero leaves it bounded only by the
	// request context, which for a redirect is no bound worth having: the target
	// is 100ms uncached, and a query still running after a second is not going to
	// produce a useful answer — it is going to hold a connection from the small
	// redirect pool while more requests queue behind it.
	DBTimeout    time.Duration
	MemCacheSize int
	Logger       *slog.Logger
}

// Resolver turns (domain, alias) into a Snapshot.
type Resolver struct {
	q     *dbgen.Queries
	redis *goredis.Client
	mem   *memCache
	opts  Options
	log   *slog.Logger

	// singleflight collapses concurrent misses for the same alias into one
	// database query. Without it, a link going viral means every concurrent
	// request for a cold alias hits Postgres at once — the cache stampede that
	// turns a traffic spike into an outage.
	group singleflight.Group
}

func NewResolver(pool *pgxpool.Pool, rdb *goredis.Client, opts Options) *Resolver {
	if opts.MemCacheSize <= 0 {
		opts.MemCacheSize = 10_000
	}
	if opts.RedisTimeout <= 0 {
		opts.RedisTimeout = 50 * time.Millisecond
	}
	// Large enough to fit the three attempts and their pauses at the default
	// RedisTimeout, so a budget nobody set changes nothing about how a healthy
	// or briefly stalled cache behaves — it only caps the pathological end.
	if opts.InvalidateBudget <= 0 {
		opts.InvalidateBudget = 250 * time.Millisecond
	}
	// The collapsed database flight runs detached from any one request, so it
	// needs a bound of its own even when the caller left DBTimeout at zero —
	// otherwise the one context that used to stop it is gone and nothing
	// replaces it.
	if opts.DBTimeout <= 0 {
		opts.DBTimeout = time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Resolver{
		q:     dbgen.New(pool),
		redis: rdb,
		mem:   newMemCache(opts.MemCacheSize),
		opts:  opts,
		log:   opts.Logger,
	}
}

// key is the cache key. Host-scoped from the start even though Phase 1 has a
// single domain, so Phase 2 custom domains need no key change and no cache
// flush on upgrade.
func key(domainID uuid.UUID, alias string) string {
	return "lc:a:" + CacheKeyVersion + ":" + domainID.String() + ":" + alias
}

// Resolve returns the snapshot for an alias, consulting memory, then Redis,
// then Postgres.
func (r *Resolver) Resolve(ctx context.Context, domainID uuid.UUID, alias string) (Result, error) {
	now := time.Now()
	k := key(domainID, alias)

	if snap, ok := r.mem.get(k, now); ok {
		return Result{Snapshot: snap, Source: sourceFor(snap, SourceMemory)}, nil
	}

	if snap := r.fromRedis(ctx, k, now); snap != nil {
		return Result{Snapshot: snap, Source: sourceFor(snap, SourceRedis)}, nil
	}

	// Collapse concurrent misses. The shared result is used by every waiter,
	// so a stampede costs one query rather than N.
	//
	// The flight runs on a context detached from whichever request happened to
	// start it, with its own budget. singleflight hands the leader's error to
	// every waiter, so a leader whose client hit Stop mid-query cancelled the
	// query and turned every other waiter's redirect into a 503 — the more
	// popular the cold alias, the more people one abandoned tab took with it.
	// The detached context still bounds the work; it just cannot be cancelled
	// by a single visitor on behalf of the rest.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.opts.DBTimeout)
	defer cancel()

	v, err, _ := r.group.Do(k, func() (any, error) {
		return r.fromDatabase(fctx, domainID, alias, k, now)
	})
	if err != nil {
		return Result{}, err
	}
	// Checked rather than asserted: the hot path must not panic on a redirect,
	// and a 404 is a survivable answer where a panic is not.
	snap, ok := v.(*Snapshot)
	if !ok {
		return Result{}, fmt.Errorf("redirect: resolver returned %T, not a snapshot", v)
	}
	return Result{Snapshot: snap, Source: sourceFor(snap, SourceDatabase)}, nil
}

// ResolveCached answers only from the in-process cache. It never touches Redis
// or Postgres, and never populates anything.
//
// This exists for one caller: the redirect handler serving a request that the
// 404-probe limit has throttled. Refusing such a request outright would mean an
// address that tripped the limit could no longer follow a working link — and
// with a proxy misconfigured so that every visitor shares one address, that is
// the whole site. Serving from memory keeps live links working at the cost of a
// single map lookup, while an alias nobody is using still cannot be turned into
// a database query. It is the cheapest operation in the package, which is what
// makes it safe to offer to a client being throttled.
func (r *Resolver) ResolveCached(domainID uuid.UUID, alias string) (Result, bool) {
	k := key(domainID, alias)
	snap, ok := r.mem.get(k, time.Now())
	if !ok {
		return Result{}, false
	}
	return Result{Snapshot: snap, Source: sourceFor(snap, SourceMemory)}, true
}

func sourceFor(s *Snapshot, src Source) Source {
	if s != nil && s.NotFound {
		return SourceNegative
	}
	return src
}

func (r *Resolver) fromRedis(ctx context.Context, k string, now time.Time) *Snapshot {
	if r.redis == nil {
		return nil
	}
	// Its own deadline, independent of the request's. A slow cache must not
	// consume the whole redirect budget.
	rctx, cancel := context.WithTimeout(ctx, r.opts.RedisTimeout)
	defer cancel()

	b, err := r.redis.Get(rctx, k).Bytes()
	if err != nil {
		// Every failure mode is a miss: key absent, Redis down, timeout. The
		// path continues to Postgres and still meets the uncached target,
		// which is why a cache outage degrades latency instead of breaking
		// the service.
		if !errors.Is(err, goredis.Nil) {
			r.log.Debug("redis lookup failed; falling through to postgres",
				slog.String("key", k), slog.Any("error", err))
		}
		return nil
	}

	snap, err := decodeSnapshot(b)
	if err != nil {
		// A corrupt or older-format payload is treated as a miss rather than
		// an error, and dropped so it stops costing a decode on every request.
		r.log.Warn("discarding undecodable cache entry", slog.String("key", k))
		_ = r.redis.Del(context.WithoutCancel(rctx), k).Err()
		return nil
	}

	// Populate the in-process tier so the next hit skips Redis entirely.
	r.mem.set(k, snap, snap.CacheTTL(now, r.opts.TTL, r.opts.NegativeTTL), now)
	return snap
}

// fromDatabase is only ever called inside the singleflight, whose context
// already carries DBTimeout. Bounding it a second time here would just be a
// second copy of the same budget started a moment later.
func (r *Resolver) fromDatabase(ctx context.Context, domainID uuid.UUID, alias, k string, now time.Time) (*Snapshot, error) {
	row, err := r.q.ResolveAliasForRedirect(ctx, dbgen.ResolveAliasForRedirectParams{
		DomainID: domainID, Alias: alias,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			snap := notFoundSnapshot()
			r.store(ctx, k, snap, now)
			return snap, nil
		}
		return nil, fmt.Errorf("resolve alias: %w", err)
	}

	snap := &Snapshot{
		LinkID:       row.ID,
		WorkspaceID:  row.WorkspaceID,
		URL:          row.PrimaryUrl,
		Status:       row.Status,
		ExpiresAt:    row.ExpiresAt,
		ForwardQuery: row.ForwardQuery,
		HasPassword:  row.PasswordHash != nil,
		MaxClicks:    row.MaxClicks,
		OneTime:      row.OneTime,
	}
	r.store(ctx, k, snap, now)
	return snap, nil
}

func (r *Resolver) store(ctx context.Context, k string, snap *Snapshot, now time.Time) {
	ttl := snap.CacheTTL(now, r.opts.TTL, r.opts.NegativeTTL)
	r.mem.set(k, snap, ttl, now)

	if r.redis == nil {
		return
	}
	b, err := snap.encode()
	if err != nil {
		return
	}
	// Detached from the request context: the write must survive the client
	// disconnecting, or a cancelled request leaves the cache cold and the next
	// one pays for the query again.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.opts.RedisTimeout)
	defer cancel()
	if err := r.redis.Set(wctx, k, b, ttl).Err(); err != nil {
		r.log.Debug("failed to populate cache", slog.String("key", k), slog.Any("error", err))
	}
}

// InvalidateAlias drops a cached entry. Implements link.Invalidator.
//
// Called on create as well as update and delete. Create matters because of
// negative caching: an alias somebody probed before it existed would otherwise
// keep returning 404 for the whole negative TTL, and the link would look
// broken the moment it was made.
func (r *Resolver) InvalidateAlias(ctx context.Context, domainID uuid.UUID, alias string) {
	k := key(domainID, alias)
	r.mem.delete(k)

	if r.redis == nil {
		return
	}
	// Retried, unlike the populate path. A failed Set costs one extra query on
	// the next request; a failed Del leaves the *old* snapshot authoritative for
	// up to REDIRECT_TTL, and the memory tier this function just cleared refills
	// from it on the very next miss. The edit looks applied in the dashboard
	// while every visitor keeps reaching the previous destination — including a
	// destination the owner deleted on purpose.
	if err := r.deleteFromRedis(ctx, k); err != nil {
		r.log.Error("failed to invalidate cache entry; the previous destination "+
			"may keep being served until it expires",
			slog.String("alias", alias),
			slog.Duration("stale_for_up_to", r.opts.TTL),
			slog.Any("error", err))
	}

	// Every other replica holds its own in-process copy, which neither the
	// delete above nor this process can reach. Publishing is what closes that
	// gap; before it existed, an edit took up to REDIRECT_TTL to be visible on
	// a replica that had already cached the alias.
	r.publish(ctx, invalidation{Kind: kindAlias, Key: k})
}

// invalidateAttempts is how many times a delete is tried before the entry is
// left to expire by TTL. Three, because the common failure is a brief stall
// rather than a durable outage — and the cost of giving up is serving a
// destination the owner has already changed.
const invalidateAttempts = 3

// deleteFromRedis removes a key, retrying a transient failure inside one total
// budget.
//
// The budget is held out here rather than pushed into the context go-redis is
// given, because that context does not bind a stalled read. Measured for M26.6
// against a server that accepts the connection and then never answers: a client
// whose ReadTimeout was 400ms took 402ms to fail a command carrying a 50ms
// context, and one left at go-redis's 3s default took 3.0s. What bounds a
// stalled read is the client's own ReadTimeout; the caller's deadline is
// consulted for the dial and not for this. So a loop spending RedisTimeout on
// each of three attempts multiplied whatever REDIS_READ_TIMEOUT was set to —
// which is how the same three attempts measured 9.07s while M23 was being
// built, against a test client that had left the timeout at the default.
//
// Bounding the wait from outside the call is what internal/ratelimit does for
// the same failure and the same reason (M24). An attempt still running when the
// budget is spent has its context cancelled, which a stalled read will not
// notice; what makes the bound hold is that the caller has stopped waiting. A
// delete that lands a moment later removes a key that should be gone anyway.
//
// Detached from the caller's context, because the invalidation must complete
// even if the request that triggered the edit has gone away.
func (r *Resolver) deleteFromRedis(ctx context.Context, k string) error {
	base, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), r.opts.InvalidateBudget)

	done := make(chan error, 1)
	go func() {
		defer cancel()
		done <- r.retryDelete(base, k)
	}()

	select {
	case err := <-done:
		return err
	case <-base.Done():
		// The budget can expire in the same instant the delete succeeds, and a
		// bare select would then choose between them at random. Ask once more
		// before reporting a failure that did not happen.
		select {
		case err := <-done:
			return err
		default:
			return fmt.Errorf("invalidation budget of %s spent",
				r.opts.InvalidateBudget)
		}
	}
}

// retryDelete runs the attempts inside whatever budget ctx already carries.
func (r *Resolver) retryDelete(ctx context.Context, k string) error {
	var err error
	for attempt := range invalidateAttempts {
		if attempt > 0 {
			// Short and fixed. This runs after a successful commit, on the
			// write path rather than the redirect path, so a few milliseconds
			// of patience is affordable — but an operator waiting on a form
			// submission is not going to wait out an exponential backoff.
			timer := time.NewTimer(time.Duration(attempt) * 20 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return err
			}
		}
		dctx, cancel := context.WithTimeout(ctx, r.opts.RedisTimeout)
		err = r.redis.Del(dctx, k).Err()
		cancel()
		if err == nil {
			return nil
		}
		// A spent budget ends the loop rather than starting an attempt that
		// cannot finish inside it.
		if ctx.Err() != nil {
			return err
		}
	}
	return err
}

// CacheSize reports in-process entries, for metrics.
func (r *Resolver) CacheSize() int { return r.mem.len() }
