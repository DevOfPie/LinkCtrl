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

	// reload collapses concurrent reload requests. A burst of invalidations —
	// a job unverifying several domains in one pass — must cost one query, not
	// one per message.
	reloading atomic.Bool
	// pending records that a reload arrived while one was already running, so
	// the running one repeats rather than the change being lost.
	pending atomic.Bool

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
// Keyed through config.CanonicalHost rather than through a local copy of the
// same three operations, and that is not tidiness. The split-host router matches
// its own two hostnames with exactly that function; a second spelling here would
// mean a Host header that matches one router and not the other, and the
// direction that fails silently is the dangerous one — a name normalized
// differently stops being served while every page goes on saying it is
// verified.
func (c *HostCache) Lookup(host string) (VerifiedDomain, bool) {
	if c == nil || !c.ready.Load() {
		return VerifiedDomain{}, false
	}
	name := config.CanonicalHost(host)
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

	rows, err := dbgen.New(c.pool).ListVerifiedDomains(rctx)
	if err != nil {
		return fmt.Errorf("load verified domains: %w", err)
	}
	next := make(map[string]VerifiedDomain, len(rows))
	for _, r := range rows {
		d := VerifiedDomain{ID: r.ID, Hostname: r.Hostname, SSLStatus: r.SslStatus}
		if r.RootRedirectUrl != nil {
			d.RootRedirectURL = *r.RootRedirectUrl
		}
		next[r.Hostname] = d
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

// Refresh reloads in the background, collapsing a burst into one query.
//
// Used by the subscriber, which must not block its read loop on Postgres: a
// reload that stalled would stop the replica hearing every *other* invalidation
// as well, which is a much larger fault than the one being applied.
func (c *HostCache) Refresh(ctx context.Context) {
	if c == nil {
		return
	}
	c.pending.Store(true)
	if !c.reloading.CompareAndSwap(false, true) {
		// A reload is already running and will see the pending flag.
		return
	}
	base := context.WithoutCancel(ctx)
	go func() {
		defer c.reloading.Store(false)
		for c.pending.CompareAndSwap(true, false) {
			if err := c.Reload(base); err != nil {
				c.log.Error("could not reload the verified-hostname cache; this "+
					"replica may keep serving a domain that has been unverified, "+
					"or may not yet serve one that has just been verified",
					slog.Any("error", err))
				return
			}
		}
	}()
}

// MarkTLSActive records locally that the on-demand ask has been answered, so a
// second handshake does not repeat the write. Storage is the caller's.
func (c *HostCache) MarkTLSActive(host string) {
	if c == nil {
		return
	}
	name := config.CanonicalHost(host)
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
