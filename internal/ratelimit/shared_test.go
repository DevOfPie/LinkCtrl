package ratelimit

import (
	"testing"
	"time"
)

// Every request refused by an open breaker is a fallback, and the counter must
// say so.
//
// take answering "not answered" is what sends the caller to its local bucket,
// so during an outage every breaker-refused request is a locally decided one.
// When only the dispatched calls counted — the failures that opened the breaker,
// then one probe per cooldown — the counter's rate during an outage was a fixed
// trickle per limiter per replica, independent of traffic. The runbook's rate()
// alert still fired off those probes; what was wrong was the magnitude: an
// operator dividing the fallback rate by the request rate to size the
// degradation was dividing by noise.
func TestABreakerOpenTakeCountsAsAFallback(t *testing.T) {
	now := time.Now()
	s := &Shared{}
	s.breaker.nowOverride = func() time.Time { return now }

	for range breakerThreshold {
		s.breaker.recordFailure()
	}
	if s.breaker.allow() {
		t.Fatal("the breaker is not open after the failure threshold; this test " +
			"would dispatch real Redis calls and assert nothing")
	}

	before := s.Fallbacks()
	const n = 7
	for range n {
		if _, _, answered := s.take(1, 1, 1, time.Minute, "k"); answered {
			t.Fatal("an open breaker dispatched a Redis call")
		}
	}
	if got := s.Fallbacks() - before; got != n {
		t.Errorf("Fallbacks() grew by %d over %d breaker-open requests, want %d: "+
			"each one was decided from the local bucket, and a counter that only "+
			"moves on dispatched calls reports an outage at a fixed trickle "+
			"whatever the traffic", got, n, n)
	}
}
