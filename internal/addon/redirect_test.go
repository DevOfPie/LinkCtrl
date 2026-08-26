package addon

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// redirectHost installs the `redirect` fixture with the permissions given and
// opens a host on it.
//
// The fixture is one module and one manifest for every case in this file, because
// what it does is decided by the alias it is handed — see its own doc comment.
// What changes between tests is the *manifest*, which is the thing the host reads.
func redirectHost(t *testing.T, permissions ...string) (*Host, *logSink, *observability.Metrics) {
	t.Helper()
	return redirectHostNamed(t, "redirect", permissions...)
}

// testInlineDeadline and testInstantiateDeadline are the budgets every test in
// this file that is **not** about a bound runs with, and they are deliberately not
// the shipped defaults.
//
// The shipped call deadline is 25 ms, which is roughly six times what a guest call
// costs on this machine — and the race detector costs this measurement an order of
// magnitude (D225's effect, on the invocation path). Under `-race`, with the rest
// of this package running in parallel, an ordinary invocation reaches and passes
// 25 ms, so a test asserting what a module *answered* would fail because the host
// correctly killed it. That is the deadline working, reported as a broken rewrite.
//
// **The instantiation budget is here for a harder reason, and it is F326's.** The
// five integration tests that shipped a broken M66 were green on this VM and could
// not pass on a hosted runner, because they left that bound at the shipped default
// and the runner needed more of it than this machine does. A test that leaves a
// machine-dependent bound at its default is a test asserting that this machine is
// fast. So a behaviour test here buys room it could never plausibly need, and the
// tests that are *about* a bound set a hostile one with [redirectHostBounded] —
// which is what makes a slow machine reachable on a machine that is not slow.
const (
	testInlineDeadline      = 10 * time.Second
	testInstantiateDeadline = 10 * time.Second
)

// redirectHostNamed opens a host on one fixture with room to spare on both bounds.
func redirectHostNamed(t *testing.T, module string,
	permissions ...string,
) (*Host, *logSink, *observability.Metrics) {
	t.Helper()
	return redirectHostBounded(t, module, testInlineDeadline, testInstantiateDeadline,
		permissions...)
}

// redirectHostBounded is the same with both bounds named, for the tests whose
// subject is a bound.
func redirectHostBounded(t *testing.T, module string, deadline, instantiate time.Duration,
	permissions ...string,
) (*Host, *logSink, *observability.Metrics) {
	t.Helper()
	code := fixture(t, module)
	dir := t.TempDir()
	m := manifestFor(module, ClassDegrade, code)
	m.Permissions = permissions
	// A declared setting, so the fixture's inline call to config_get is a call to a
	// function it may make rather than one refused for a reason this file is not
	// about.
	m.Settings = []Setting{{Name: "retention_days", Type: SettingText, Default: "30"}}
	install(t, dir, m, code)

	metrics := observability.NewMetrics()
	sink := &logSink{}
	h, err := Open(t.Context(), Options{
		Dir:                 dir,
		Metrics:             metrics,
		Logger:              slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
		InlineDeadline:      deadline,
		InstantiateDeadline: instantiate,
	})
	if err != nil {
		t.Fatalf("the %s fixture did not load: %v", module, err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h, sink, metrics
}

func decisionFor(alias, destination string) RedirectDecision {
	return RedirectDecision{
		LinkID:      uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		WorkspaceID: uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Alias:       alias,
		Destination: destination,
	}
}

// --- the two classes are the two grants -------------------------------------

// m66.md's first bullet, from the side that decides it: what puts a module on the
// redirect path is the grant its manifest declared and nothing else.
//
// The negative half is the one worth having. A module that declared `storage` and
// `config` and exports both redirect handlers is still never called, because the
// host resolves the inline set from the *grants* and not from what the wasm
// happens to export — and a module cannot make itself inline by exporting a
// function with the right name.
func TestOnlyADeclaredClassReachesTheRedirectPath(t *testing.T) {
	h, _, _ := redirectHost(t, "config.read")
	if h.HasInline() {
		t.Error("a module that declared no redirect class is on the redirect path, and " +
			"it exports the handler — which is exactly the thing that must not be enough")
	}
	if got := h.ObservingAddons(); len(got) != 0 {
		t.Errorf("a module that declared no redirect class is observing: %v", got)
	}
	// And the invocation itself is a no-op rather than a refusal, which is what the
	// redirect path is entitled to: nothing installed and nothing to say.
	got := h.Inline(t.Context(), decisionFor("veto", "https://example.test/"))
	if got.Vetoed || got.Rewritten || got.Destination != "https://example.test/" {
		t.Errorf("an add-on with no redirect grant changed a redirect: %+v", got)
	}
}

// The zero-cost case, which is every instance that installed no add-on at all.
// Asserted rather than promised, because this runs on every redirect.
func TestNoHostMeansNothingOnTheRedirectPath(t *testing.T) {
	var h *Host
	if h.HasInline() {
		t.Fatal("a nil host reports something on the redirect path")
	}
	got := h.Inline(context.Background(), decisionFor("veto", "https://example.test/"))
	if got.Vetoed || got.Destination != "https://example.test/" {
		t.Errorf("a nil host changed a redirect: %+v", got)
	}
	h.Observe(RedirectEvent{})
	if allocs := testing.AllocsPerRun(1000, func() {
		if h.HasInline() {
			t.Fatal("measured the wrong branch")
		}
	}); allocs != 0 {
		t.Errorf("the redirect path's add-on check allocated %v times per run", allocs)
	}
}

// --- what an inline module may do -------------------------------------------

// The ordinary case: a module reads the decision, says nothing, and the redirect
// is exactly what it would have been.
//
// It also asserts the *record* reached the guest, because a module that could not
// see the decision would answer nothing for a reason this file would otherwise
// not tell apart from a module that chose to.
func TestAnInlineModuleReadsTheDecisionAndMayLeaveItAlone(t *testing.T) {
	h, sink, metrics := redirectHost(t, PermissionRedirectInline, "config.read")
	if !h.HasInline() {
		t.Fatal("the module declared redirect.inline and is not on the path")
	}
	const dest = "https://example.test/landing?a=1"
	got := h.Inline(t.Context(), decisionFor("quiet", dest))
	if got.Vetoed || got.Rewritten || got.Destination != dest {
		t.Errorf("a module that answered nothing changed the redirect: %+v", got)
	}
	logs := sink.String()
	for _, want := range []string{
		"redirect: alias=quiet",
		"redirect: destination=" + dest,
		"redirect: answer_allow=silent",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the fixture did not report %q\n%s", want, logs)
		}
	}
	// Per-module attribution, which is the owner's second requirement on the
	// redirect answer: the add-on's time is its own curve and not folded into
	// core's.
	scraped := scrape(t, metrics)
	series := `linkctrl_addon_redirect_duration_seconds_count{addon="redirect",class="inline"} 1`
	if !strings.Contains(scraped, series) {
		t.Errorf("the scrape does not carry %s\n%s", series,
			seriesLike(scraped, "linkctrl_addon_redirect_duration_seconds"))
	}
	if killed := seriesLike(scraped, "linkctrl_addon_redirect_kills_total"); killed != "" {
		t.Errorf("a module that answered on time was counted as killed: %s", killed)
	}
}

// A veto, which is the power the inline class exists for.
func TestAnInlineModuleMayVeto(t *testing.T) {
	h, sink, _ := redirectHost(t, PermissionRedirectInline, "config.read")
	got := h.Inline(t.Context(), decisionFor("veto", "https://example.test/"))
	if !got.Vetoed {
		t.Fatalf("the module vetoed and the host did not report it: %+v\n%s", got, sink.String())
	}
	// The host's own voice, naming the add-on: an operator asked why a link stopped
	// working needs to know which module answered.
	if logs := sink.String(); !strings.Contains(logs, "an add-on vetoed a redirect") ||
		!strings.Contains(logs, `addon=redirect`) {
		t.Errorf("the veto was not attributed in the log\n%s", logs)
	}
}

// The rewrite, and its bound. D317 in one test: the module may alter the query
// and may not reach anything else, and the case that bought it the power is
// stripping tracking parameters.
func TestAnInlineModuleMayRewriteTheQueryAndNothingElse(t *testing.T) {
	h, _, _ := redirectHost(t, PermissionRedirectInline, PermissionRewriteQuery, "config.read")
	const dest = "https://shop.example.test/item/42?utm_source=x&id=7&fbclid=abc&utm_medium=y"
	got := h.Inline(t.Context(), decisionFor("strip", dest))
	if got.Vetoed {
		t.Fatal("a rewriting module vetoed")
	}
	if !got.Rewritten {
		t.Fatalf("the destination did not change: %q", got.Destination)
	}
	if want := "https://shop.example.test/item/42?id=7"; got.Destination != want {
		t.Errorf("the destination is %q, want %q", got.Destination, want)
	}

	// Dropping the query entirely, which is what the `rewrite` flag exists to make
	// expressible: an empty query is a real answer and not an absent one.
	got = h.Inline(t.Context(), decisionFor("drop", dest))
	if want := "https://shop.example.test/item/42"; got.Destination != want {
		t.Errorf("dropping the query gave %q, want %q", got.Destination, want)
	}
}

// The second grant, refused. **This is the whole of why the rewrite is a token of
// its own** (D317): a manifest that declared *run on the redirect path* has not
// declared *and edit where the visitor goes*.
func TestARewriteWithoutItsOwnGrantIsRefusedAndTheRedirectIsUnchanged(t *testing.T) {
	h, sink, metrics := redirectHost(t, PermissionRedirectInline, "config.read")
	const dest = "https://shop.example.test/item/42?fbclid=abc"
	got := h.Inline(t.Context(), decisionFor("strip", dest))
	if got.Rewritten || got.Destination != dest {
		t.Errorf("a module without redirect.rewrite_query rewrote the destination: %+v", got)
	}
	// Denied, and the module was told so — a refusal it cannot see is a refusal it
	// cannot report to its own author.
	if logs := sink.String(); !strings.Contains(logs, "redirect: answer_strip=denied") {
		t.Errorf("the module was not told its rewrite was denied\n%s", logs)
	}
	// On the same series an undeclared call is counted on, because it is the same
	// question: an operator asking "is anything being refused" gets one answer.
	scraped := scrape(t, metrics)
	series := `linkctrl_addon_refusals_total{addon="redirect",permission="redirect.rewrite_query"}`
	if !strings.Contains(scraped, series) {
		t.Errorf("the scrape does not carry %s\n%s", series,
			seriesLike(scraped, "linkctrl_addon_refusals_total"))
	}
}

// Everything a module can get wrong in its answer, from the guest side, and none
// of it changes the redirect.
func TestAMalformedAnswerIsRefusedAndTheRedirectStands(t *testing.T) {
	h, sink, _ := redirectHost(t, PermissionRedirectInline, PermissionRewriteQuery, "config.read")
	const dest = "https://example.test/p?a=1"
	for _, tc := range []struct{ alias, report string }{
		// A `#` would start a fragment, which is the one character that makes a query
		// substitution reach past the query.
		{"badquery", "redirect: answer_badquery=invalid"},
		// A verdict outside the closed vocabulary. Refused rather than read as allow,
		// because a module that spelled `veto` wrongly meant to refuse somebody.
		{"verdict", "redirect: answer_bad_verdict=invalid"},
	} {
		got := h.Inline(t.Context(), decisionFor(tc.alias, dest))
		if got.Vetoed || got.Rewritten || got.Destination != dest {
			t.Errorf("%s: a refused answer changed the redirect: %+v", tc.alias, got)
		}
		if logs := sink.String(); !strings.Contains(logs, tc.report) {
			t.Errorf("%s: the fixture did not report %q\n%s", tc.alias, tc.report, logs)
		}
	}

	// Two answers for one invocation: the first stands and the second is refused,
	// for the reason a second HTTP response is. A module that vetoed after allowing
	// does not know which the visitor got.
	got := h.Inline(t.Context(), decisionFor("twice", dest))
	if got.Vetoed {
		t.Error("the second answer replaced the first, and a module that answered twice " +
			"does not know which one the visitor got")
	}
	logs := sink.String()
	if !strings.Contains(logs, "redirect: answer_first=ok") ||
		!strings.Contains(logs, "redirect: answer_second=invalid") {
		t.Errorf("the two-answer case did not report as expected\n%s", logs)
	}
}

// --- the redirect-safe subset ------------------------------------------------

// m66.md: *an inline module's host functions are the redirect-safe subset only —
// no storage I/O on the hot path; asserted by the ABI refusing the storage
// functions to an inline invocation.*
//
// Driven from the guest, because the claim is about what a module is told. The
// manifest declares every grant, so each refusal below is the *placement* refusing
// a capability the add-on genuinely holds — which is the only version of this
// worth asserting.
func TestAnInlineInvocationReachesOnlyTheRedirectSafeSubset(t *testing.T) {
	h, sink, _ := redirectHost(t, grantable()...)
	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))

	logs := sink.String()
	for _, want := range []string{
		// The pair the milestone names.
		"redirect: inline_storage_query=denied",
		"redirect: inline_storage_exec=denied",
		// And the rest of what the redirect tree does not do.
		"redirect: inline_session_context=denied",
		"redirect: inline_http_request=denied",
		// The other class's read, from this one.
		"redirect: inline_event_read=denied",
		// And egress (M68.5), which is the newest member of the set and the one
		// with a second refusal behind it — see
		// TestNeitherRedirectClassMayFetchAndTheGuestIsToldSo for why *denied*
		// rather than an outcome is the right word here.
		"redirect: inline_network_fetch=denied",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the fixture did not report %q\n%s", want, logs)
		}
	}
	// The other half: what the subset allows still works, so the refusals above are
	// about the subset and not about an instance that could not answer anything.
	// The fixture reports these only on failure, so their absence is the assertion.
	for _, unwanted := range []string{"redirect: inline_abi_version=", "redirect: inline_config_get="} {
		if strings.Contains(logs, unwanted) {
			t.Errorf("a redirect-safe function was refused inside an inline invocation\n%s", logs)
		}
	}
	// The host said so where an operator can read it, which is what makes a module
	// author's bug report answerable.
	if !strings.Contains(logs, "reaches only the redirect-safe subset") {
		t.Errorf("the host did not log the refusal\n%s", logs)
	}
}

// The ABI's own statement of the same bound, from the surface rather than from a
// running host: nothing that costs the storage permission is callable inline.
func TestTheInlineSafeSetHoldsNoStorageFunction(t *testing.T) {
	known := map[string]abi.Function{}
	for _, f := range abi.Functions {
		known[f.Name] = f
	}
	for _, name := range abi.InlineSafe {
		f, ok := known[name]
		if !ok {
			t.Errorf("abi.InlineSafe names %q and the ABI has no such function", name)
			continue
		}
		if f.Requires == abi.PermissionStorage {
			t.Errorf("%q costs the storage permission and is callable inside a redirect; "+
				"the whole of the redirect-safe subset is that there is no storage I/O on "+
				"the hot path", name)
		}
		if !f.Live {
			t.Errorf("%q is callable inline and this host does not implement it", name)
		}
	}
	// And the direction that catches a function added to the ABI and quietly waved
	// onto the hot path: every inline-safe entry is one somebody wrote down.
	for _, f := range abi.Functions {
		if abi.CallableInline(f.Name) && f.Requires == abi.PermissionStorage {
			t.Errorf("%q is inline-safe and costs storage", f.Name)
		}
	}
}

// --- the deadline -------------------------------------------------------------

// The availability half of the owner's boundary, and the reason it is a boundary
// rather than a giveaway: an add-on's latency is its own, and an add-on that never
// returns is killed so the redirect completes without it.
//
// The fixture loads in microseconds and then hangs *inside the invocation*, which
// is the case the load timeout cannot see — `spinning` never finishes loading and
// is bounded by a different mechanism entirely.
func TestAnOverrunningInlineModuleIsKilledAndTheRedirectSurvives(t *testing.T) {
	// A budget a test can afford to spend. The default is measured against a real
	// module doing real work; this one is measuring that the kill happens at all.
	h, sink, metrics := redirectHostBounded(t, "slow", 50*time.Millisecond,
		testInstantiateDeadline, PermissionRedirectInline, PermissionRewriteQuery)

	const dest = "https://example.test/still-here?a=1"
	start := time.Now()
	got := h.Inline(t.Context(), decisionFor("veto", dest))
	took := time.Since(start)

	// **Not vetoed**, and this is the load-bearing assertion of the whole
	// milestone's availability claim: a module the host had to kill wrote nothing,
	// and nothing means allow. A bug in an add-on must not be able to refuse
	// somebody's links.
	if got.Vetoed || got.Rewritten || got.Destination != dest {
		t.Errorf("a killed module changed the redirect: %+v", got)
	}
	if took > 5*time.Second {
		t.Fatalf("the invocation took %s, so the deadline bounded nothing", took)
	}
	scraped := scrape(t, metrics)
	if want := `linkctrl_addon_redirect_kills_total{addon="slow",step="call"} 1`; !strings.Contains(scraped, want) {
		t.Errorf("the scrape does not carry %s\n%s", want,
			seriesLike(scraped, "linkctrl_addon_redirect_kills_total"))
	}
	// Absent rather than zero: an invocation that never ran would drag a p99 towards
	// a latency nobody experienced.
	if strings.Contains(scraped, `linkctrl_addon_redirect_duration_seconds_count{addon="slow",class="inline"} 1`) {
		t.Error("a killed invocation was recorded on the duration histogram")
	}
	if logs := sink.String(); !strings.Contains(logs, "overran its deadline") {
		t.Errorf("the kill was not logged where an operator would read it\n%s", logs)
	}

	// A second one, which is the case m66.md's third risk names: the runtime closes
	// the *module*, so a repeatedly overrunning add-on must go on being killed
	// rather than turning into something else.
	h.Inline(t.Context(), decisionFor("veto", dest))
	if want := `linkctrl_addon_redirect_kills_total{addon="slow",step="call"} 2`; !strings.Contains(scrape(t, metrics), want) {
		t.Errorf("a second overrun was not counted; a thrashing module has to stay visible")
	}
}

// A redirect whose own request was cancelled is **not** an add-on overrunning, and
// counting it as one would put a number on the Add-on manager that blames the
// wrong party.
func TestAVisitorLeavingIsNotAnAddonOverrun(t *testing.T) {
	h, _, metrics := redirectHostBounded(t, "slow", time.Minute, testInstantiateDeadline,
		PermissionRedirectInline)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := h.Inline(ctx, decisionFor("veto", "https://example.test/"))
	if got.Vetoed {
		t.Error("a cancelled redirect was vetoed")
	}
	if series := seriesLike(scrape(t, metrics), "linkctrl_addon_redirect_kills_total"); series != "" {
		t.Errorf("a cancelled request was counted as an add-on overrun: %s", series)
	}
}

// **The two bounds must not add up, and they must not eat each other.**
//
// Splitting one deadline into two leaves a trap on the other side: a call context
// created before the instance exists is already part-spent when the guest is
// finally called, so the add-on's 25 ms quietly becomes 25 ms *minus whatever this
// machine took to start it* — F326 again with the numbers rearranged, and just as
// invisible. This is the assertion that the guest's budget starts when the guest
// does.
//
// Asserted on a module that never returns, because that is the only module whose
// guest-call duration is known: it is exactly the deadline. So the whole
// invocation has to cost the deadline **plus** an instantiation, and one
// millisecond is a floor no instantiation has ever come near — 1.6 ms measured
// idle, 9.6 ms mean under contention. The comparison is one-sided by construction:
// a host that charged instantiation to the guest can only be *faster* than this,
// never slower, so the test cannot fail for being on a slow machine.
func TestTheGuestsBudgetStartsWhenTheGuestDoes(t *testing.T) {
	const deadline = 50 * time.Millisecond
	h, _, _ := redirectHostBounded(t, "slow", deadline, testInstantiateDeadline,
		PermissionRedirectInline)

	start := time.Now()
	h.Inline(t.Context(), decisionFor("veto", "https://example.test/"))
	took := time.Since(start)

	if took < deadline+time.Millisecond {
		t.Errorf("a module that never returns was killed after %s against a %s "+
			"deadline, so instantiating it was charged to its own budget: the add-on "+
			"gets less of its deadline the slower the machine is", took, deadline)
	}
}

// F326, as a test that fails on a fast machine.
//
// **The defect this milestone was reopened for could not be reached by any suite
// on this VM**, because reaching it needed a machine that instantiates slowly and
// this one does not. The bound coming from configuration is what makes it
// reachable anyway: a hostile instantiation budget is a slow runner, deterministic
// and on any hardware.
//
// What it asserts is the pair of facts that were indistinguishable before. The
// redirect is unharmed — the module contributes nothing and nothing means allow —
// and an operator can tell *the add-on never ran* from *the add-on declined to
// act*, on the counter and in the log, without owning the machine.
func TestAModuleThisHostCannotStartInTimeIsNotTheAddonsFault(t *testing.T) {
	// One nanosecond, which no instantiation on any machine fits inside, so what
	// this measures is the branch rather than the hardware. The call budget is
	// generous: the point is that the invocation dies before the guest, not that
	// something died.
	h, sink, metrics := redirectHostBounded(t, "redirect", testInlineDeadline, time.Nanosecond,
		PermissionRedirectInline, PermissionRewriteQuery, "config.read")

	const dest = "https://shop.example.test/item/42?utm_source=x&id=7"
	got := h.Inline(t.Context(), decisionFor("strip", dest))

	// The `strip` alias is the fixture rewriting the query. Unrewritten is the whole
	// point: the module did not run, and a module that did not run changes nothing.
	if got.Vetoed || got.Rewritten || got.Destination != dest {
		t.Errorf("a module that was never started changed the redirect: %+v", got)
	}
	scraped := scrape(t, metrics)
	if want := `linkctrl_addon_redirect_kills_total{addon="redirect",step="instantiate"} 1`; !strings.Contains(scraped, want) {
		t.Errorf("the scrape does not carry %s, so nothing tells an operator the "+
			"add-on never ran\n%s", want,
			seriesLike(scraped, "linkctrl_addon_redirect_kills_total"))
	}
	if strings.Contains(scraped, `step="call"`) {
		t.Error("a module that was never started was counted as having overrun its " +
			"own deadline, which is the attribution F326 was about")
	}
	logs := sink.String()
	if !strings.Contains(logs, "could not start an add-on") ||
		!strings.Contains(logs, "LINKCTRL_ADDON_INSTANTIATE_DEADLINE") {
		t.Errorf("the log does not name the bound that was missed or the variable "+
			"that moves it\n%s", logs)
	}
	if strings.Contains(logs, "overran its deadline") {
		t.Errorf("the host blamed the add-on for its own setup cost\n%s", logs)
	}
}

// The observe class has the same two bounds and the same attribution, and it is
// worth its own case because F326 was in both call sites and only the inline one
// was found.
func TestAnObservingModuleThisHostCannotStartIsCountedTheSameWay(t *testing.T) {
	h, sink, metrics := redirectHostBounded(t, "redirect", testInlineDeadline,
		time.Nanosecond, PermissionRedirectObserve, "config.read")
	h.Observe(RedirectEvent{LinkID: "a-link", WorkspaceID: "a-workspace"})
	waitFor(t, sink, "could not start an add-on")
	if want := `linkctrl_addon_redirect_kills_total{addon="redirect",step="instantiate"}`; !strings.Contains(scrape(t, metrics), want) {
		t.Errorf("an observation that never reached the module was not counted\n%s",
			seriesLike(scrape(t, metrics), "linkctrl_addon_redirect_kills_total"))
	}
}

// What instantiation costs when the host is busy, which is the measurement D327
// asks for and the one D318 did not take.
//
// **The number D318 published was best-case** — one invocation at a time on an
// idle VM — and every entry that rests on it rests on that. This runs
// [maxConcurrentRoutes] instantiations against each other, which is the state a
// redirect meets when an add-on is installed and the instance is under load, and
// it is the number [DefaultInstantiateDeadline] is argued from.
//
// It reports rather than asserts a budget, for the reason the invocation
// measurement beside it does: a threshold on a shared machine is a flake. The one
// comparison it makes is against the shipped bound, because a bound a busy machine
// cannot instantiate inside is a bound that kills add-ons that were working — the
// same failure as F326, one step later.
func TestInstantiationCostsWhatItCostsUnderContention(t *testing.T) {
	if testing.Short() {
		t.Skip("a timing measurement")
	}
	h, _, _ := redirectHost(t, PermissionRedirectInline, "config.read")
	if len(h.current().inline) != 1 {
		t.Fatalf("the fixture is not on the redirect path: %v", h.InlineAddons())
	}
	l := &h.current().inline[0]
	d := decisionFor("quiet", "https://example.test/landing")

	// One instantiation is the unit: what the redirect path does per invocation,
	// registered exactly as invokeInline registers it, because a module whose
	// package initialization calls a host function must find its state.
	instantiate := func() time.Duration {
		instance := l.Manifest.Name + "#measured-" + strconv.FormatUint(h.instances.Add(1), 10)
		st := h.hostState(l.Manifest.Name).forRedirect(&d, nil)
		h.mu.Lock()
		if h.states == nil {
			h.states = make(map[string]*hostState)
		}
		h.states[instance] = st
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			delete(h.states, instance)
			h.mu.Unlock()
		}()
		at := time.Now()
		mod, err := h.runtime.InstantiateModule(t.Context(), l.compiled, guestModuleConfig(instance))
		took := time.Since(at)
		if err != nil {
			t.Errorf("the module did not instantiate, so this measured nothing: %v", err)
			return took
		}
		_ = mod.Close(context.Background())
		return took
	}

	instantiate() // Whatever the runtime caches on first use is not what a redirect pays.

	const each = 8
	took := make([][]time.Duration, maxConcurrentRoutes)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range maxConcurrentRoutes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				took[i] = append(took[i], instantiate())
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	var total, worst time.Duration
	var n int
	for _, run := range took {
		for _, d := range run {
			total += d
			n++
			if d > worst {
				worst = d
			}
		}
	}
	if n == 0 {
		t.Fatal("nothing was measured")
	}
	mean := total / time.Duration(n)
	t.Logf("instantiation with %d in flight costs mean %s, worst of %d %s "+
		"(%d instantiations in %s of wall clock); the shipped bound is %s",
		maxConcurrentRoutes, mean.Round(time.Microsecond), n,
		worst.Round(time.Microsecond), n, wall.Round(time.Millisecond),
		DefaultInstantiateDeadline)

	if raceDetector {
		// Reported, not asserted, exactly as the invocation measurement is: the
		// detector costs this an order of magnitude, and `make check` runs with it.
		// What checks the shipped bound is a plain `go test`, and this says so when
		// it cannot.
		t.Log("built with -race, so the comparison against the shipped bound is " +
			"skipped: the detector's own cost is most of the number above")
		return
	}
	if worst >= DefaultInstantiateDeadline {
		t.Errorf("instantiating under contention took %s against a shipped bound of "+
			"%s, so a busy instance kills add-ons before their code runs — which is "+
			"F326 with a different number", worst, DefaultInstantiateDeadline)
	}
}

// --- the observe class ---------------------------------------------------------

// m66.md's first bullet, other half: the observe class *can delay nothing*.
//
// Asserted as the property rather than as a duration — a timing assertion on a
// shared machine is a flake — by handing over an event the worker will spend the
// whole deadline on and measuring the call that hands it over.
func TestObservingCannotDelayAnything(t *testing.T) {
	h, _, _ := redirectHostBounded(t, "slow", time.Minute, testInstantiateDeadline,
		PermissionRedirectObserve)
	if got := h.ObservingAddons(); len(got) != 1 {
		t.Fatalf("the observe class carries %v", got)
	}
	start := time.Now()
	for range observeQueueDepth + 8 {
		h.Observe(RedirectEvent{LinkID: "l"})
	}
	// The queue is bounded and full sends drop, so this loop cannot block whatever
	// the worker is doing. A second is orders of magnitude more than the send costs
	// and orders of magnitude less than the minute the worker is going to spend.
	if took := time.Since(start); took > time.Second {
		t.Fatalf("handing %d observations over took %s; the pipeline waited on an add-on",
			observeQueueDepth+8, took)
	}
}

// The class works, which the test above deliberately does not show: an observing
// module is handed the redirect's record and can read it.
func TestAnObservingModuleIsHandedTheRecordedRedirect(t *testing.T) {
	h, sink, metrics := redirectHost(t, PermissionRedirectObserve, PermissionStorageOwnSchema)
	h.Observe(RedirectEvent{
		LinkID: "11111111-1111-4111-8111-111111111111", Country: "SE",
		Browser: "Firefox", IsBot: true,
	})
	// The worker is a goroutine, so the assertion waits for it rather than assuming
	// it has run — and it waits by *polling for the answer* rather than by closing
	// the host, which is a real distinction: Close cancels an invocation in flight,
	// so joining that way would race the very work being asserted about.
	logs := waitFor(t, sink, "redirect: observe_storage_query=")
	for _, want := range []string{
		"redirect: observed_link=11111111-1111-4111-8111-111111111111",
		"redirect: observed_country=SE",
		"redirect: observed_browser=Firefox",
		"redirect: observed_bot=true",
		// The other class's payload, from this one. **Denied**, and by the grant
		// rather than by the placement: this manifest declared `redirect.observe`
		// and not `redirect.inline`, and the permission check comes before
		// everything else — so an observing add-on cannot reach the inline class's
		// functions even in the invocation where they would have nothing to say.
		// The other answer, for a module holding both, is the one the test below
		// drives.
		"redirect: observe_decision_read=denied",
		"redirect: observe_answer_write=denied",
		// **Storage is the difference between the two classes.** The same call is
		// denied inside an inline invocation and reaches the host here — where it
		// fails only because this host has no database, which is what internal means.
		"redirect: observe_storage_query=internal",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the fixture did not report %q\n%s", want, logs)
		}
	}
	// **Polled for the same reason the log lines are**, and it is a distinct wait:
	// the marker above is written *inside* the guest call and the histogram is
	// observed after it, so the log arriving never implied the metric had. The gap
	// was microseconds until M66.5 put the instance's reset between the two, and a
	// test that reads the scrape the instant the marker lands measures that gap.
	series := `linkctrl_addon_redirect_duration_seconds_count{addon="redirect",class="observe"} 1`
	deadline := time.Now().Add(10 * time.Second)
	for {
		scraped := scrape(t, metrics)
		if strings.Contains(scraped, series) {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("the scrape does not carry %s\n%s", series,
				seriesLike(scraped, "linkctrl_addon_redirect_duration_seconds"))
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The same module holding **both** classes, which is where the inline functions
// stop being refused by the grant and start being refused by the placement: there
// is no decision inside an observing invocation, so the read is not-found and the
// answer has nowhere to go.
//
// It is the pair of the test above and it is what makes that one's `denied` mean
// *the grant* rather than *the class*.
func TestAModuleHoldingBothClassesIsStillOnlyInOneAtATime(t *testing.T) {
	h, sink, _ := redirectHost(t, PermissionRedirectObserve, PermissionRedirectInline)
	h.Observe(RedirectEvent{LinkID: "l"})
	logs := waitFor(t, sink, "redirect: observe_answer_write=")
	for _, want := range []string{
		"redirect: observe_decision_read=not_found",
		"redirect: observe_answer_write=not_found",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the fixture did not report %q\n%s", want, logs)
		}
	}
	// And the mirror, inside an inline invocation: the event read is refused by the
	// *placement* — it is not in the redirect-safe subset — even though this add-on
	// holds redirect.observe.
	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	if got := sink.String(); !strings.Contains(got, "redirect: inline_event_read=denied") {
		t.Errorf("an inline invocation reached the observe class's read\n%s", got)
	}
}

// --- egress, and the two places a redirect class meets it ---------------------

// m68.5.md: *egress is refused on the redirect path*, and *a test that a refused
// path refuses*. The refusal is real in both classes and it is **not the same
// refusal**, which is what this test exists to pin down — the third review of this
// milestone found seven places describing one where there are two.
//
// Driven from the guest, and that is the whole point. `TestOnlyARouteInvocationMayFetch`
// calls [hostState.doFetch] directly, which is *below* the dispatch gate, so it
// cannot see that an inline invocation never arrives there at all. Only a module
// that made the call can report what it was told.
//
//   - **Inline**: `network_fetch` is outside abi.InlineSafe, so dispatch refuses
//     it before any of fetch.go runs and the guest gets ErrDenied — the same
//     refusal `storage_query` gets one line above it, and deliberately
//     indistinguishable from the undeclared-permission one (M66). Uncounted, also
//     deliberately: that is the redirect hot path and the debug log is the record.
//   - **Observe**: not inline, so it reaches the function and is refused by the
//     class inside it. The guest gets a FetchResponse saying `class_refused` and
//     the operator gets the counter.
func TestNeitherRedirectClassMayFetchAndTheGuestIsToldSo(t *testing.T) {
	h, sink, metrics := redirectHost(t, grantable()...)

	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	if got := sink.String(); !strings.Contains(got, "redirect: inline_network_fetch=denied") {
		t.Errorf("an inline invocation did not report ErrDenied for network_fetch\n%s", got)
	}
	// The counter an inline refusal does *not* write. Asserted rather than assumed,
	// because docs/operations.md tells an operator what this series means and a row
	// nobody can produce is a row that means something else.
	if series := seriesLike(scrape(t, metrics), "linkctrl_addon_fetch_total"); series != "" {
		t.Errorf("an inline refusal reached the fetch counter, which is off by design "+
			"on the redirect path:\n%s", series)
	}

	h.Observe(RedirectEvent{LinkID: "l"})
	logs := waitFor(t, sink, "redirect: observe_network_fetch=")
	if !strings.Contains(logs, "redirect: observe_network_fetch=outcome:class_refused") {
		t.Errorf("an observing invocation did not get the class_refused record\n%s", logs)
	}
	// Polled for the reason the histogram above is: the guest's log line is written
	// inside the call and the counter after it.
	series := `linkctrl_addon_fetch_total{addon="redirect",outcome="class_refused"} 1`
	deadline := time.Now().Add(10 * time.Second)
	for {
		scraped := scrape(t, metrics)
		if strings.Contains(scraped, series) {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("the scrape does not carry %s\n%s", series,
				seriesLike(scraped, "linkctrl_addon_fetch_total"))
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The grant is still checked first, which is what makes the `class_refused` above
// mean *the class* rather than *the manifest*. A module that never declared
// `network.fetch` gets ErrDenied in the observing class too, and learns nothing
// about whether this host implements the function at all.
func TestAnObservingModuleWithoutTheGrantIsRefusedBeforeTheClassIsReached(t *testing.T) {
	h, sink, metrics := redirectHost(t, PermissionRedirectObserve)
	h.Observe(RedirectEvent{LinkID: "l"})
	logs := waitFor(t, sink, "redirect: observe_network_fetch=")
	if !strings.Contains(logs, "redirect: observe_network_fetch=denied") {
		t.Errorf("a module that did not declare network.fetch was not refused by the grant\n%s", logs)
	}
	// An undeclared call is counted on the refusals series and not on the fetch
	// one: it never reached the fetch machinery.
	if series := seriesLike(scrape(t, metrics), "linkctrl_addon_fetch_total"); series != "" {
		t.Errorf("a grant refusal reached the fetch counter:\n%s", series)
	}
}

// Nothing declared the grant, so there is no queue and no goroutine — the same
// "off costs nothing" the whole subsystem is held to.
func TestNoObserverMeansNoQueue(t *testing.T) {
	h, _, metrics := redirectHost(t, PermissionRedirectInline)
	if h.observe.Load() != nil {
		t.Error("a host with no observing add-on built an observation queue")
	}
	h.Observe(RedirectEvent{LinkID: "l"})
	if series := seriesLike(scrape(t, metrics), "linkctrl_rate_limited_total"); series != "" {
		t.Errorf("observing with nothing installed counted a drop: %s", series)
	}
}

// --- what the host will do to a URL --------------------------------------------

// D317 asked for this in as many words: *an assertion that the rewritten URL
// differs from the decided one in its query and in nothing else, so the bound is
// enforced by the host rather than trusted to the module.*
//
// It is a unit test over the substitution rather than a drive of the fixture,
// because what has to be shown is that **no query a module could write** reaches
// past the query — and a fixture can only try the ones somebody thought of.
func TestSubstitutingAQueryReachesNothingElseAboutTheURL(t *testing.T) {
	const dest = "https://user:pw@shop.example.test:8443/a%2Fb/c?old=1"
	for _, query := range []string{
		"", "a=1", "a=1&b=2", "just-a-string", "a=%2F%3A%40", "?=?", "a=1&a=2",
		// The shapes that would reach past the query if the host were building a
		// string rather than substituting into a parsed URL.
		"a=1&b=//evil.test", "a=1/../../x", "@evil.test", "//evil.test",
	} {
		got, err := applyQuery(dest, query)
		if err != nil {
			t.Errorf("applyQuery(%q) refused: %v", query, err)
			continue
		}
		if !strings.HasPrefix(got, "https://user:pw@shop.example.test:8443/a%2Fb/c") {
			t.Errorf("query %q moved the URL off its origin and path: %q", query, got)
		}
		want := "https://user:pw@shop.example.test:8443/a%2Fb/c"
		if query != "" {
			want += "?" + query
		}
		if got != want {
			t.Errorf("applyQuery(%q) = %q, want %q", query, got, want)
		}
	}

	// The one case where *the query* and *the query string* differ: a destination
	// ending in a bare `?` parses with an empty query and a flag saying the `?` was
	// there. A module asking for the query to be removed means the `?` too — left
	// alone, an empty rewrite would produce the URL it was handed and the module
	// would be told it changed something it did not.
	got, err := applyQuery("https://shop.example.test/p?", "")
	if err != nil {
		t.Fatalf("removing the query from a bare-? URL: %v", err)
	}
	if want := "https://shop.example.test/p"; got != want {
		t.Errorf("removing the query gave %q, want %q", got, want)
	}
}

// The characters a query may not hold, refused at the record rather than at the
// substitution — so a module learns from the call it made.
func TestAQueryOutsideTheGrammarIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		ok          bool
	}{
		{"a fragment", "a=1#b", false},
		{"a space", "a=1 b", false},
		{"a newline", "a=1\nb=2", false},
		{"a control character", "a=\x00", false},
		{"a raw non-ASCII byte", "a=é", false},
		{"percent-encoded anything", "a=%C3%A9", true},
		{"the sub-delims", "a=!$&'()*+,;=:@/?", true},
		{"empty", "", true},
	} {
		got := validQuery(tc.query)
		if got != tc.ok {
			t.Errorf("%s: validQuery(%q) = %v, want %v", tc.name, tc.query, got, tc.ok)
		}
	}
}

// Every bound on the answer record, checked where a module learns about it.
func TestWhatARedirectAnswerMayCarry(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantErr   bool
	}{
		{"empty", `{}`, false},
		{"allow", `{"verdict":"allow"}`, false},
		{"veto", `{"verdict":"veto"}`, false},
		{"a rewrite", `{"rewrite":true,"query":"a=1"}`, false},
		{"dropping the query", `{"rewrite":true}`, false},
		{"a verdict outside the vocabulary", `{"verdict":"maybe"}`, true},
		// The two halves of the rewrite flag disagreeing. A module that wrote a query
		// and did not ask for a rewrite believes it changed something and did not.
		{"a query with no rewrite", `{"query":"a=1"}`, true},
		{"a field this host does not know", `{"verdict":"allow","reason":"because"}`, true},
		{"two records", `{}{}`, true},
		{"not an object", `"veto"`, true},
		{"a query past the bound", `{"rewrite":true,"query":"` + strings.Repeat("a", maxRedirectQuery+1) + `"}`, true},
	} {
		_, err := decodeRedirectAnswer([]byte(tc.raw))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: decodeRedirectAnswer(%s) error = %v, want error %v",
				tc.name, tc.raw, err, tc.wantErr)
		}
	}
}

// --- what an invocation costs ----------------------------------------------------

// What one inline invocation costs end to end, measured rather than inherited.
//
// **It was TestAnInlineInvocationCostsAnInstantiation until M66.5**, and the
// rename is the milestone: an invocation no longer builds an instance, so the old
// name asserted the thing that was removed. What is left in the number is the
// guest's own code plus the host resetting its memory before the instance goes
// back to the pool, which [TestResettingAPooledInstanceIsCheaperThanBuildingOne]
// separates out.
//
// The comparison is still against [DefaultInlineDeadline], because that is the
// number this default is measured into, and it is still skipped under `-race` for
// the reason racecost_test.go gives.
func TestAnInlineInvocationCostsWhatTheGuestDoes(t *testing.T) {
	if testing.Short() {
		t.Skip("a timing measurement")
	}
	h, _, _ := redirectHost(t, PermissionRedirectInline, PermissionRewriteQuery, "config.read")
	d := decisionFor("strip", "https://shop.example.test/item/42?utm_source=x&id=7")

	// One outside the measurement: the first invocation of a compiled module pays
	// for whatever the runtime caches, and that is not what a redirect at rate pays.
	h.Inline(t.Context(), d)

	const runs = 20
	var worst time.Duration
	start := time.Now()
	for range runs {
		at := time.Now()
		if got := h.Inline(t.Context(), d); !got.Rewritten {
			t.Fatalf("the module stopped answering, so this measured the wrong thing: %+v", got)
		}
		if took := time.Since(at); took > worst {
			worst = took
		}
	}
	mean := time.Since(start) / runs
	t.Logf("an inline invocation costs mean %s, worst of %d %s; the shipped deadline is %s",
		mean.Round(time.Microsecond), runs, worst.Round(time.Microsecond), DefaultInlineDeadline)

	if raceDetector {
		// Reported, not asserted. The detector costs this measurement an order of
		// magnitude — ~3 ms plainly against ~20 ms here, and past the deadline when
		// the whole package runs in parallel around it — and D225 already records
		// the same effect on the load path. What is skipped is the comparison, never
		// the measurement.
		t.Log("built with -race, so the comparison against the shipped deadline is " +
			"skipped: the detector's own cost is most of the number above")
		return
	}
	if worst >= DefaultInlineDeadline {
		t.Errorf("a module doing ordinary work took %s against a shipped deadline of %s, "+
			"so the default kills add-ons that are working; the number is measured into "+
			"and this is the measurement", worst, DefaultInlineDeadline)
	}
}

// waitFor blocks until the sink holds the marker, and fails if it never does.
//
// The observe class is asynchronous by construction, so a test of it either waits
// or asserts nothing. Polling rather than a channel because what is being watched
// is a log sink a fixture writes through the ABI, which is the only report a guest
// can make — and the marker is the *last* line the fixture writes, so seeing it
// means the lines before it are there too.
func waitFor(t *testing.T, sink *logSink, marker string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if logs := sink.String(); strings.Contains(logs, marker) {
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the observing add-on never reported %q\n%s", marker, sink.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// PermissionStorageOwnSchema is abi.PermissionStorage, named here so the tests in
// this file read as a list of grants rather than as one grant and a qualified
// constant.
const PermissionStorageOwnSchema = abi.PermissionStorage
