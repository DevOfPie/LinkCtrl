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
	v, err, _ := r.group.Do(k, func() (any, error) {
		return r.fromDatabase(ctx, domainID, alias, k, now)
	})
	if err != nil {
		return Result{}, err
	}
	snap := v.(*Snapshot)
	return Result{Snapshot: snap, Source: sourceFor(snap, SourceDatabase)}, nil
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
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.opts.RedisTimeout)
	defer cancel()
	if err := r.redis.Del(dctx, k).Err(); err != nil {
		r.log.Warn("failed to invalidate cache entry",
			slog.String("alias", alias), slog.Any("error", err))
	}

	// Known limitation: this clears Redis and THIS process's memory tier.
	// Another replica keeps its own copy until the entry's TTL expires, so an
	// edit can take up to REDIRECT_TTL to be visible everywhere. Phase 2 adds
	// pub/sub invalidation; single-replica deployments, which is what Phase 1
	// targets, are unaffected.
}

// CacheSize reports in-process entries, for metrics.
func (r *Resolver) CacheSize() int { return r.mem.len() }
