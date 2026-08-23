package addon

import (
	"context"
	"log/slog"
	"strings"
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
	return redirectHostNamed(t, "redirect", testInlineDeadline, permissions...)
}

// testInlineDeadline is the budget every test in this file that is **not** about
// the deadline runs with, and it is deliberately not [DefaultInlineDeadline].
//
// The shipped default is 25 ms, which is roughly six times what an invocation
// costs on this machine — and the race detector costs this measurement an order of
// magnitude (D225's effect, on the invocation path). Under `-race`, with the rest
// of this package running in parallel, an ordinary invocation reaches and passes
// 25 ms, so a test asserting what a module *answered* would fail because the host
// correctly killed it. That is the deadline working, reported as a broken rewrite.
//
// So the behaviour tests buy room and the deadline tests set their own. What holds
// the shipped default honest is [TestAnInlineInvocationCostsAnInstantiation],
// which measures rather than assumes and says so when the detector makes the
// comparison meaningless.
const testInlineDeadline = 10 * time.Second

func redirectHostNamed(t *testing.T, module string, deadline time.Duration,
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
		Dir:            dir,
		Metrics:        metrics,
		Logger:         slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
		InlineDeadline: deadline,
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
	if strings.Contains(scraped, `linkctrl_addon_redirect_kills_total{addon="redirect"} 1`) {
		t.Error("a module that answered on time was counted as killed")
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
	h, sink, metrics := redirectHostNamed(t, "slow", 50*time.Millisecond,
		PermissionRedirectInline, PermissionRewriteQuery)

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
	if want := `linkctrl_addon_redirect_kills_total{addon="slow"} 1`; !strings.Contains(scraped, want) {
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
	if want := `linkctrl_addon_redirect_kills_total{addon="slow"} 2`; !strings.Contains(scrape(t, metrics), want) {
		t.Errorf("a second overrun was not counted; a thrashing module has to stay visible")
	}
}

// A redirect whose own request was cancelled is **not** an add-on overrunning, and
// counting it as one would put a number on the Add-on manager that blames the
// wrong party.
func TestAVisitorLeavingIsNotAnAddonOverrun(t *testing.T) {
	h, _, metrics := redirectHostNamed(t, "slow", time.Minute, PermissionRedirectInline)
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

// --- the observe class ---------------------------------------------------------

// m66.md's first bullet, other half: the observe class *can delay nothing*.
//
// Asserted as the property rather than as a duration — a timing assertion on a
// shared machine is a flake — by handing over an event the worker will spend the
// whole deadline on and measuring the call that hands it over.
func TestObservingCannotDelayAnything(t *testing.T) {
	h, _, _ := redirectHostNamed(t, "slow", time.Minute, PermissionRedirectObserve)
	if got := h.ObservingAddons(); len(got) != 1 {
		t.Fatalf("the observe class carries %v", got)
	}
	start := time.Now()
	for range observeQueue + 8 {
		h.Observe(RedirectEvent{LinkID: "l"})
	}
	// The queue is bounded and full sends drop, so this loop cannot block whatever
	// the worker is doing. A second is orders of magnitude more than the send costs
	// and orders of magnitude less than the minute the worker is going to spend.
	if took := time.Since(start); took > time.Second {
		t.Fatalf("handing %d observations over took %s; the pipeline waited on an add-on",
			observeQueue+8, took)
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
	series := `linkctrl_addon_redirect_duration_seconds_count{addon="redirect",class="observe"} 1`
	if scraped := scrape(t, metrics); !strings.Contains(scraped, series) {
		t.Errorf("the scrape does not carry %s\n%s", series,
			seriesLike(scraped, "linkctrl_addon_redirect_duration_seconds"))
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

// Nothing declared the grant, so there is no queue and no goroutine — the same
// "off costs nothing" the whole subsystem is held to.
func TestNoObserverMeansNoQueue(t *testing.T) {
	h, _, metrics := redirectHost(t, PermissionRedirectInline)
	if h.observe != nil {
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

// The measurement m66.md asks for and the number the deadline's default is chosen
// against: *wazero invocation overhead at 2,000 rps is unmeasured until now (M60
// measured instantiation, not steady-state call cost)*.
//
// It reports rather than asserts a budget, for the reason M60's instantiation
// measurement does: a threshold on a shared machine is a flake, and what this has
// to keep true is that the number is **taken** rather than guessed at. The
// assertion is against [DefaultInlineDeadline], which is the thing a wrong number
// would break — if a module doing ordinary work cannot finish inside the shipped
// default, the default is wrong and this fails.
func TestAnInlineInvocationCostsAnInstantiation(t *testing.T) {
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
