package redirect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// CacheKeyVersion is bumped only when the Snapshot encoding changes
// incompatibly. Including it in the key means an upgrade cannot read a stale
// payload written by the previous version, which would otherwise deserialize
// into a plausible-looking wrong answer rather than failing.
//
// **v2 is M34's, and it is the phase's one deliberate bump.** Routing rules are
// the first snapshot field whose *absence* means something different from its
// zero value in a way a visitor can observe: an entry written by the previous
// build carries no rules, and a link whose owner has since routed British
// traffic somewhere else would go on sending it to the link's own destination
// for up to REDIRECT_TTL. Every earlier Phase 2 field could argue its way out of
// a bump because the stale reading was the behaviour the link already had —
// bot blocking off, path forwarding off. This one cannot: the stale reading is
// a rule not being applied, which is the control the owner configured being
// silently absent, and that is precisely what a cold cache is for.
//
// The consequence is stated in the CHANGELOG rather than only here: upgrading
// to this version abandons every cached snapshot at once, so the first request
// for each alias after the upgrade reads Postgres. That is one query per live
// alias, spread over however long it takes traffic to arrive, and the
// singleflight in Resolve is what keeps a popular alias from turning into a
// stampede while it happens.
//
// **v3 is M36's**, and it is the same argument twice rather than a second kind
// of argument. Split testing changes the destination list from `[]string` to a
// list of objects carrying an id and a weight, so a v2 payload does not decode
// into the new shape at all — but that alone would only have cost a discarded
// entry, which decodeSnapshot already handles. What forces the bump is the same
// thing that forced v2: a v2 entry carries no split arms, and a link whose owner
// has since divided its traffic between two destinations would keep sending all
// of it to one of them for up to REDIRECT_TTL. The stale reading is a control the
// owner configured being silently absent, and a cold cache is what that costs.
//
// Both bumps land in the same unreleased minor, so no deployed instance ever
// holds a v2 entry this build could meet.
const CacheKeyVersion = "v3"

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

	// unavailableUntil is the instant after which the shared cache is worth
	// writing to again, as unix nanoseconds. Zero means "no failure seen".
	//
	// Not a circuit breaker: reads are never skipped, because a read is how the
	// cache recovers and because a Get against a healthy server is the fast path
	// this whole tier exists for. It suppresses only the repopulating *write*
	// after a read has just failed, for one uncached resolve's worth of time.
	unavailableUntil atomic.Int64
}

// markUnavailable records that Redis just failed to answer a read.
//
// The window is DBTimeout — one uncached resolve — because that is exactly the
// span between the failed lookup and the write it would otherwise provoke. Long
// enough to cover the request that observed the failure and the ones already in
// flight beside it; short enough that a server which comes back is written to
// again on the next miss rather than after a cooldown somebody has to reason
// about.
func (r *Resolver) markUnavailable(now time.Time) {
	r.unavailableUntil.Store(now.Add(r.opts.DBTimeout).UnixNano())
}

func (r *Resolver) redisUnavailable(now time.Time) bool {
	until := r.unavailableUntil.Load()
	return until != 0 && now.UnixNano() < until
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
	return keyPrefix(domainID) + alias
}

// keyPrefix is every cached alias on one domain. Split out because a
// domain-level setting change has to reach all of them and has no list of
// aliases to work from.
func keyPrefix(domainID uuid.UUID) string {
	return "lc:a:" + CacheKeyVersion + ":" + domainID.String() + ":"
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
			// A lookup that failed for any reason other than *absence* is
			// evidence the server will not usefully answer the write that
			// repopulates this key either. Recording that here is what stops
			// the same request paying the timeout a second time in store()
			// (F9): a cold resolve against a stalled Redis measured 108ms, of
			// which 50ms was a Set nobody was going to read.
			r.markUnavailable(now)
		}
		return nil
	}

	snap, err := decodeSnapshot(b)
	if err != nil {
		// A corrupt or older-format payload is treated as a miss rather than
		// an error, and dropped so it stops costing a decode on every request.
		r.log.Warn("discarding undecodable cache entry", slog.String("key", k))
		// WithoutCancel *and* a timeout, like every other detached call in this
		// file. Stripping cancellation without adding a deadline leaves the call
		// bounded by nothing, and D26 leaves go-redis's MaxRetries at its
		// default on the stated ground that every Redis call site here carries
		// one — this was the single site where that was not true (F101).
		dctx, cancel := context.WithTimeout(context.WithoutCancel(rctx), r.opts.RedisTimeout)
		_ = r.redis.Del(dctx, k).Err()
		cancel()
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
		ForwardPath:  row.ForwardPath,
		// The gates (M35). Note what is *not* here: `row.PasswordHash` is
		// reduced to a boolean and the hash itself never enters the struct that
		// gets serialized into Redis. See the field comments on Snapshot.
		HasPassword:      row.PasswordHash != nil && *row.PasswordHash != "",
		MaxClicks:        row.MaxClicks,
		OneTime:          row.OneTime,
		RequireSignature: row.RequireSignature,
		// Carried, not decided. Storing the resolved boolean instead would be
		// cheaper per request and would put the answer somewhere other than
		// domain.BlocksBots — an encoding of a decision whose inputs are no
		// longer readable, which is how the redirect path and the dashboard come
		// to disagree about what a link is doing. The two fields are two string
		// comparisons at request time; that is not the cost worth trading.
		BotPolicy:       domain.BotPolicy(row.BotBlocking),
		DomainBotPolicy: domain.DomainBots(row.BlockBots, row.BlockBotsEnforced),
	}
	r.attachRules(snap, row.Rules, alias)
	r.store(ctx, k, snap, now)
	return snap, nil
}

// queryRule is a rule as the SQL lateral spells it.
//
// A separate type from SnapshotRule on purpose. The query's vocabulary is the
// database's — `url`, `conditions`, the same words the columns use — and the
// snapshot's is a single letter per key because it is serialized on every cache
// write. Letting one drive the other would mean a column rename reaching into
// the cache payload, or the payload's compression reaching into a query
// somebody has to read.
type queryRule struct {
	ID         uuid.UUID             `json:"id"`
	URL        string                `json:"url"`
	Kind       string                `json:"kind"`
	Weight     int32                 `json:"weight"`
	Conditions domain.RuleConditions `json:"conditions"`
}

// attachRules fills in the snapshot's rules and destination list.
//
// Failure here is deliberately not an error return. A link whose rules cannot
// be decoded still has a destination, and refusing to redirect over it would
// turn a malformed condition — something an operator could produce with one
// hand-written UPDATE — into an outage for that alias. The rules are dropped,
// the link behaves as though it had none, and the log says so once per resolve
// rather than once per request, because this runs on the miss path only.
func (r *Resolver) attachRules(snap *Snapshot, raw []byte, alias string) {
	if len(raw) == 0 {
		return
	}
	var rows []queryRule
	if err := json.Unmarshal(raw, &rows); err != nil {
		r.log.Error("a link's routing rules could not be read and are being ignored; "+
			"it will redirect to its own destination",
			slog.String("alias", alias), slog.Any("error", err))
		return
	}
	if len(rows) == 0 {
		return
	}

	// Deduplicated, because two rules pointing at the same destination is the
	// ordinary case — "everyone outside the EU goes here" written as several
	// country rules — and the string would otherwise be in the payload once per
	// rule.
	//
	// On the destination **id** rather than on the URL since M36, and the change
	// costs payload to buy correct attribution. Every rule creates a destination
	// row of its own, so two rules with the same URL are two ids and now travel
	// as two entries where they used to collapse into one. Collapsing them was
	// free while nothing distinguished them; it stopped being free the moment a
	// click carries the id of the row that served it, because the merged entry
	// would credit every one of those clicks to whichever rule happened to be
	// first. The extra copies are bounded by domain.MaxRulesPerLink.
	index := make(map[uuid.UUID]int, len(rows))
	snap.Destinations = make([]SnapshotDest, 0, len(rows))
	snap.Rules = make([]SnapshotRule, 0, len(rows))
	for _, row := range rows {
		if row.URL == "" {
			continue
		}
		at, ok := index[row.ID]
		if !ok {
			at = len(snap.Destinations)
			snap.Destinations = append(snap.Destinations, SnapshotDest{
				ID: row.ID, URL: row.URL, Weight: row.Weight,
			})
			index[row.ID] = at
		}
		kind := row.Kind
		if kind == domain.RuleKindMatch {
			// Encoded as absent. See SnapshotRule.KindOf: this is what keeps a
			// link carrying only M34's rules at the payload size it had.
			kind = ""
		}
		snap.Rules = append(snap.Rules, SnapshotRule{Dest: at, Cond: row.Conditions, Kind: kind})
	}
	if len(snap.Rules) == 0 {
		// Everything was dropped. Leave the fields nil rather than empty, so the
		// encoded payload is byte-identical to a link that never had rules.
		snap.Destinations, snap.Rules = nil, nil
	}
}

func (r *Resolver) store(ctx context.Context, k string, snap *Snapshot, now time.Time) {
	ttl := snap.CacheTTL(now, r.opts.TTL, r.opts.NegativeTTL)
	r.mem.set(k, snap, ttl, now)

	if r.redis == nil {
		return
	}
	// The lookup that led here already failed against this server, recently
	// enough that the answer has not changed. Spending a second RedisTimeout on
	// a write nobody will read is what made a cold resolve cost the timeout
	// *twice* rather than once (F9), which is the difference between meeting the
	// 100ms uncached target during a Redis stall and missing it.
	//
	// The in-process tier is populated above regardless, so the request after
	// this one is a memory hit on this replica whatever Redis is doing. Skipping
	// costs only that the *shared* tier stays cold for the window — which it was
	// going to be anyway, since the server is not answering.
	if r.redisUnavailable(now) {
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

// InvalidateDomain drops every cached entry on a domain. Implements
// link.Invalidator.
//
// This is the expensive invalidation and there is no cheap version of it. A
// snapshot carries the domain's bot policy so that a cache hit can answer
// without a second lookup (see fromDatabase), and the price of that is paid
// here: changing the policy changes the answer for every alias underneath, and
// every cached copy of every one of them is now wrong.
//
// Same order as InvalidateAlias, for the same reasons. Memory first so this
// replica is correct immediately — it is the one the operator is about to
// reload. Then Redis, waited on, because an entry left there is authoritative
// and refills the memory tier this call just cleared. Then the broadcast, so
// the other replicas clear their own memory tiers after the shared copy is
// already gone.
func (r *Resolver) InvalidateDomain(ctx context.Context, domainID uuid.UUID) {
	prefix := keyPrefix(domainID)
	r.mem.deletePrefix(prefix)

	if r.redis == nil {
		return
	}
	if err := r.sweepRedis(ctx, prefix); err != nil {
		r.log.Error("failed to invalidate a domain's cached links; some may keep "+
			"applying the previous bot-blocking policy until they expire",
			slog.String("domain_id", domainID.String()),
			slog.Duration("stale_for_up_to", r.opts.TTL),
			slog.Any("error", err))
	}

	r.publish(ctx, invalidation{Kind: kindDomain, Key: prefix})
}

// domainSweepBudget bounds a whole domain invalidation.
//
// Deliberately much larger than InvalidateBudget, and for a reason that is not
// generosity: that budget bounds a single DEL, while this bounds a walk of the
// keyspace whose length is the number of cached aliases. Sharing one number
// would mean either a bound that cannot finish the sweep on a real instance or
// one that lets a single stalled DEL hold an operator's form submission for
// five seconds. They are different operations and they get different bounds.
const domainSweepBudget = 5 * time.Second

// sweepScanCount is the SCAN batch size. Large enough that a hundred thousand
// keys is a few hundred round trips rather than a few thousand, small enough
// that no single call blocks Redis for long — SCAN's cost is per call, and the
// whole point of using it over KEYS is not stalling the server other requests
// are being served from.
const sweepScanCount = 500

// sweepRedis removes every key under a prefix.
//
// SCAN and UNLINK rather than KEYS and DEL. KEYS blocks Redis for the length of
// the whole keyspace, on a server that is at that moment answering redirects;
// UNLINK frees memory on a background thread rather than in the command. The
// cost of SCAN is that it iterates the whole keyspace to find our prefix, which
// is acceptable precisely because this runs when somebody changes a policy and
// never on the redirect path.
//
// Bounded from outside the call, like deleteFromRedis, because the budget is
// for the whole walk and no per-command deadline can express that: the loop
// below issues an unbounded number of SCAN and UNLINK pairs, and each one is
// separately entitled to REDIS_READ_TIMEOUT. Since M45 a stalled command does
// honour the context it is handed (F138), which bounds one command; this bounds
// the walk. An unfinished sweep is reported so the operator learns their change
// may take until TTL to reach every replica, rather than believing it landed
// everywhere.
func (r *Resolver) sweepRedis(ctx context.Context, prefix string) error {
	base, cancel := context.WithTimeout(context.WithoutCancel(ctx), domainSweepBudget)

	done := make(chan error, 1)
	go func() {
		defer cancel()
		done <- r.scanDelete(base, prefix)
	}()

	select {
	case err := <-done:
		return err
	case <-base.Done():
		// The same last look deleteFromRedis takes: the budget can expire in the
		// instant the sweep finishes, and a bare select would pick between them
		// at random.
		select {
		case err := <-done:
			return err
		default:
			return fmt.Errorf("domain invalidation budget of %s spent", domainSweepBudget)
		}
	}
}

func (r *Resolver) scanDelete(ctx context.Context, prefix string) error {
	var cursor uint64
	for {
		keys, next, err := r.redis.Scan(ctx, cursor, prefix+"*", sweepScanCount).Result()
		if err != nil {
			return fmt.Errorf("scan %s*: %w", prefix, err)
		}
		if len(keys) > 0 {
			if err := r.redis.Unlink(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("unlink %d keys under %s: %w", len(keys), prefix, err)
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
		// A spent budget ends the walk rather than starting a batch that cannot
		// finish inside it.
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
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
// given, because it bounds the *loop* and a per-command deadline cannot. Three
// attempts each entitled to RedisTimeout, plus the backoff between them, is
// three times whatever REDIS_READ_TIMEOUT is set to however faithfully each one
// is honoured — which is how the same three attempts measured 9.07s while M23
// was being built, against a test client that had left the timeout at go-redis's
// 3s default.
//
// Until M45 nothing honoured them at all. Measured for M26.6 against a server
// that accepts the connection and then never answers: a client whose ReadTimeout
// was 400ms took 402ms to fail a command carrying a 50ms context. That was
// go-redis handing the socket deadline context.Background() unless
// Options.ContextTimeoutEnabled is set, which internal/platform/redis now sets
// (F138), so an attempt is bounded by its own dctx below. The budget is what
// still bounds three of them.
//
// Bounding the wait from outside the call is what internal/ratelimit does for
// the same failure and the same reason (M24). An attempt still running when the
// budget is spent is cancelled and, since F138, notices; what makes the bound
// hold either way is that the caller has stopped waiting. A delete that lands a
// moment later removes a key that should be gone anyway.
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
