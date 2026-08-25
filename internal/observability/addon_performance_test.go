package observability

import (
	"testing"
	"time"
)

// What the Add-on manager reads back off this registry (M68).
//
// The page renders per-module p99 and kill counts as values rather than as a link
// to `/metrics`, which is the checkable form of the owner's "attribution without
// Prometheus". These are the arithmetic behind that: the estimate has to be the
// one `histogram_quantile` makes, or the number on the page and the number on a
// dashboard are two answers to one question.

// A module that has never run on the redirect path is **absent**, not zero.
//
// This is what m68.md's "modules holding no redirect grant show no redirect
// figures rather than zeros" rests on. A zero p99 would be indistinguishable from
// a very fast add-on, which is the reading an operator would act on.
func TestAnUnobservedAddonHasNoPerformanceAtAll(t *testing.T) {
	m := NewMetrics()
	m.ObserveAddonRedirect("clickstats", "observe", 800*time.Microsecond)

	perf := m.AddonPerformance()
	if _, ok := perf["never-ran"]; ok {
		t.Error("an add-on with no observations has an entry; the page would draw " +
			"zeros over a module that has never run")
	}
	seen, ok := perf["clickstats"]
	if !ok || !seen.Observed() {
		t.Fatalf("the add-on that did run is absent from %v", perf)
	}
	if len(seen.Classes) != 1 || seen.Classes[0].Class != "observe" {
		t.Errorf("classes are %v; a class with no observations must not appear either",
			seen.Classes)
	}
	if seen.Classes[0].Count != 1 {
		t.Errorf("the count behind the estimate is %d, want 1", seen.Classes[0].Count)
	}
}

// The two kill steps are counted apart and added together for the list column.
//
// F326's split: a kill at `call` is the add-on holding a redirect open past its
// deadline, and a kill at `instantiate` is this host failing to start the module.
// The detail page shows both because they have different owners; the list has one
// column, so it shows the sum.
func TestKillsAreSplitByStepAndSummedForTheList(t *testing.T) {
	m := NewMetrics()
	m.ObserveAddonRedirectKill("hostile", "call")
	m.ObserveAddonRedirectKill("hostile", "call")
	m.ObserveAddonRedirectKill("hostile", "instantiate")

	k := m.AddonPerformance()["hostile"].Kills
	if k.Call != 2 || k.Instantiate != 1 || k.Total() != 3 {
		t.Errorf("kills are call=%d instantiate=%d total=%d, want 2/1/3",
			k.Call, k.Instantiate, k.Total())
	}
	// A module killed at every attempt has no duration observations at all — a
	// killed invocation is deliberately absent from the histogram — and it still
	// has to appear on the page, because it is the module an operator is looking
	// for.
	if !m.AddonPerformance()["hostile"].Observed() {
		t.Error("an add-on that was only ever killed reads as unobserved, which is " +
			"the one add-on the manager exists to surface")
	}
}

// Inline sorts before observe, so a page does not reorder its own rows between
// renders — a map's iteration order would.
func TestClassesAreOrderedInlineFirst(t *testing.T) {
	m := NewMetrics()
	m.ObserveAddonRedirect("both", "observe", time.Millisecond)
	m.ObserveAddonRedirect("both", "inline", time.Millisecond)

	classes := m.AddonPerformance()["both"].Classes
	if len(classes) != 2 || classes[0].Class != "inline" {
		t.Errorf("classes are %v; inline is the one that costs a visitor and reads first",
			classes)
	}
}

// The p99 is Prometheus's own interpolation, checked against a distribution whose
// answer can be worked out by hand.
//
// A hundred observations, ninety-nine of them at 0.5 ms and one at 25 ms. The
// rank is 99, which lands in the bucket that closes at 0.0005 — where the
// cumulative count first reaches 99 — so the estimate interpolates inside that
// bucket rather than being dragged up by the outlier. That is the property worth
// pinning: a single slow invocation must not move a p99 over a hundred, exactly as
// it would not on a dashboard.
func TestTheP99IsTheEstimateAHistogramSupports(t *testing.T) {
	m := NewMetrics()
	for range 99 {
		m.ObserveAddonRedirect("steady", "inline", 500*time.Microsecond)
	}
	m.ObserveAddonRedirect("steady", "inline", 25*time.Millisecond)

	got := m.AddonPerformance()["steady"].Classes[0].P99
	if got < 400*time.Microsecond || got > 600*time.Microsecond {
		t.Errorf("p99 is %v; ninety-nine observations at 500µs and one at 25ms put "+
			"the ninety-ninth-percentile rank inside the 500µs bucket", got)
	}
}

// The interpolation itself, on a distribution where it differs visibly from the
// bucket's own boundary.
//
// Ninety observations in the first bucket and ten at 900 ms. The rank is 99, which
// falls in the bucket closing at 1 s with ninety already below it and ten inside,
// so the estimate is 0.25 + 0.75 × 9/10 = 925 ms — not the 1 s boundary, and not
// the 900 ms the slow observations actually took. That is what an interpolated
// quantile is, and pinning it here is what stops the arithmetic being quietly
// replaced by "the bucket the rank landed in", which agrees on most distributions
// and is wrong on this one.
func TestTheP99InterpolatesInsideItsBucket(t *testing.T) {
	m := NewMetrics()
	for range 90 {
		m.ObserveAddonRedirect("bimodal", "inline", 50*time.Microsecond)
	}
	for range 10 {
		m.ObserveAddonRedirect("bimodal", "inline", 900*time.Millisecond)
	}
	got := m.AddonPerformance()["bimodal"].Classes[0].P99
	if got < 920*time.Millisecond || got > 930*time.Millisecond {
		t.Errorf("p99 is %v, want about 925ms — 0.25s plus nine tenths of the way to "+
			"the 1s boundary, which is what histogram_quantile answers here", got)
	}
}

// A rank that lands past the last finite bucket answers that boundary rather than
// a figure the data does not support.
//
// The buckets stop at one second. An add-on every one of whose invocations took
// longer has nothing above to interpolate towards, so the honest answer is the
// last boundary — and the manager renders it as such rather than printing an
// invented number.
func TestAP99PastTheLastBucketAnswersItsBoundary(t *testing.T) {
	m := NewMetrics()
	for range 10 {
		m.ObserveAddonRedirect("glacial", "inline", 30*time.Second)
	}
	got := m.AddonPerformance()["glacial"].Classes[0].P99
	if got != time.Second {
		t.Errorf("p99 is %v, want the last finite boundary of 1s; anything else is a "+
			"number interpolated against no upper bound", got)
	}
}

// A nil *Metrics answers nil, because the CLI and half the tests in this
// repository build routers without one and a page must not have to ask.
func TestNilMetricsHaveNoAddonPerformance(t *testing.T) {
	var m *Metrics
	if got := m.AddonPerformance(); got != nil {
		t.Errorf("a nil registry answered %v", got)
	}
}
