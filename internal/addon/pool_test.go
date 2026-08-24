package addon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- reuse happens at all -----------------------------------------------------

// The measurement this milestone exists to move, from the side a unit test can
// see: a redirect no longer builds a wasm instance.
//
// Counting instances rather than timing them, because a timing test on a busy
// machine measures the machine. h.instances is the monotonic counter every
// instance name is drawn from, so it is exactly how many were made.
func TestAnInlineRedirectReusesTheInstanceTheLastOneUsed(t *testing.T) {
	h, _, _ := redirectHost(t, PermissionRedirectInline, "config.read")
	d := decisionFor("quiet", "https://example.test/")
	const runs = 25
	for range runs {
		if got := h.Inline(t.Context(), d); got.Vetoed {
			t.Fatalf("the fixture stopped answering, so this measured the wrong thing: %+v", got)
		}
	}
	if made := h.instances.Load(); made != 1 {
		t.Errorf("%d redirects made %d instances; pooling means one", runs, made)
	}
	if idle := h.idleInstanceCount(); idle != 1 {
		t.Errorf("after %d sequential redirects the pool holds %d instances, want 1", runs, idle)
	}
}

// The observe class gets the same treatment, which m66.5 required rather than
// left to be argued: it pays the same startup on the same slots.
func TestAnObservingAddonReusesItsInstanceToo(t *testing.T) {
	h, sink, _ := redirectHost(t, PermissionRedirectObserve, "config.read")
	for i := range 5 {
		h.Observe(RedirectEvent{LinkID: "link-" + string(rune('a'+i)), Country: "DE"})
	}
	waitFor(t, sink, "redirect: observed_link=link-e")
	if made := h.instances.Load(); made != 1 {
		t.Errorf("five observations made %d instances; pooling means one", made)
	}
}

// A module holding both classes gets an idle set per class, because package
// initialization runs once per instance and the redirect-safe subset applies to
// it: an entry whose init ran as an observer must not later serve an inline
// invocation, where storage is refused.
func TestTheTwoClassesDoNotShareAnInstance(t *testing.T) {
	h, sink, _ := redirectHost(t, PermissionRedirectInline, PermissionRedirectObserve,
		"config.read")
	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	h.Observe(RedirectEvent{LinkID: "one", Country: "DE"})
	waitFor(t, sink, "redirect: observed_link=one")
	if made := h.instances.Load(); made != 2 {
		t.Errorf("one invocation of each class made %d instances; the two classes are "+
			"pooled apart, so it is two", made)
	}
	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	h.Observe(RedirectEvent{LinkID: "two", Country: "DE"})
	waitFor(t, sink, "redirect: observed_link=two")
	if made := h.instances.Load(); made != 2 {
		t.Errorf("a second invocation of each class made %d instances in total; both "+
			"should have been reused", made)
	}
}

// --- the isolation claim ------------------------------------------------------

// **The central claim of M66.5.** A pooled instance carries the guest's memory,
// so reuse would hand the next redirect whatever the last one left there. The
// host resets it, and this is what says so.
//
// Two redirects through one entry — asserted, not assumed, by the instance count
// — and the second reports `<none>` where a dirty instance would report the first
// one's destination. Removing the mem.Write in releaseInstance turns the second
// report into the first destination and fails this test, which is the sabotage it
// was written against.
func TestAPooledInstanceCannotSeeTheLastRedirect(t *testing.T) {
	h, sink, _ := redirectHost(t, PermissionRedirectInline, "config.read")
	const first = "https://first.example.test/one"
	const second = "https://second.example.test/two"
	h.Inline(t.Context(), decisionFor("remember", first))
	h.Inline(t.Context(), decisionFor("remember", second))

	if made := h.instances.Load(); made != 1 {
		t.Fatalf("the two redirects ran in %d instances, so this test proves nothing "+
			"about reuse", made)
	}
	logs := sink.String()
	if n := strings.Count(logs, "redirect: remembered=<none>"); n != 2 {
		t.Errorf("the second redirect through a pooled instance saw the first one's "+
			"residue: %d of 2 invocations found guest memory as _initialize left it\n%s",
			n, logs)
	}
	if strings.Contains(logs, "redirect: remembered="+first) {
		t.Errorf("a pooled instance handed the second redirect the first one's "+
			"destination\n%s", logs)
	}
}

// The same claim across many reuses, because a reset that works once and drifts
// is the failure a two-invocation test cannot see: the image is written back over
// the same instance every time, and a Go runtime that had been left inconsistent
// would stop answering rather than answer wrongly.
func TestAPooledInstanceStaysCleanAcrossManyRedirects(t *testing.T) {
	h, sink, _ := redirectHost(t, PermissionRedirectInline, "config.read")
	const runs = 50
	for i := range runs {
		d := decisionFor("remember", "https://example.test/"+strings.Repeat("x", i))
		if got := h.Inline(t.Context(), d); got.Vetoed {
			t.Fatalf("invocation %d was refused: %+v", i, got)
		}
	}
	if made := h.instances.Load(); made != 1 {
		t.Fatalf("%d redirects ran in %d instances, so this test proves nothing", runs, made)
	}
	if n := strings.Count(sink.String(), "redirect: remembered=<none>"); n != runs {
		t.Errorf("%d of %d reuses of one instance found guest memory reset; the rest "+
			"read the previous redirect's destination", n, runs)
	}
}

// --- a killed instance leaves the pool ----------------------------------------

// m66.5's third bullet. wazero closes the module underneath a call it interrupts,
// so a killed instance is a dead one — and returning it to the pool would hand the
// next redirect a module that cannot run. The pool evicts it, and the redirect
// after it works.
//
// The bound is hostile rather than shipped, for the reason every bound test in
// this package sets its own: a test that leaves a machine-dependent number at its
// default is a test asserting this machine is fast, which is F326.
func TestAKilledInstanceIsEvictedAndTheNextRedirectStillWorks(t *testing.T) {
	h, sink, metrics := redirectHostBounded(t, "slow", time.Millisecond,
		testInstantiateDeadline, PermissionRedirectInline, "config.read")
	got := h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	if got.Vetoed {
		t.Fatalf("a killed invocation vetoed a redirect: %+v", got)
	}
	if !strings.Contains(scrape(t, metrics),
		`linkctrl_addon_redirect_kills_total{addon="slow",step="call"} 1`) {
		t.Fatalf("the invocation was not killed, so nothing was evicted\n%s", sink.String())
	}
	if idle := h.idleInstanceCount(); idle != 0 {
		t.Errorf("a killed instance was returned to the pool: %d idle", idle)
	}
	// And the pool is not poisoned: the next redirect makes a new instance and is
	// served, which is what "degrades to M66's behaviour" means in practice.
	before := h.instances.Load()
	if got := h.Inline(t.Context(), decisionFor("quiet", "https://example.test/")); got.Vetoed {
		t.Errorf("the redirect after a kill was vetoed: %+v", got)
	}
	if made := h.instances.Load() - before; made != 1 {
		t.Errorf("the redirect after a kill reused %d instances rather than building "+
			"one; the dead entry is still in the pool", 1-made)
	}
}

// --- the two bounds -----------------------------------------------------------

// The size bound holds what is kept at rest, and it is the term the guest-memory
// ceiling gained. Sixteen concurrent invocations are allowed to *run*; only
// PoolSize of their instances survive being released.
func TestThePoolKeepsNoMoreThanItsSize(t *testing.T) {
	h := poolHost(t, 2, DefaultPoolTTL, PermissionRedirectInline)
	var wg sync.WaitGroup
	for range maxConcurrentRoutes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Inline(context.Background(), decisionFor("quiet", "https://example.test/"))
		}()
	}
	wg.Wait()
	if idle := h.idleInstanceCount(); idle > 2 {
		t.Errorf("the pool holds %d instances against a size of 2", idle)
	}
	// And the bound did not become a concurrency bound: every invocation that got
	// a slot ran, whatever the pool could keep afterwards.
	if made := h.instances.Load(); made == 0 {
		t.Error("no instance was made at all, so nothing ran")
	}
}

// The lifetime bound. An instance nothing has wanted for a TTL is closed, so idle
// cost is proportional to traffic rather than to the busiest moment since boot.
func TestAnIdleInstanceIsClosedAfterItsTTL(t *testing.T) {
	h := poolHost(t, DefaultPoolSize, 40*time.Millisecond, PermissionRedirectInline)
	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	if idle := h.idleInstanceCount(); idle != 1 {
		t.Fatalf("the redirect left %d instances in the pool, want 1", idle)
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.idleInstanceCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("an idle instance outlived its TTL by ten seconds")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The seam M67 consumes. Removing an add-on has to end the instances made from
// its module, and a pool whose invalidation nobody owns is where this repository's
// cache defects have always lived.
func TestDrainingAnAddonsPoolClosesItsInstances(t *testing.T) {
	h, _, _ := redirectHost(t, PermissionRedirectInline, "config.read")
	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	if idle := h.idleInstanceCount(); idle != 1 {
		t.Fatalf("the redirect left %d instances in the pool, want 1", idle)
	}
	h.drainPool(t.Context(), "redirect", h.current().pools)
	if idle := h.idleInstanceCount(); idle != 0 {
		t.Errorf("draining left %d instances in the pool", idle)
	}
	// The add-on still works afterwards: draining is not disabling.
	if got := h.Inline(t.Context(), decisionFor("veto", "https://example.test/")); !got.Vetoed {
		t.Error("the add-on stopped answering after its pool was drained")
	}
}

// Closing a host closes what the pool holds, which is the other half of the same
// obligation: an instance is a module, and a runtime closed out from under one is
// how this package's shutdown defects have looked.
func TestClosingAHostEmptiesThePool(t *testing.T) {
	h, _, _ := redirectHost(t, PermissionRedirectInline, "config.read")
	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	pools := h.current().pools
	if err := h.Close(context.Background()); err != nil {
		t.Fatalf("closing the host: %v", err)
	}
	if idle := h.idleInstanceCount(); idle != 0 {
		t.Errorf("a closed host still holds %d add-on instances", idle)
	}
	// Emptied rather than discarded. The set a closed host publishes is the empty
	// one (M67), so this reads the map the pool entries were actually in — the one
	// Close drained — rather than what a reader would find now.
	for key, p := range pools {
		if left := p.drain(); len(left) != 0 {
			t.Errorf("the %q pool still holds %d instances after Close", key, len(left))
		}
	}
}

// The defaults are what an operator gets, and a zero in Options is a caller that
// did not care rather than a caller asking for no pool. Asserted because "0 means
// the default" is exactly the reading .env.example promises.
func TestAZeroPoolSettingMeansTheDefault(t *testing.T) {
	h, _, _ := redirectHost(t, PermissionRedirectInline, "config.read")
	if h.poolSize != DefaultPoolSize {
		t.Errorf("an unset pool size gave %d, want %d", h.poolSize, DefaultPoolSize)
	}
	if h.poolTTL != DefaultPoolTTL {
		t.Errorf("an unset pool TTL gave %v, want %v", h.poolTTL, DefaultPoolTTL)
	}
}

// --- what the reset costs -----------------------------------------------------

// The milestone's premise as an assertion rather than as a paragraph: resetting a
// pooled instance is **cheaper than building one**, which is the whole of why the
// pool is worth its memory.
//
// Both halves are measured in the same test on the same machine, so the comparison
// survives a slow one and survives `-race` — each cost moves together. The ratio
// is what is asserted; the two numbers are logged because docs/slo.md quotes the
// reset's, and a number nobody can see is not measured into anything.
func TestResettingAPooledInstanceIsCheaperThanBuildingOne(t *testing.T) {
	if testing.Short() {
		t.Skip("a timing measurement")
	}
	h, _, _ := redirectHost(t, PermissionRedirectInline, "config.read")
	l := &h.current().inline[0]
	ctx := t.Context()

	// One outside each measurement, for the reason the invocation measurement takes
	// one: the first instance of a compiled module pays for whatever the runtime
	// caches on the way.
	first, err := h.acquireInstance(ctx, l, ClassInline)
	if err != nil {
		t.Fatalf("the first instance would not start: %v", err)
	}
	image := len(first.image)
	h.closeInstance(ctx, first)

	const runs = 20
	build := time.Now()
	for range runs {
		e, err := h.acquireInstance(ctx, l, ClassInline)
		if err != nil {
			t.Fatalf("instantiating: %v", err)
		}
		h.closeInstance(ctx, e)
	}
	perBuild := time.Since(build) / runs

	pooled, err := h.acquireInstance(ctx, l, ClassInline)
	if err != nil {
		t.Fatalf("the pooled instance would not start: %v", err)
	}
	reset := time.Now()
	for i := range runs {
		h.releaseInstance(ctx, pooled, true)
		got, err := h.acquireInstance(ctx, l, ClassInline)
		if err != nil {
			t.Fatalf("re-acquiring on pass %d: %v", i, err)
		}
		if got != pooled {
			t.Fatalf("pass %d got a different instance, so this measured a build", i)
		}
	}
	perReset := time.Since(reset) / runs
	h.releaseInstance(ctx, pooled, true)

	t.Logf("resetting a pooled instance costs %s a use over %d bytes of guest memory; "+
		"building one costs %s", perReset.Round(time.Microsecond), image,
		perBuild.Round(time.Microsecond))
	if perReset >= perBuild {
		t.Errorf("resetting an instance costs %s and building one costs %s, so the pool "+
			"buys nothing and the milestone's premise does not hold on this machine",
			perReset, perBuild)
	}
}

// poolHost opens a host on the `redirect` fixture with both pool bounds named.
func poolHost(t *testing.T, size int, ttl time.Duration, permissions ...string) *Host {
	t.Helper()
	code := fixture(t, "redirect")
	dir := t.TempDir()
	m := manifestFor("redirect", ClassDegrade, code)
	m.Permissions = append(permissions, "config.read")
	m.Settings = []Setting{{Name: "retention_days", Type: SettingText, Default: "30"}}
	install(t, dir, m, code)
	h, err := Open(t.Context(), Options{
		Dir:                 dir,
		InlineDeadline:      testInlineDeadline,
		InstantiateDeadline: testInstantiateDeadline,
		PoolSize:            size,
		PoolTTL:             ttl,
	})
	if err != nil {
		t.Fatalf("the redirect fixture did not load: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}
