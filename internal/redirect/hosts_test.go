package redirect

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// The host cache's scheduling carries a stronger promise than the alias tier's:
// an invalidation is not a hint to expire sooner, it is "a hostname's right to
// be served just changed", and M40's definition of done says no replica keeps
// serving a domain that was just unverified. So what these tests pin down is
// not the query — the loader is substituted — but the promise itself: an
// arming is never lost, however it interleaves with a running reload, and a
// failed reload leaves the work owed rather than consumed.

func newTestHostCache(load func(context.Context) (map[string]VerifiedDomain, error)) *HostCache {
	c := NewHostCache(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.load = load
	return c
}

// waitFor polls for a condition that a background worker establishes. Two
// seconds is orders of magnitude beyond a stubbed load; hitting it means the
// condition will never hold, not that the machine is slow.
func waitFor(t *testing.T, failure string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(failure)
		}
		time.Sleep(time.Millisecond)
	}
}

// schedState reads the worker flags the way the scheduler itself does.
func schedState(c *HostCache) (running, pending bool) {
	c.schedMu.Lock()
	defer c.schedMu.Unlock()
	return c.running, c.pending
}

// countingLoader returns a loader that counts calls and serves one hostname.
func countingLoader(calls *atomic.Int32) func(context.Context) (map[string]VerifiedDomain, error) {
	return func(context.Context) (map[string]VerifiedDomain, error) {
		calls.Add(1)
		return map[string]VerifiedDomain{
			"go.example.com": {Hostname: "go.example.com"},
		}, nil
	}
}

// An invalidation that lands in the worker's exit window must still be served.
//
// The window: a reload worker has looked at the pending flag, found it clear,
// and left its loop — but has not yet given back the running flag. A Refresh
// arriving at that instant arms pending, sees running held, and returns on the
// strength of "whoever holds the flag will look again". If the holder's last
// look is already behind it, the arming is stranded: pending set, nobody
// running, and on a quiet instance — no later traffic, no later operator
// action — the revoked-hostname set never reloads.
//
// The schedule is reproduced deterministically by playing the worker's part
// from the test: hold the running flag, let Refresh land, then run the real
// exit path to completion on this goroutine. The assertion is the promise, not
// the mechanism: the arming must produce a reload before the worker retires.
func TestRefreshInWorkerExitWindowStillReloads(t *testing.T) {
	var calls atomic.Int32
	c := newTestHostCache(countingLoader(&calls))

	// A worker exists and has already consumed every arming so far.
	c.schedMu.Lock()
	c.running = true
	c.schedMu.Unlock()

	// The invalidation lands while the flag is held: armed, no new worker.
	c.Refresh(context.Background())
	if n := calls.Load(); n != 0 {
		t.Fatalf("Refresh started a second worker alongside a live one: %d loads", n)
	}

	// The worker now reaches its exit. Its check of pending and its release of
	// running are one critical section, so the arming above must be seen and
	// served before it retires — the stranded state (armed, nobody running)
	// must be unreachable.
	c.reloadWorker(context.Background())

	if n := calls.Load(); n != 1 {
		t.Fatalf("the worker retired past an armed invalidation: %d loads, want 1", n)
	}
	if running, pending := schedState(c); running || pending {
		t.Fatalf("after draining: running=%v pending=%v, want both clear", running, pending)
	}
}

// A reload that fails must leave the invalidation armed, not consume it.
//
// The arming is a fact about the world — the verified set changed — and a
// failed query has not made that false. Consuming the flag on the error path
// means the change is applied never rather than late: nothing else re-arms it,
// and the hourly backstop is absent entirely when domain verification is
// switched off. Armed-and-waiting costs one redundant bit; dropped costs a
// replica serving a revoked hostname indefinitely.
func TestFailedReloadKeepsTheInvalidationArmed(t *testing.T) {
	var calls atomic.Int32
	boom := errors.New("postgres is away")
	c := newTestHostCache(func(context.Context) (map[string]VerifiedDomain, error) {
		if calls.Add(1) == 1 {
			return nil, boom
		}
		return map[string]VerifiedDomain{
			"go.example.com": {Hostname: "go.example.com"},
		}, nil
	})

	c.Refresh(context.Background())
	waitFor(t, "the failing worker never retired", func() bool {
		running, _ := schedState(c)
		return calls.Load() >= 1 && !running
	})

	// One attempt, then wait for the next trigger. A worker that looped on a
	// persistent error would have every replica hammering a database that is
	// already in trouble.
	if n := calls.Load(); n != 1 {
		t.Fatalf("a failing reload was attempted %d times; one attempt per arming, not a retry loop", n)
	}
	if _, pending := schedState(c); !pending {
		t.Fatal("the failed reload consumed the invalidation: pending is no longer armed")
	}

	// The next trigger — the subscriber's next message, the operator's next
	// change — retries and the set becomes visible.
	c.Refresh(context.Background())
	waitFor(t, "a later Refresh did not retry the armed invalidation", func() bool {
		_, ok := c.Lookup("go.example.com")
		return ok
	})
}

// A burst of armings during one load costs one more load, not one each.
// This is the collapse the fields exist for, pinned so the lost-wakeup and
// error-path guarantees cannot be bought by giving it up.
func TestRefreshCollapsesABurstIntoOneFollowUpLoad(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var calls atomic.Int32
	c := newTestHostCache(func(context.Context) (map[string]VerifiedDomain, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return map[string]VerifiedDomain{}, nil
	})

	c.Refresh(context.Background())
	<-entered // the worker is inside the first load

	for i := 0; i < 8; i++ {
		c.Refresh(context.Background())
	}
	close(release)

	waitFor(t, "the worker never drained the burst and retired", func() bool {
		running, pending := schedState(c)
		return !running && !pending
	})
	if n := calls.Load(); n != 2 {
		t.Fatalf("8 armings during one load cost %d loads in total, want 2", n)
	}
}

// Reload's own contract, exercised through the substituted loader: the set is
// installed whole, readiness flips, OnReload fires, and Lookup answers through
// the same normalization the keys were built with.
func TestReloadInstallsTheSetAndSignals(t *testing.T) {
	var calls atomic.Int32
	c := newTestHostCache(countingLoader(&calls))
	reloads := 0
	c.OnReload = func() { reloads++ }

	if c.Ready() {
		t.Fatal("ready before any load has succeeded")
	}
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !c.Ready() || reloads != 1 {
		t.Fatalf("after one load: ready=%v reloads=%d, want true and 1", c.Ready(), reloads)
	}
	// A port on the Host header must not defeat the match (F88's lesson).
	if _, ok := c.Lookup("go.example.com:8443"); !ok {
		t.Fatal("Lookup missed a verified hostname spelled with a port")
	}
	if _, ok := c.Lookup("scanner.example.net"); ok {
		t.Fatal("Lookup served a hostname that was never verified")
	}
}
