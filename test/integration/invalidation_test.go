//go:build integration

package integration

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// redisProxy is a TCP proxy in front of Redis whose connections a test can cut.
//
// A real severed connection rather than a fake client, because the behaviour
// under test is what go-redis does when a read fails mid-subscription — which
// is precisely the thing a fake would be asserting an assumption about. Cutting
// at the socket also reproduces the shape of the real failure: the process
// keeps running, Redis keeps running, and the connection between them stops.
type redisProxy struct {
	t        *testing.T
	ln       net.Listener
	upstream string

	mu        sync.Mutex
	conns     []net.Conn
	reject    bool
	blackhole bool
	// flow is closed while bytes are being carried and open while they are not,
	// so a relay blocks on a receive rather than spinning on a flag.
	flow chan struct{}
}

func newRedisProxy(t *testing.T) *redisProxy {
	t.Helper()

	url := os.Getenv("LINKCTRL_REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/0"
	}
	opt, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatalf("LINKCTRL_REDIS_URL is not a valid Redis URL: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	flowing := make(chan struct{})
	close(flowing)
	p := &redisProxy{t: t, ln: ln, upstream: opt.Addr, flow: flowing}
	t.Cleanup(func() {
		// Order matters: a relay parked in the stall would otherwise still be
		// holding a goroutine when the test ends.
		p.stalled(false)
		_ = ln.Close()
		p.cut()
	})

	go p.serve()
	return p
}

func (p *redisProxy) serve() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		rejecting, swallowing := p.reject, p.blackhole
		p.mu.Unlock()
		if rejecting {
			_ = client.Close()
			continue
		}
		if swallowing {
			// Accepted and then ignored, which is what a stalled Redis looks
			// like from the client side — and unlike a refused connection, it
			// is the shape that can actually hang a caller.
			p.mu.Lock()
			p.conns = append(p.conns, client)
			p.mu.Unlock()
			continue
		}

		server, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()

		go p.relay(server, client)
		go p.relay(client, server)
	}
}

// relay copies one direction, parking whenever the proxy is stalled.
//
// io.Copy would be enough for the other modes; this exists so a connection can
// be held open with its bytes going nowhere, which is a different failure from
// a connection that was never answered at all.
func (p *redisProxy) relay(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			p.awaitFlow()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = dst.Close()
}

func (p *redisProxy) awaitFlow() {
	p.mu.Lock()
	ch := p.flow
	p.mu.Unlock()
	<-ch
}

// addr is what a client should connect to.
func (p *redisProxy) addr() string { return p.ln.Addr().String() }

// cut severs every open connection. New ones are accepted unless refuse is on.
func (p *redisProxy) cut() {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// refuse makes the proxy drop new connections too, so a test can hold the
// subscriber in its disconnected state for as long as it needs to.
func (p *redisProxy) refuse(on bool) {
	p.mu.Lock()
	p.reject = on
	p.mu.Unlock()
}

// blackholed accepts connections and then never answers them. A refused
// connection fails immediately; this is the failure that can hang a caller, and
// so the only one that tests a timeout.
func (p *redisProxy) blackholed(on bool) {
	p.mu.Lock()
	p.blackhole = on
	p.mu.Unlock()
}

// stalled keeps established connections open and stops carrying their bytes.
//
// The second shape of stall, and the one blackholed cannot produce: the
// handshake already succeeded, the pool holds a connection it believes is
// healthy, and the command written down it is simply never answered. A fix
// tuned to a server that never speaks at all is not shown to bound this one.
func (p *redisProxy) stalled(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.flow:
		// Currently flowing.
		if on {
			p.flow = make(chan struct{})
		}
	default:
		// Currently stalled.
		if !on {
			close(p.flow)
		}
	}
}

// client is a plain go-redis client on the proxy, carrying go-redis's own
// defaults.
//
// Those defaults are not a deployment's, and the difference is worth naming
// here because it is what deferred finding F2 turned on: ReadTimeout defaults
// to 3s, while internal/platform/redis.Open sets it from REDIS_READ_TIMEOUT,
// which ships at 50ms. A stall measured through this client therefore costs
// sixty times what the same stall costs an operator. Use clientAsDeployed for
// anything asserting how long a failure takes.
func (p *redisProxy) client(t *testing.T) *goredis.Client {
	t.Helper()
	c := goredis.NewClient(&goredis.Options{Addr: p.addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// clientAsDeployed builds the client internal/platform/redis.Open builds, with
// the timeouts named rather than inherited, so a test asserting a bound says
// which configuration it is asserting it for.
//
// ContextTimeoutEnabled is part of that and not an optimisation: without it
// go-redis hands the socket deadline context.Background() and every deadline a
// caller passes is inert, which is the shipped behaviour this file exists to
// measure. F138 is the row; a helper that omitted it would assert bounds for a
// client no deployment runs, which is exactly how F2's nine seconds came to be
// recorded against a deployment that would have seen 215ms.
func (p *redisProxy) clientAsDeployed(t *testing.T, dial, read time.Duration) *goredis.Client {
	t.Helper()
	c := goredis.NewClient(&goredis.Options{
		Addr:                  p.addr(),
		DialTimeout:           dial,
		ReadTimeout:           read,
		WriteTimeout:          read,
		PoolSize:              50,
		MinIdleConns:          2,
		ContextTimeoutEnabled: true,
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// waitFor polls until cond holds, so a test asserts on an outcome rather than
// on a sleep long enough to hide a race.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// The limitation this milestone discharges, stated as a test: an edit on one
// replica has to reach the in-process cache of another.
//
// Before pub/sub, the second resolver kept serving its own copy until the
// entry's TTL expired — a day, by default — while the dashboard showed the edit
// as applied.
func TestInvalidationReachesAnotherReplica(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	rdb := newRedisClient(t)
	ctx := t.Context()

	dom, err := dbgen.New(f.pool).ResolveDefaultDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alias := createLink(t, f, "replica1", "https://example.com/before")

	opts := redirect.Options{TTL: time.Hour, NegativeTTL: time.Minute}
	// Two resolvers on one Redis is what two replicas are.
	editor := redirect.NewResolver(f.pool, rdb, opts)
	other := redirect.NewResolver(f.pool, rdb, opts)

	sub := &redirect.Subscriber{Redis: rdb, Resolver: other, ReconnectBackoff: 10 * time.Millisecond}
	subCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go sub.Run(subCtx)

	// Warm the other replica's in-process tier.
	if _, err := other.Resolve(ctx, dom.ID, alias); err != nil {
		t.Fatal(err)
	}
	if _, ok := other.ResolveCached(dom.ID, alias); !ok {
		t.Fatal("the second replica did not cache the alias in process")
	}

	// The edit happens on the first replica.
	editor.InvalidateAlias(ctx, dom.ID, alias)

	waitFor(t, 10*time.Second, "the other replica to drop its cached copy", func() bool {
		_, ok := other.ResolveCached(dom.ID, alias)
		return !ok
	})
}

// The root redirect is a separate cache with its own invalidation path, and it
// is the setting that sends every stray visitor somewhere — a stale copy on one
// replica points a share of them at the previous destination.
func TestRootInvalidationReachesAnotherReplica(t *testing.T) {
	rdb := newRedisClient(t)

	publisher := redirect.NewResolver(nil, rdb, redirect.Options{TTL: time.Hour})
	local := &countingRoot{}

	sub := &redirect.Subscriber{
		Redis: rdb, Root: local, ReconnectBackoff: 10 * time.Millisecond,
	}
	subCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go sub.Run(subCtx)

	// The subscriber flushes once when it first establishes, so wait that out
	// before counting — otherwise the assertion below could pass on the startup
	// flush rather than on the published message.
	waitFor(t, 10*time.Second, "the subscriber to establish", func() bool {
		return local.count() >= 1
	})
	before := local.count()

	publisher.PublishRootInvalidation(context.Background())

	waitFor(t, 10*time.Second, "the root invalidation to arrive", func() bool {
		return local.count() > before
	})
}

// Applying an invalidation must not publish one. The wiring hazard is real:
// the link service holds a wrapper that clears locally *and* broadcasts, and
// handing that same wrapper to the subscriber would make every replica answer
// every other replica's message, forever, off one edit.
func TestApplyingARootInvalidationDoesNotRepublishIt(t *testing.T) {
	rdb := newRedisClient(t)

	publisher := redirect.NewResolver(nil, rdb, redirect.Options{TTL: time.Hour})
	local := &countingRoot{}

	sub := &redirect.Subscriber{
		Redis: rdb, Root: local, ReconnectBackoff: 10 * time.Millisecond,
	}
	subCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go sub.Run(subCtx)

	waitFor(t, 10*time.Second, "the subscriber to establish", func() bool {
		return local.count() >= 1
	})
	before := local.count()

	publisher.PublishRootInvalidation(context.Background())
	waitFor(t, 10*time.Second, "the invalidation to arrive", func() bool {
		return local.count() > before
	})

	// One published message must produce one application, and then stop. A
	// republishing subscriber would keep climbing for as long as it is watched.
	settled := local.count()
	time.Sleep(500 * time.Millisecond)
	if grew := local.count() - settled; grew > 0 {
		t.Errorf("the count rose by %d more after the message was applied; the "+
			"subscriber is republishing what it receives", grew)
	}
}

type countingRoot struct {
	mu sync.Mutex
	n  int
}

func (c *countingRoot) InvalidateRoot() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *countingRoot) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// D20, and the reason this milestone exists in the shape it does.
//
// Pub/sub does not replay. A subscriber that was disconnected when an
// invalidation was published never learns of it and cannot know which key it
// named, so on reconnect it must distrust everything it holds. The assertion
// that matters is that the pre-drop entry is *gone*, not merely expiring later:
// a subscriber that resubscribed and carried on would leave it there, serving a
// destination the owner had already changed, with nothing reporting a problem.
func TestReconnectingSubscriberFlushesWhatItCouldNotHear(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	ctx := t.Context()

	dom, err := dbgen.New(f.pool).ResolveDefaultDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alias := createLink(t, f, "recon001", "https://example.com/before")

	proxy := newRedisProxy(t)
	viaProxy := proxy.client(t)

	// A long TTL, so anything still cached at the end of this test is cached
	// because it was never flushed rather than because it had not expired.
	replica := redirect.NewResolver(f.pool, viaProxy, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	root := &countingRoot{}
	sub := &redirect.Subscriber{
		Redis: viaProxy, Resolver: replica, Root: root,
		ReconnectBackoff: 10 * time.Millisecond,
	}
	subCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go sub.Run(subCtx)

	waitFor(t, 5*time.Second, "the subscriber to establish", func() bool {
		return root.count() >= 1
	})
	establishFlushes := root.count()

	// Warm the in-process tier, then cut the connection underneath it.
	if _, err := replica.Resolve(ctx, dom.ID, alias); err != nil {
		t.Fatal(err)
	}
	if _, ok := replica.ResolveCached(dom.ID, alias); !ok {
		t.Fatal("the replica did not cache the alias in process")
	}

	proxy.cut()

	// Whatever was published during the gap is unrecoverable, which is the
	// point: nothing here republishes it after the reconnect.
	waitFor(t, 10*time.Second, "the subscriber to reconnect and flush", func() bool {
		return root.count() > establishFlushes
	})

	if _, ok := replica.ResolveCached(dom.ID, alias); ok {
		t.Error("an entry cached before the connection dropped survived the " +
			"reconnect; invalidations published during the gap cannot be " +
			"replayed, so the only sound answer is to trust none of them (D20)")
	}
}

// The shape F30 found, and the one the shipped subscriber could not see.
//
// A cut connection fails a read, so it is observable exactly once and then
// handled. A *stalled* one — both sockets open, bytes going nowhere — is
// silence, and silence is also what an idle channel looks like. The shipped
// loop read with no deadline at all and so never had to tell them apart:
// `ReceiveMessage` blocked indefinitely, `establish` was never reached,
// `flush` never ran, and the replica served pre-edit destinations for the rest
// of the entry's TTL with no error, no log line and no metric.
//
// Both halves below matter, and the first is the one a careless fix breaks.
// Silence on a healthy connection has to cost nothing: a subscriber that
// flushed every time a quiet period elapsed would answer this finding by
// throwing the in-process tier away on every quiet instance, which is most of
// them. Silence on a stalled connection has to end with the replica no longer
// serving what it can no longer vouch for.
//
// m23.md allows either outcome for that second half — reconnect inside the
// bound, or stop serving the stale entry. While the stall is held no
// reconnection is possible, because the proxy stalls the replacement connection
// too, so the flush is the only observable that can satisfy it. The two are the
// same assertion anyway: reconnecting flushes (D20).
func TestAStalledSubscriberStopsTrustingWhatItCannotHear(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	ctx := t.Context()

	dom, err := dbgen.New(f.pool).ResolveDefaultDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alias := createLink(t, f, "stall001", "https://example.com/before")

	proxy := newRedisProxy(t)
	viaProxy := proxy.client(t)

	// Milliseconds where the shipped default is 30s, and that is the only
	// reason: detection costs at most two of these, so the whole test runs in
	// about a second rather than a minute.
	const bound = 250 * time.Millisecond

	// An hour of TTL, so an entry missing at the end of this is missing because
	// something dropped it rather than because it expired.
	replica := redirect.NewResolver(f.pool, viaProxy, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute, RedisTimeout: 50 * time.Millisecond,
	})
	root := &countingRoot{}
	logs := &syncBuffer{}
	sub := &redirect.Subscriber{
		Redis: viaProxy, Resolver: replica, Root: root,
		ReconnectBackoff: 10 * time.Millisecond,
		ReadTimeout:      bound,
		Log:              slog.New(slog.NewTextHandler(logs, nil)),
	}
	subCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go sub.Run(subCtx)

	waitFor(t, 5*time.Second, "the subscriber to establish", func() bool {
		return root.count() >= 1
	})
	established := root.count()

	// Warm the in-process tier while Redis is still answering.
	if _, err := replica.Resolve(ctx, dom.ID, alias); err != nil {
		t.Fatal(err)
	}
	if _, ok := replica.ResolveCached(dom.ID, alias); !ok {
		t.Fatal("the replica did not cache the alias in process")
	}

	// Several bounds of a quiet but healthy connection. Nothing is published,
	// so every read times out, and the subscriber must conclude nothing from
	// that.
	time.Sleep(4 * bound)
	if grew := root.count() - established; grew > 0 {
		t.Fatalf("the subscriber flushed %d times over %s of a quiet but healthy "+
			"connection. An idle channel is not a broken one, and a fix that "+
			"cannot tell them apart trades this finding for an in-process tier "+
			"that empties itself on every instance nobody is editing links on",
			grew, 4*bound)
	}
	if _, ok := replica.ResolveCached(dom.ID, alias); !ok {
		t.Fatal("the cached alias was dropped while Redis was answering normally")
	}

	// The stall. Both sockets stay open and stop carrying bytes, so from the
	// subscriber's side this is byte-for-byte the same as the quiet above —
	// which is the finding, stated as a test.
	proxy.stalled(true)

	waitFor(t, 10*time.Second,
		"the stalled replica to stop serving an entry it can no longer vouch for",
		func() bool {
			_, ok := replica.ResolveCached(dom.ID, alias)
			return !ok
		})

	// A silent recovery is still silence. The operator has to be able to find
	// out that this replica spent time unable to hear invalidations, which is
	// the half a cut connection already had and a stall did not.
	if !strings.Contains(logs.String(), "did not answer") {
		t.Errorf("nothing was logged when the subscription stopped being "+
			"delivered; a stall that is handled invisibly is still a replica "+
			"nobody can tell was stale. Log was:\n%s", logs.String())
	}
}

// A subscriber that cannot reach Redis must degrade to TTL staleness and keep
// the redirect path working — never fail a redirect, and never sit silently in
// a state that looks the same as "nothing has changed".
func TestRedirectsSurviveRedisBeingDown(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	ctx := t.Context()

	dom, err := dbgen.New(f.pool).ResolveDefaultDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alias := createLink(t, f, "redisdn1", "https://example.com/down")

	proxy := newRedisProxy(t)
	viaProxy := proxy.client(t)

	replica := redirect.NewResolver(f.pool, viaProxy, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute, RedisTimeout: 50 * time.Millisecond,
	})
	sub := &redirect.Subscriber{
		Redis: viaProxy, Resolver: replica, ReconnectBackoff: 10 * time.Millisecond,
	}
	subCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go sub.Run(subCtx)

	// Redis goes away entirely: existing connections cut, new ones refused.
	proxy.refuse(true)
	proxy.cut()

	// Resolution still works, from Postgres.
	res, err := replica.Resolve(ctx, dom.ID, alias)
	if err != nil {
		t.Fatalf("a redirect failed while Redis was unreachable: %v", err)
	}
	if res.Snapshot == nil || res.Snapshot.URL != "https://example.com/down" {
		t.Fatalf("resolve returned %+v with Redis down", res.Snapshot)
	}
	if res.Source == redirect.SourceRedis {
		t.Errorf("source = %s with Redis unreachable", res.Source)
	}

	// Invalidating must not error or block, even though the publish cannot
	// land. It clears this process; the others fall back to TTL.
	assertInvalidateDoesNotHang(t, "with Redis refusing connections", func() {
		replica.InvalidateAlias(ctx, dom.ID, alias)
	})
	if _, ok := replica.ResolveCached(dom.ID, alias); ok {
		t.Error("the local tier was not cleared while Redis was down")
	}

	// The harder half, and the one a refused connection cannot test. A stalled
	// Redis accepts the connection and never answers, so every call waits out
	// whatever timeout it was given — which is why the publish carries
	// RedisTimeout rather than inheriting an unbounded context. Without that
	// bound this call sits on an operator's form submission until the default
	// client timeout, and the edit appears to hang on an optional dependency.
	proxy.refuse(false)
	proxy.blackholed(true)
	proxy.cut()

	if _, err := replica.Resolve(ctx, dom.ID, alias); err != nil {
		t.Fatalf("a redirect failed while Redis was stalled: %v", err)
	}
	// A generous ceiling on purpose, and it stays generous now that F2 is
	// closed. This resolver is built without an InvalidateBudget and on a
	// client carrying go-redis's 3s read timeout, so what it asserts is that
	// the path terminates under defaults nobody tuned. The bound itself is
	// asserted by TestAnEditIsBoundedWhenRedisStalls, which names the
	// configuration it is asserting it for; the publish half is pinned down by
	// TestPublishingAnInvalidationDoesNotWaitOnAStalledRedis.
	assertInvalidateDoesNotHang(t, "with Redis stalled", func() {
		replica.InvalidateAlias(ctx, dom.ID, alias)
	})

	// And it recovers on its own once Redis comes back.
	proxy.blackholed(false)
	proxy.refuse(false)
	waitFor(t, 15*time.Second, "the subscriber to recover after Redis returned", func() bool {
		return viaProxy.Ping(context.Background()).Err() == nil
	})
}

// The bound M26.6 exists to establish: an edit made while Redis has stopped
// answering returns inside one total budget, instead of inside one budget per
// retry — and the edit still lands.
//
// The read timeout below is 400ms rather than the shipped 50ms, and that choice
// is what makes the test discriminating. The defect was multiplication: three
// attempts at RedisTimeout each, so an operator's worst case was three times
// whatever they had set REDIS_READ_TIMEOUT to. At the shipped 50ms the old loop
// and the new one both finish near 215ms and no assertion could tell them
// apart; at 400ms the old loop needs at least 1.26s while the budget here is
// 250ms. An operator running Redis across a WAN is exactly who sets 400ms, and
// F2's nine seconds were the same multiplication over a 3s read timeout.
//
// Both shapes of stall are covered, because a fix tuned to one is not shown to
// bound the other: a server that never answers at all, and a server that
// answered the handshake and then stopped carrying bytes. Not covered: a dial
// whose packets are dropped outright, which needs a network this test cannot
// arrange. That shape is bounded by the per-attempt deadline instead, which a
// dial has always honoured and which since M45 a stalled read honours too
// (F138) — the budget below is still what bounds three attempts of it.
func TestAnEditIsBoundedWhenRedisStalls(t *testing.T) {
	const (
		readTimeout = 400 * time.Millisecond
		budget      = 250 * time.Millisecond
		// The budget plus room for the edit's own transaction and for
		// scheduling. The old loop needed 3 x readTimeout + 60ms of backoff, so
		// this sits below that by more than a factor of two and above the
		// budget by more than a factor of two.
		bound = 600 * time.Millisecond
	)

	shapes := []struct {
		name  string
		stall func(*redisProxy)
	}{
		{"never answers", func(p *redisProxy) { p.blackholed(true); p.cut() }},
		{"answered, then stopped mid-command", func(p *redisProxy) { p.stalled(true) }},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			pool := newDB(t)
			ctx := t.Context()

			authSvc := auth.NewService(pool, auth.ServiceConfig{
				Params: fastParams,
				TTL: auth.SessionTTL{
					Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour,
				},
			})
			owner, err := authSvc.Register(ctx, auth.RegisterInput{
				Email:    "owner@example.com",
				Name:     "Owner",
				Password: "a-sufficiently-long-password",
			})
			if err != nil {
				t.Fatalf("register owner: %v", err)
			}

			proxy := newRedisProxy(t)
			viaProxy := proxy.clientAsDeployed(t, time.Second, readTimeout)

			logs := &syncBuffer{}
			resolver := redirect.NewResolver(pool, viaProxy, redirect.Options{
				TTL:              time.Hour,
				NegativeTTL:      time.Minute,
				RedisTimeout:     readTimeout,
				InvalidateBudget: budget,
				Logger:           slog.New(slog.NewTextHandler(logs, nil)),
			})
			links := link.NewService(pool, link.Config{
				Policy:  link.DefaultDestinationPolicy(),
				BaseURL: "http://lnk.test",
				Cache:   resolver,
			})

			created, err := links.Create(ctx, owner, link.CreateInput{
				URL: "https://example.com/before", Alias: "stalled1",
			})
			if err != nil {
				t.Fatalf("create the link to edit: %v", err)
			}

			dom, err := dbgen.New(pool).ResolveDefaultDomain(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Warm both tiers, so the edit has something to invalidate and the
			// second shape has an established connection to stall.
			if _, err := resolver.Resolve(ctx, dom.ID, created.Alias); err != nil {
				t.Fatalf("warm the cache: %v", err)
			}
			if _, ok := resolver.ResolveCached(dom.ID, created.Alias); !ok {
				t.Fatal("the resolver did not cache the alias in process")
			}

			shape.stall(proxy)

			after := "https://example.com/after"
			start := time.Now()
			updated, err := links.Update(ctx, owner, created.ID,
				link.UpdateInput{URL: &after})
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("the edit failed because Redis was stalled; the cache is "+
					"optional and correctness must never depend on it: %v", err)
			}
			if updated.URL != after {
				t.Errorf("the edit returned %q, want %q", updated.URL, after)
			}
			if elapsed > bound {
				t.Errorf("the edit took %s against a stalled Redis, want under %s. "+
					"The invalidation budget is %s and it is meant to bound the "+
					"whole retry loop; a per-attempt budget would spend %s three "+
					"times over", elapsed, bound, budget, readTimeout)
			}

			// The edit is durable regardless of the cache, which is the half
			// that must not be traded away for the bound.
			var stored string
			if err := pool.QueryRow(ctx,
				`SELECT primary_url FROM links WHERE id = $1`, created.ID,
			).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != after {
				t.Errorf("the stored destination is %q, want %q: the edit must "+
					"survive a cache that never answered", stored, after)
			}

			// A bounded failure is still a failure to invalidate, and the
			// operator has to be able to find that out.
			if _, ok := resolver.ResolveCached(dom.ID, created.Alias); ok {
				t.Error("the in-process tier still holds the alias after an edit")
			}
			if !strings.Contains(logs.String(), "failed to invalidate cache entry") {
				t.Errorf("nothing logged that the invalidation did not happen; a "+
					"bound that hides the failure is worse than the delay it "+
					"replaced. Log was:\n%s", logs.String())
			}
		})
	}
}

// syncBuffer collects log output from the goroutines the invalidation path
// spawns, which is why it is not a bare bytes.Buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// assertInvalidateDoesNotHang fails if an edit is still blocked after a budget
// far larger than any timeout the invalidation path is allowed to carry.
//
// Three seconds rather than something tighter: the delete path retries three
// times with its own timeout each, so the honest bound is "well under a
// person's patience", not a precise number this would have to track.
func assertInvalidateDoesNotHang(t *testing.T, when string, invalidate func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		invalidate()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("invalidating never returned %s; an edit must not hang "+
			"indefinitely on an optional dependency", when)
	}
}

// The publish half, on its own, which is the part this milestone owns.
//
// The client here is the bare one, with go-redis's own three-second defaults and
// no ContextTimeoutEnabled, and that is the point rather than an oversight: it
// is the worst client this code could be handed, and the assertion is that the
// publish does not wait on it whatever it is. Measured at three seconds of added
// edit latency before the publish was made fire-and-forget. The delete beside it
// is slower still and predates this milestone; see deferred finding F2. This
// asserts the half that was fixed here rather than the total, so it stays
// meaningful when F2 is eventually addressed.
func TestPublishingAnInvalidationDoesNotWaitOnAStalledRedis(t *testing.T) {
	proxy := newRedisProxy(t)
	proxy.blackholed(true)

	publisher := redirect.NewResolver(nil, proxy.client(t), redirect.Options{
		TTL: time.Hour, RedisTimeout: 50 * time.Millisecond,
	})

	done := make(chan struct{})
	go func() {
		publisher.PublishRootInvalidation(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing a cache invalidation blocked on a stalled Redis; " +
			"the caller has no use for the result, so it must not wait for one")
	}
}

// A nil Redis client is the cache-disabled deployment. The subscriber must
// return immediately rather than spin, and nothing else may change.
func TestSubscriberWithoutRedisIsANoOp(t *testing.T) {
	sub := &redirect.Subscriber{Redis: nil}

	done := make(chan struct{})
	go func() { sub.Run(context.Background()); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscriber.Run did not return with no Redis client; with the " +
			"cache disabled there is nothing to subscribe to")
	}
}

// The staleness gap M40 closes, stated as a test: a hostname unverified on one
// replica has to stop being served by another.
//
// This is the one the plan review found missing in both rival orderings. Without
// it, a workspace that loses control of its DNS keeps having links served on the
// name by every replica that did not handle the write — for as long as the
// process runs, because a whole-set cache has no TTL to expire behind.
//
// The reload counter is not decoration. A subscriber flushes once when it first
// establishes, and that flush reloads the host set; without waiting it out, the
// assertion below could be satisfied by the *startup* reload landing after the
// update rather than by the published message being applied at all — which is
// what happened when this test was first sabotaged, and it passed.
func TestHostInvalidationReachesAnotherReplica(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	rdb := newRedisClient(t)
	ctx := t.Context()

	// A verified hostname, written directly: what is under test is the
	// invalidation path, and going through the service would additionally
	// require a DNS stub for no gain here.
	id := uuid.Must(uuid.NewV7())
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO domains (id, hostname, verified_at, verification_token, ssl_status)
		VALUES ($1, 'replica.custom.test', now(), 'tok', 'pending')`, id); err != nil {
		t.Fatal(err)
	}

	publisher := redirect.NewResolver(f.pool, rdb, redirect.Options{TTL: time.Hour})
	other := redirect.NewHostCache(f.pool, nil)
	var reloads atomic.Int64
	other.OnReload = func() { reloads.Add(1) }
	if err := other.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := other.Lookup("replica.custom.test"); !ok {
		t.Fatal("the second replica did not load the verified hostname; the rest of " +
			"this test would prove nothing")
	}

	sub := &redirect.Subscriber{
		Redis: rdb, Hosts: other, ReconnectBackoff: 10 * time.Millisecond,
	}
	subCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go sub.Run(subCtx)

	// Wait the establishment flush out, so the drop below can only come from the
	// message.
	waitFor(t, 10*time.Second, "the subscriber to establish and flush", func() bool {
		return reloads.Load() >= 2
	})
	if _, ok := other.Lookup("replica.custom.test"); !ok {
		t.Fatal("the hostname went out of the second replica's set before anything " +
			"was published")
	}

	// The un-verification happens on the first replica: the column is cleared and
	// the broadcast goes out.
	if _, err := f.pool.Exec(ctx,
		`UPDATE domains SET verified_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	publisher.PublishHostInvalidation(ctx)

	waitFor(t, 10*time.Second, "the other replica to stop serving the hostname", func() bool {
		_, ok := other.Lookup("replica.custom.test")
		return !ok
	})
}
