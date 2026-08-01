package redirect

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// InvalidationChannel carries cache invalidations between replicas.
//
// Versioned with the cache key, so a mixed-version deployment mid-rolling-update
// cannot have one replica interpret another's message under different rules. A
// replica on the old version simply never hears the new channel, which degrades
// to the TTL staleness that existed before this milestone rather than to a
// misapplied invalidation.
const InvalidationChannel = "lc:inval:" + CacheKeyVersion

// Kinds of invalidation. Short strings because this is on the wire.
const (
	kindAlias = "a"
	kindRoot  = "r"
	// kindDomain clears every alias on one domain, and its Key is a prefix
	// rather than a whole key. A separate kind rather than N alias messages:
	// a domain with a hundred thousand links would otherwise publish a hundred
	// thousand messages to say one thing.
	kindDomain = "d"
)

// invalidation is one published message.
//
// JSON rather than a packed string so that M40's hostname cache can add a field
// without a channel version bump: an older replica decoding a newer message
// ignores what it does not know, which is the behaviour that makes a rolling
// deploy safe. The field names are one letter because every redirect edit
// publishes one of these and the content is not read by humans.
type invalidation struct {
	Kind string `json:"k"`
	Key  string `json:"key,omitempty"`
}

// publish sends an invalidation to every replica.
//
// Best-effort by design, and the asymmetry with deleteFromRedis is deliberate.
// A failed Redis DEL leaves the *old snapshot authoritative* for every replica,
// so it is retried. A failed publish leaves the other replicas holding their own
// in-process copies until TTL — the exact behaviour this instance had before
// pub/sub existed. Degrading to the previous known-good staleness is not worth
// blocking an operator's form submission over.
//
// The publisher receives its own message and applies it again. That is a
// redundant delete of a key it just deleted, and cheap enough that filtering by
// sender id would add a field, a comparison and a way to get it wrong for no
// gain.
//
// It does not wait for the result, and that is load-bearing rather than
// laziness. A stalled Redis — one that accepts the connection and then never
// answers — does not honour the short context this hands its client: measured
// against a black-holed server, waiting for the publish added three seconds to
// an edit. Since the caller has nothing to do with the outcome either way, the
// bound that actually holds is to not wait at all. Failures are logged from the
// goroutine, so a broadcast that never landed is still visible.
func (r *Resolver) publish(ctx context.Context, msg invalidation) {
	if r.redis == nil {
		return
	}
	b, err := json.Marshal(msg)
	if err != nil {
		// Cannot happen for this struct; if it ever does, the invalidation is
		// lost and staleness is bounded by TTL, so it is logged and not fatal.
		r.log.Error("could not encode a cache invalidation", slog.Any("error", err))
		return
	}

	// Detached from the request that triggered the edit, for the same reason
	// the delete is: the operator's HTTP response may already have been written.
	base := context.WithoutCancel(ctx)
	go func() {
		pctx, cancel := context.WithTimeout(base, r.opts.RedisTimeout)
		defer cancel()

		if err := r.redis.Publish(pctx, InvalidationChannel, b).Err(); err != nil {
			r.log.Warn("could not publish a cache invalidation; other replicas will "+
				"serve their cached copy until it expires",
				slog.String("kind", msg.Kind),
				slog.Duration("stale_for_up_to", r.opts.TTL),
				slog.Any("error", err))
		}
	}()
}

// PublishRootInvalidation tells every replica to drop its cached root redirect.
//
// Exported because the root cache lives in the HTTP layer, above this package,
// while the Redis connection lives here. The local drop is the caller's job;
// this is only the broadcast.
func (r *Resolver) PublishRootInvalidation(ctx context.Context) {
	r.publish(ctx, invalidation{Kind: kindRoot})
}

// RootCache is the in-process root-redirect cache, as the subscriber sees it.
//
// An interface declared here rather than an import because the implementation
// is in the HTTP layer, which imports this package. Nil is valid and means this
// process does not serve the link host's root.
type RootCache interface {
	InvalidateRoot()
}

// BroadcastRootInvalidator clears the local root cache and tells every other
// replica to do the same.
//
// It exists because the two halves live in different layers: the cache is an
// HTTP handler and the Redis connection belongs to the resolver. The link
// service holds one of these instead of the handler directly, so a setting
// change reaches every replica without the service knowing pub/sub exists.
//
// Local first, then publish. If the publish fails the operator's own instance
// is still correct, which is the one they are about to reload to check their
// work.
type BroadcastRootInvalidator struct {
	Local     RootCache
	Publisher *Resolver
}

func (b BroadcastRootInvalidator) InvalidateRoot() {
	if b.Local != nil {
		b.Local.InvalidateRoot()
	}
	if b.Publisher != nil {
		b.Publisher.PublishRootInvalidation(context.Background())
	}
}

// Subscriber applies invalidations published by other replicas.
//
// It runs in its own goroutine and never touches the request path: a redirect
// reads the in-process tier, and this only ever deletes from it. That is what
// keeps the cached p99 unaffected by however much invalidation traffic there is.
type Subscriber struct {
	// Redis is the connection to subscribe on. Nil disables the subscriber
	// entirely, which is the cache-disabled and Redis-absent deployment: every
	// replica then falls back to TTL staleness, exactly as before this existed.
	Redis *goredis.Client
	// Resolver owns the alias tier this clears.
	Resolver *Resolver
	// Root is the root-redirect cache. Nil on a single-host deployment, where
	// the link root belongs to the dashboard and there is nothing to cache.
	Root RootCache
	Log  *slog.Logger

	// ReconnectBackoff bounds how fast a subscriber retries a dead connection.
	// Zero uses defaultReconnectBackoff.
	ReconnectBackoff time.Duration
}

const defaultReconnectBackoff = 500 * time.Millisecond

func (s *Subscriber) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Subscriber) backoff() time.Duration {
	if s.ReconnectBackoff > 0 {
		return s.ReconnectBackoff
	}
	return defaultReconnectBackoff
}

// Run subscribes and applies invalidations until the context is cancelled.
//
// The shape of this loop is the milestone's whole risk. go-redis returns the
// read error to the caller and re-establishes the connection underneath, which
// means a dropped subscriber is *observable exactly once* — on the failing
// read — and is silent afterwards. A loop that simply retried would resubscribe
// successfully and carry on serving entries whose invalidations were published
// into the gap, with nothing anywhere reporting a problem. That is stale data
// mistaken for fresh data, which is the failure this design exists to prevent.
//
// So every re-establishment, including the first, is followed by a flush of
// both in-process tiers (decision D20). Redis pub/sub does not replay and the
// process cannot know which keys it missed, so the only sound answer is to
// trust none of them.
func (s *Subscriber) Run(ctx context.Context) {
	if s.Redis == nil {
		return
	}
	log := s.logger()

	ps := s.Redis.Subscribe(ctx, InvalidationChannel)
	defer func() { _ = ps.Close() }()

	// The first establishment flushes too. A subscriber that starts while Redis
	// is down, or that comes up after the process has already served traffic
	// from Postgres, is in exactly the position a reconnecting one is: holding
	// entries whose invalidations it was not there to hear.
	if !s.establish(ctx, ps, log, true) {
		return
	}

	for {
		msg, err := ps.ReceiveMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown, not a failure.
				return
			}
			log.Error("cache invalidation subscriber lost its connection; this "+
				"replica may serve edited links from cache until it reconnects",
				slog.Any("error", err))
			if !s.establish(ctx, ps, log, false) {
				return
			}
			continue
		}
		s.apply(msg.Payload, log)
	}
}

// establish blocks until the subscription is live, then flushes both tiers.
//
// It pings rather than waiting for the next published message. Waiting would
// leave the stale window open until some unrelated edit happened to arrive,
// which on a quiet instance is indefinitely — and "no messages" is precisely
// what a broken subscriber also looks like.
//
// Reports false only when the context ended, which is shutdown.
func (s *Subscriber) establish(ctx context.Context, ps *goredis.PubSub, log *slog.Logger, first bool) bool {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		// Ping forces the reconnect-and-resubscribe that go-redis otherwise
		// performs lazily on the next read, so success here means the
		// subscription is genuinely live rather than merely not-yet-failed.
		if err := ps.Ping(ctx); err == nil {
			s.flush()
			if first {
				log.Info("cache invalidation subscriber ready",
					slog.String("channel", InvalidationChannel))
			} else {
				log.Info("cache invalidation subscriber reconnected; in-process " +
					"caches flushed because invalidations published while it was " +
					"disconnected cannot be replayed")
			}
			return true
		} else if attempt == 0 {
			log.Warn("cache invalidation subscriber cannot reach redis; this "+
				"replica is serving cached entries with TTL staleness only",
				slog.Any("error", err))
		}

		timer := time.NewTimer(s.backoff())
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false
		}
	}
}

// flush drops every in-process cached entry this process holds.
func (s *Subscriber) flush() {
	if s.Resolver != nil {
		s.Resolver.mem.flush()
	}
	if s.Root != nil {
		s.Root.InvalidateRoot()
	}
}

// apply performs one invalidation.
func (s *Subscriber) apply(payload string, log *slog.Logger) {
	var msg invalidation
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		// A message this build cannot read is not a reason to stop listening:
		// during a rolling upgrade the other side may be a newer version.
		log.Debug("ignoring an unreadable cache invalidation", slog.Any("error", err))
		return
	}

	switch msg.Kind {
	case kindAlias:
		if msg.Key == "" || s.Resolver == nil {
			return
		}
		// Only the in-process tier. Whoever published it already deleted the
		// Redis copy, and every replica racing to delete the same key would
		// turn one edit into N round trips for no benefit.
		s.Resolver.mem.delete(msg.Key)
	case kindDomain:
		if msg.Key == "" || s.Resolver == nil {
			return
		}
		// In-process only, for the same reason as an alias — and here the reason
		// is much sharper. The publisher has already swept Redis; N replicas
		// each running their own SCAN of the whole keyspace to delete keys that
		// are already gone would turn one policy change into N keyspace walks on
		// the server that is answering redirects.
		s.Resolver.mem.deletePrefix(msg.Key)
	case kindRoot:
		if s.Root != nil {
			s.Root.InvalidateRoot()
		}
	default:
		// A kind this build does not know, from a newer replica. Ignored
		// rather than treated as a flush: guessing wrong in the safe direction
		// still means clearing a cache on every message a future version sends.
		log.Debug("ignoring an unknown cache invalidation kind", slog.String("kind", msg.Kind))
	}
}
