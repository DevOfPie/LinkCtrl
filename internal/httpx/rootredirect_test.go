package httpx

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// One refill serves the whole herd, and the refill is bounded.
//
// Both halves are F48, and both matter because of where this handler sits. It is
// mounted on the redirect mux, which deliberately has no RequestTimeout —
// appHandler is where that middleware lives, and the redirect tree is outside
// it — so before this the only bound on the read was the server's 30s
// WriteTimeout. And M23's subscriber calls InvalidateRoot from flush() on every
// re-establishment including the first, so a miss is fleet-simultaneous by
// design rather than by bad luck: every in-flight request on every replica would
// issue its own query for a value that changes approximately never.
func TestTheRootRedirectRefillsOnceAndUnderADeadline(t *testing.T) {
	var (
		loads    atomic.Int64
		deadline atomic.Bool
		release  = make(chan struct{})
	)

	h := &RootRedirect{
		TTL: time.Hour,
		Load: func(ctx context.Context) (string, error) {
			loads.Add(1)
			if _, ok := ctx.Deadline(); ok {
				deadline.Store(true)
			}
			// Hold the first loader open so the others pile up behind it. A
			// refill that is not serialized would have every one of them issue
			// its own query while this one is still running, which is exactly
			// the state a fleet-wide invalidation produces.
			<-release
			return "https://example.com/", nil
		},
	}

	const callers = 8
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		}()
	}

	// Let the piled-up callers reach the handler before the first load returns.
	// Without the wait this test could pass by scheduling luck rather than by
	// the exclusion being there.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := loads.Load(); n != 1 {
		t.Errorf("%d concurrent requests produced %d loads, want exactly 1 — the "+
			"refill is not serialized, and M23's flush makes this miss simultaneous "+
			"on every replica", callers, n)
	}
	if !deadline.Load() {
		t.Error("the refill ran with no deadline. This handler is on the redirect mux, " +
			"which has no RequestTimeout, so nothing else supplies one")
	}
}

// A second request after the TTL lapses refills again, so the exclusion above
// is a bound on concurrency and not an accidental once.Do.
func TestTheRootRedirectStillRefreshesWhenItsTTLLapses(t *testing.T) {
	var loads atomic.Int64
	h := &RootRedirect{
		TTL: time.Millisecond,
		Load: func(context.Context) (string, error) {
			loads.Add(1)
			return "https://example.com/", nil
		},
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	time.Sleep(5 * time.Millisecond)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if n := loads.Load(); n != 2 {
		t.Errorf("%d loads across a lapsed TTL, want 2 — serializing the refill must "+
			"not turn it into a one-shot", n)
	}
}
