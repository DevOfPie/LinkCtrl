package observability

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// stubLimiter reports fixed bookkeeping.
type stubLimiter struct {
	keys      int
	overflows int64
	fallbacks int64
}

func (s stubLimiter) Len() int         { return s.keys }
func (s stubLimiter) Overflows() int64 { return s.overflows }
func (s stubLimiter) Fallbacks() int64 { return s.fallbacks }

// A healthy shared limiter is distinguishable from an idle one.
//
// The tracked-keys gauge cannot tell them apart and never could: a shared
// limiter's local table is only written when Redis did not answer, so on a
// working instance it reads zero — which is what the gauge also reads when
// nothing is happening at all. The runbook's row for "the shared limit fell
// back" pointed at a rate that "stays unchanged", which is a description and not
// an expression (F102).
//
// The fallback counter is the series that answers it, and it is a counter rather
// than a gauge on purpose: it is monotonic, so a threshold on the value latches
// forever after one transient blip, and only a rate is readable.
func TestTheFallbackCounterDistinguishesSharedFromFallenBack(t *testing.T) {
	c := NewLimiterCollector(map[string]LimiterStats{
		// Healthy and shared: nothing local, nothing fallen back.
		"login": stubLimiter{keys: 0, fallbacks: 0},
		// Fallen back: this replica has been deciding on its own.
		"api": stubLimiter{keys: 42, fallbacks: 17},
	})

	got := testutil.CollectAndCount(c, "linkctrl_rate_limit_fallback_total")
	if got != 2 {
		t.Fatalf("collected %d fallback series, want one per limiter", got)
	}

	want := `
# HELP linkctrl_rate_limit_fallback_total Decisions made from this replica's own buckets because the shared limiter did not answer. Movement means the configured limit is being enforced per replica rather than across them.
# TYPE linkctrl_rate_limit_fallback_total counter
linkctrl_rate_limit_fallback_total{limit="api"} 17
linkctrl_rate_limit_fallback_total{limit="login"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"linkctrl_rate_limit_fallback_total"); err != nil {
		t.Errorf("the fallback series is not what the runbook alerts on: %v", err)
	}

	// And the gauge still reads zero for the healthy shared limiter, which is
	// the ambiguity this counter exists beside rather than replaces.
	if err := testutil.CollectAndCompare(c, strings.NewReader(`
# HELP linkctrl_rate_limit_tracked_keys Client keys currently tracked by each limiter.
# TYPE linkctrl_rate_limit_tracked_keys gauge
linkctrl_rate_limit_tracked_keys{limit="api"} 42
linkctrl_rate_limit_tracked_keys{limit="login"} 0
`), "linkctrl_rate_limit_tracked_keys"); err != nil {
		t.Errorf("tracked keys: %v", err)
	}
}
