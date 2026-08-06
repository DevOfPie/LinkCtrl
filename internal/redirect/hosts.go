package redirect

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Hostname-to-domain resolution for the redirect path (M40).
//
// **No per-request query, and no replica serving a domain another has just
// unverified.** Those two sentences are the milestone's cross-replica staleness
// gap, and this type is where both are answered: the whole verified set is held
// in memory, and a change to it is broadcast on M23's channel so every replica
// reloads rather than waiting out a TTL.
//
// A whole-set cache rather than per-hostname entries with their own TTLs, which
// is the shape the alias cache has and the wrong shape here. Three reasons:
//
//   - The set is small. Verified custom hostnames are a per-customer artefact
//     numbering in the tens, not the millions that make an LRU necessary.
//   - A miss must not cost a query. Per-hostname caching would mean an unknown
//     Host — which is what a scanner sends, constantly — either queries Postgres
//     or needs negative entries of its own. Holding the whole set makes "not
//     ours" a map lookup that can never miss.
//   - Un-verification has to be *complete*. Dropping one hostname from a set
//     that is authoritative is exact; expiring one entry out of a partial cache
//     leaves the question of what else was stale.

// VerifiedDomain is one hostname the router may serve, as the hot path needs it.
type VerifiedDomain struct {
	ID uuid.UUID
	// Hostname is already lowered, matching config.CanonicalHost's spelling.
	Hostname string
	// RootRedirectURL is where this hostname's own root sends a visitor. Empty
	// means 404, which is the default and the state that says nothing about the
	// instance.
	RootRedirectURL string
	// SSLStatus is what this instance last recorded about the certificate. The
	// app never speaks ACME (decision D3), so this says whether the on-demand
	// ask has been answered for the name, and nothing about the certificate
	// itself — which is Caddy's.
	SSLStatus string
}

// HostCache holds every verified hostname this instance serves.
type HostCache struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu    sync.RWMutex
	hosts map[string]VerifiedDomain

	// ready is false until a load has succeeded. Nothing is served on any custom
	// hostname before then, which is the direction a cache that cannot reach its
	// database has to fail in: a lookup that answered "not verified" would be
	// indistinguishable from the truth, and one that answered "verified" would
	// be serving an alias namespace on the strength of nothing.
	ready atomic.Bool

	// One reload worker at a time, and no arming lost: `running` says a worker
	// goroutine exists, `pending` says the verified set changed and no
	// completed load has observed it yet. A burst of invalidations — a job
	// unverifying several domains in one pass — collapses into the one bit and
	// costs one query, not one per message.
	//
	// Guarded by a mutex rather than held as two independent atomics, and the
	// reason is the worker's exit. Deciding "nothing is pending" and giving up
	// `running` must be one indivisible step: as separate atomics there is a
	// legal schedule — worker reads pending false and leaves its loop, a
	// caller stores pending true, the caller reads running still true and
	// returns trusting the worker to look again, the worker's deferred release
	// fires — whose terminal state is pending armed with nobody left to serve
	// it. On a quiet instance nothing ever re-triggers, so a revoked hostname
	// keeps being served indefinitely. Under the mutex a caller can only ever
	// observe "worker present, its next check will see my arming" or "no
	// worker, I start one"; the stranded third state cannot be reached.
	schedMu sync.Mutex
	running bool
	pending bool

	// load is how Reload obtains the next set. Nil means Postgres, which is
	// the only production value; tests substitute it because what they pin
	// down is the scheduling around Reload — collapse, retry, the worker's
	// exit — and none of that should need a database to be observable.
	load func(context.Context) (map[string]VerifiedDomain, error)

	// OnReload runs after every successful load, including the first.
	//
	// One caller: the link service's id-to-hostname map, which builds short
	// URLs and would otherwise keep printing a renamed domain's old name until
	// the process restarted. It hangs off this rather than off a second
	// subscription because the two are the same event — the set of hostnames
	// changed — and two listeners for one event is how they come to disagree
	// about whether it happened.
	OnReload func()
}

// hostLoadTimeout bounds one reload. Generous next to a redirect's budget
// because it never runs on a request: this is a boot-time and invalidation-time
// query against a table with tens of rows.
const hostLoadTimeout = 5 * time.Second

func NewHostCache(pool *pgxpool.Pool, log *slog.Logger) *HostCache {
	if log == nil {
		log = slog.Default()
	}
	return &HostCache{pool: pool, log: log, hosts: map[string]VerifiedDomain{}}
}

// Lookup answers whether this Host header names a verified custom domain.
//
// This is on the redirect path and it is a read lock plus a map lookup. It never
// queries, never falls back, and never consults Redis.
//
// Keyed through config.HostOnly rather than through a local copy of the same
// operations, and that is not tidiness — it is the same rule this comment always
// stated, applied to the right function. A second *spelling* here would mean a
// Host header that matches one router and not the other, and the direction that
// fails silently is the dangerous one: a name normalized differently stops being
// served while every page goes on saying it is verified.
//
// **It was config.CanonicalHost until F88, and that was the narrower question.**
// CanonicalHost keeps a non-default port because SplitHosts compares two
// configured origins through it, so `Host: go.customer.example:8080` did not
// match a `domains.hostname` that is stored, validated and served bare — it fell
// through to the tree behind this one, which on a single-host deployment is the
// dashboard, the API and the default domain's aliases. HostOnly is the same
// normalization with the port dropped as well, which is the only spelling that
// answers "is this a verified hostname".
//
// The collision that widening could create is worth naming and is not new: a
// verified hostname equal to one of this instance's own is served by this router
// before either of them, at any port spelling rather than only at the configured
// one. Reaching it still means publishing a DNS TXT record under the operator's
// own name, which is the whole of M40's verification, so the precondition is
// unchanged and only the set of Host spellings it covers is.
func (c *HostCache) Lookup(host string) (VerifiedDomain, bool) {
	if c == nil || !c.ready.Load() {
		return VerifiedDomain{}, false
	}
	name := config.HostOnly(host)
	if name == "" {
		return VerifiedDomain{}, false
	}
	c.mu.RLock()
	d, ok := c.hosts[name]
	c.mu.RUnlock()
	return d, ok
}

// Reload replaces the set from Postgres.
//
// Whole-set replacement rather than a merge. A domain that stopped being
// verified is absent from the result, and the only way for its absence to mean
// "stop serving it" is for the result to be the entire truth.
func (c *HostCache) Reload(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hostLoadTimeout)
	defer cancel()

	load := c.load
	if load == nil {
		load = c.loadFromDB
	}
	next, err := load(rctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.hosts = next
	c.mu.Unlock()
	c.ready.Store(true)
	if c.OnReload != nil {
		c.OnReload()
	}
	return nil
}

// loadFromDB is Reload's production loader: the whole verified set, one query.
func (c *HostCache) loadFromDB(ctx context.Context) (map[string]VerifiedDomain, error) {
	rows, err := dbgen.New(c.pool).ListVerifiedDomains(ctx)
	if err != nil {
		return nil, fmt.Errorf("load verified domains: %w", err)
	}
	next := make(map[string]VerifiedDomain, len(rows))
	for _, r := range rows {
		d := VerifiedDomain{ID: r.ID, Hostname: r.Hostname, SSLStatus: r.SslStatus}
		if r.RootRedirectUrl != nil {
			d.RootRedirectURL = *r.RootRedirectUrl
		}
		// Keyed through the same function Lookup asks with, rather than through
		// the stored spelling. The two agree today — hostnames are stored lowered
		// and dot-free — and the point is that they cannot stop agreeing, which
		// is the failure F88 was: two spellings of one name, one of them never
		// reached.
		next[config.HostOnly(r.Hostname)] = d
	}
	return next, nil
}

// Refresh reloads in the background, collapsing a burst into one query.
//
// Used by the subscriber, which must not block its read loop on Postgres: a
// reload that stalled would stop the replica hearing every *other* invalidation
// as well, which is a much larger fault than the one being applied.
func (c *HostCache) Refresh(ctx context.Context) {
	if c == nil {
		return
	}
	c.schedMu.Lock()
	c.pending = true
	if c.running {
		// A worker exists, and under this mutex that is a guarantee, not a
		// hope: the worker only stops running inside the same critical section
		// in which it confirms pending is clear, so the arming above either
		// meets the worker's next check or this branch was never taken.
		c.schedMu.Unlock()
		return
	}
	c.running = true
	c.schedMu.Unlock()
	// The caller's context rides along untouched: Reload detaches and bounds
	// itself per call (WithoutCancel + hostLoadTimeout), so the subscriber's
	// read loop cancelling does not cancel a reload, and no unbounded detached
	// context is ever handed onward.
	go c.reloadWorker(ctx)
}

// reloadWorker drains the pending bit — one load per arming — and retires.
// A named method rather than a closure so a test can run the exit path to
// completion on its own goroutine and prove an armed bit is never abandoned.
func (c *HostCache) reloadWorker(ctx context.Context) {
	for {
		c.schedMu.Lock()
		if !c.pending {
			// The exit: confirm there is no work and give up the worker role
			// in one critical section. This indivisibility is the entire
			// reason the schedule state lives under a mutex.
			c.running = false
			c.schedMu.Unlock()
			return
		}
		c.pending = false
		c.schedMu.Unlock()

		if err := c.Reload(ctx); err != nil {
			// Re-arm before retiring. The arming this load was serving is a
			// fact about the world — the verified set changed — and a failed
			// query has not made it false; consuming the bit here would turn
			// "applied late" into "applied never". Armed-and-waiting means the
			// next trigger from any direction (the subscriber's next message,
			// the operator's next change, the hourly reload's next pass where
			// one is configured) retries. Retiring rather than looping is
			// equally deliberate: a persistent error re-attempted in a tight
			// loop would have every replica hammering a database that is
			// already in trouble.
			c.schedMu.Lock()
			c.pending = true
			c.running = false
			c.schedMu.Unlock()
			c.log.Error("could not reload the verified-hostname cache; this "+
				"replica may keep serving a domain that has been unverified, "+
				"or may not yet serve one that has just been verified; the "+
				"reload stays queued for the next refresh",
				slog.Any("error", err))
			return
		}
	}
}

// MarkTLSActive records locally that the on-demand ask has been answered, so a
// second handshake does not repeat the write. Storage is the caller's.
func (c *HostCache) MarkTLSActive(host string) {
	if c == nil {
		return
	}
	name := config.HostOnly(host)
	c.mu.Lock()
	if d, ok := c.hosts[name]; ok && d.SSLStatus != sslStatusActive {
		d.SSLStatus = sslStatusActive
		c.hosts[name] = d
	}
	c.mu.Unlock()
}

// The three ssl_status values this app writes. 'error' exists in the column's
// CHECK and is never written: it would be a claim about a certificate, and
// certificates are Caddy's (decision D3).
const (
	sslStatusNone    = "none"
	sslStatusPending = "pending"
	sslStatusActive  = "active"
)

// SSLStatusPending is what a freshly verified domain carries: this instance will
// answer Caddy's ask for it, and nothing more is known.
const SSLStatusPending = sslStatusPending

// Size reports how many hostnames are served, for metrics and tests.
func (c *HostCache) Size() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.hosts)
}

// Ready reports whether a load has ever succeeded.
func (c *HostCache) Ready() bool { return c != nil && c.ready.Load() }

// PublishHostInvalidation tells every replica that the verified set has changed.
//
// Exported for the same reason PublishRootInvalidation is: the set lives beside
// the link service, and the Redis connection lives here.
func (r *Resolver) PublishHostInvalidation(ctx context.Context) {
	r.publish(ctx, invalidation{Kind: kindHost})
}

// BroadcastHostInvalidator refreshes this replica's host cache and tells the
// others to refresh theirs.
//
// The same shape as BroadcastRootInvalidator and for the same reason: the link
// service changes a domain's verification without knowing that pub/sub exists.
// Local first, so the replica the operator is about to reload is already right
// if the publish fails.
type BroadcastHostInvalidator struct {
	Local     *HostCache
	Publisher *Resolver
}

func (b BroadcastHostInvalidator) InvalidateHosts(ctx context.Context) {
	if b.Local != nil {
		b.Local.Refresh(ctx)
	}
	if b.Publisher != nil {
		b.Publisher.PublishHostInvalidation(ctx)
	}
}
