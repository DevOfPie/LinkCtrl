package redirect

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
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
	// kindHost says the set of verified hostnames has changed (M40). It carries
	// no Key: the receiver reloads the whole set, because the message that
	// matters most is a *removal* and a key naming one hostname could not say
	// which others went with it.
	//
	// This is the message the whole custom-domain cache exists for. A domain
	// unverified on one replica has to stop being served on every other one, and
	// without this the other replicas would go on resolving aliases on a
	// hostname whose owner has lost control of the name.
	kindHost = "h"
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
// laziness. It was built when a stalled Redis — one that accepts the connection
// and then never answers — did not honour the short context this hands its
// client: measured against a black-holed server, waiting for the publish added
// three seconds to an edit. Since M45 that context binds (F138), so waiting
// would now cost RedisTimeout instead of three seconds — but the caller still
// has nothing to do with the outcome either way, so the right amount to wait is
// still none of it. Failures are logged from the goroutine, so a broadcast that
// never landed is still visible.
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
	// Hosts is the verified-hostname set (M40). Nil where no custom domain can
	// be served, and then a kindHost message is a no-op rather than an error.
	Hosts *HostCache
	Log   *slog.Logger

	// ReconnectBackoff bounds how fast a subscriber retries a dead connection.
	// Zero uses defaultReconnectBackoff.
	ReconnectBackoff time.Duration

	// ReadTimeout bounds how long the subscriber will sit in one read before it
	// makes Redis prove the subscription is still delivering. Zero uses
	// defaultReadTimeout. Set from REDIS_SUBSCRIBER_READ_TIMEOUT.
	ReadTimeout time.Duration
}

const (
	defaultReconnectBackoff = 500 * time.Millisecond
	defaultReadTimeout      = 30 * time.Second
)

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

func (s *Subscriber) readTimeout() time.Duration {
	if s.ReadTimeout > 0 {
		return s.ReadTimeout
	}
	return defaultReadTimeout
}

// Run subscribes and applies invalidations until the context is cancelled.
//
// The shape of this loop is the milestone's whole risk, and it has two failure
// modes rather than one. go-redis returns a read *error* to the caller and
// re-establishes the connection underneath, which means a dropped subscriber is
// observable exactly once — on the failing read — and silent afterwards. A loop
// that simply retried would resubscribe successfully and carry on serving
// entries whose invalidations were published into the gap, with nothing
// reporting a problem.
//
// The other mode has no error at all. A Redis that holds the connection open
// and stops answering produces silence, and silence is also what a channel
// nobody has published on looks like. Reading with no deadline never has to
// separate them, and never does: F30 measured `ReceiveMessage` blocked for 40s
// while Redis reported delivering an invalidation to this subscriber. So the
// read is bounded, and a read that expires is not treated as either outcome
// until the connection has been asked a question it must answer — see probe.
//
// Every establishment, including the first, flushes both in-process tiers
// (decision D20). Redis pub/sub does not replay and the process cannot know
// which keys it missed, so the only sound answer is to trust none of them.
func (s *Subscriber) Run(ctx context.Context) {
	if s.Redis == nil {
		return
	}
	log := s.logger()

	// The first establishment flushes too. A subscriber that starts while Redis
	// is down, or that comes up after the process has already served traffic
	// from Postgres, is in exactly the position a reconnecting one is: holding
	// entries whose invalidations it was not there to hear.
	ps := s.establish(ctx, nil, log, true)
	if ps == nil {
		return
	}
	// ps is reassigned on every re-establishment, so this closes whichever
	// subscription is current rather than the one opened above.
	defer func() { _ = ps.Close() }()

	for {
		reply, err := ps.ReceiveTimeout(ctx, s.readTimeout())
		if err == nil {
			// Subscription confirmations and pongs arrive on this connection
			// too and mean nothing here, which is the one service
			// ReceiveMessage performed that this loop has to perform itself.
			if msg, ok := reply.(*goredis.Message); ok {
				s.apply(ctx, msg.Payload, log)
			}
			continue
		}
		if ctx.Err() != nil {
			// Shutdown, not a failure.
			return
		}

		if isTimeout(err) {
			perr := s.probe(ctx, ps, log)
			if perr == nil {
				// The silence was real: nothing has been published, and the
				// subscription is still there to hear it when something is.
				continue
			}
			log.Error("cache invalidation subscriber went silent and redis "+
				"did not answer a probe on the subscription; this replica "+
				"cannot hear invalidations and its in-process caches are being "+
				"dropped rather than served as fresh",
				slog.Duration("silent_for", s.readTimeout()),
				slog.Any("error", perr))
		} else {
			log.Error("cache invalidation subscriber lost its connection; this "+
				"replica may serve edited links from cache until it reconnects",
				slog.Any("error", err))
		}

		next := s.establish(ctx, ps, log, false)
		if next == nil {
			return
		}
		ps = next
	}
}

// probe asks the connection a question that cannot be answered by silence.
//
// `PubSub.Ping` is write-only: it writes the command and never reads a reply,
// so a nil return proves only that bytes entered the socket buffer. Against the
// stall F30 reproduced it returned nil in 0ms on a connection that had already
// stopped delivering. The reply is the evidence, and reading it here is the
// whole difference between "nothing has changed" and "nothing is arriving".
//
// Anything the server sends counts, not only the pong. A subscription
// confirmation is not an answer to this question, but an invalidation is — and
// it is also a message, so it is applied rather than dropped on the floor by a
// health check.
//
// Reports nil when the subscription answered. The error is carried back rather
// than reduced to a bool because it is the only account anyone gets of why a
// replica stopped hearing: a refused dial and a connection that went quiet both
// end here and read very differently in a log.
func (s *Subscriber) probe(ctx context.Context, ps *goredis.PubSub, log *slog.Logger) error {
	if err := ps.Ping(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(s.readTimeout())
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return errSubscriptionSilent
		}
		reply, err := ps.ReceiveTimeout(ctx, left)
		if err != nil {
			return err
		}
		switch msg := reply.(type) {
		case *goredis.Message:
			s.apply(ctx, msg.Payload, log)
			return nil
		case *goredis.Pong:
			return nil
		}
		// A subscription confirmation. Keep waiting for the answer.
	}
}

// errSubscriptionSilent is a ping that was written and never came back. It is
// its own error because it is not a network failure — the socket is fine, and
// that is precisely the problem.
var errSubscriptionSilent = errors.New(
	"redis accepted a ping on the subscription and did not answer it")

// establish replaces the subscription with one that has answered, then flushes.
//
// It probes rather than waiting for the next published message. Waiting would
// leave the stale window open until some unrelated edit happened to arrive,
// which on a quiet instance is indefinitely — and "no messages" is precisely
// what a broken subscriber also looks like.
//
// The old subscription is closed rather than reused, and that is not tidiness.
// go-redis keeps a connection whose read merely timed out — correctly, since an
// idle channel is not a broken one — so nothing underneath this will discard
// the socket that stopped answering. Only Close does.
//
// Reports nil only when the context ended, which is shutdown.
func (s *Subscriber) establish(ctx context.Context, old *goredis.PubSub, log *slog.Logger, first bool) *goredis.PubSub {
	if old != nil {
		_ = old.Close()
		// Distrust what is held now, rather than at reconnect. The subscriber
		// has already stopped hearing invalidations, so every entry it holds is
		// one it cannot vouch for, and against a Redis that never comes back
		// "flush when we reconnect" is a flush that never happens. That is the
		// stale window F30 measured at up to REDIRECT_TTL. Flushing here bounds
		// it to the time it took to notice instead.
		s.flush(ctx)
	}

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		ps := s.Redis.Subscribe(ctx, InvalidationChannel)
		err := s.probe(ctx, ps, log)
		if err == nil {
			s.flush(ctx)
			if first {
				log.Info("cache invalidation subscriber ready",
					slog.String("channel", InvalidationChannel))
			} else {
				log.Info("cache invalidation subscriber reconnected; in-process " +
					"caches flushed because invalidations published while it was " +
					"disconnected cannot be replayed")
			}
			return ps
		}
		_ = ps.Close()
		if attempt == 0 {
			log.Warn("cache invalidation subscriber cannot reach redis; this "+
				"replica is serving cached entries with TTL staleness only",
				slog.Any("error", err))
		}

		timer := time.NewTimer(s.backoff())
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
	}
}

// isTimeout separates an expired read from a connection that failed.
//
// They are the same `error` to a caller and opposite facts: one is a deadline
// this code chose, the other is the connection going away. go-redis draws the
// same line the same way when it decides whether to discard a connection.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// flush drops every in-process cached entry this process holds.
//
// The hostname set is *reloaded* rather than emptied, and the difference is the
// direction each failure points. Emptying it would take every custom domain out
// of service until the reload landed — an availability outage caused by a Redis
// hiccup. Reloading replaces it with the truth, and the truth is what an
// unverification looks like: the row is gone from the query. Until the reload
// lands the replica serves what it last knew, bounded by however long the
// reconnect took to notice, which is the same bound the alias tier carries.
func (s *Subscriber) flush(ctx context.Context) {
	if s.Resolver != nil {
		s.Resolver.mem.flush()
	}
	if s.Root != nil {
		s.Root.InvalidateRoot()
	}
	s.Hosts.Refresh(ctx)
}

// apply performs one invalidation.
func (s *Subscriber) apply(ctx context.Context, payload string, log *slog.Logger) {
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
	case kindHost:
		// A reload rather than a delete, and it is asynchronous (see
		// HostCache.Refresh) so this loop keeps reading. The set is small and the
		// message is rare: a verification, an un-verification, a rename, a
		// removal. What it must never do is block the subscriber, because a
		// replica that stops reading stops hearing alias invalidations too.
		s.Hosts.Refresh(ctx)
	default:
		// A kind this build does not know, from a newer replica. Ignored
		// rather than treated as a flush: guessing wrong in the safe direction
		// still means clearing a cache on every message a future version sends.
		log.Debug("ignoring an unknown cache invalidation kind", slog.String("kind", msg.Kind))
	}
}
