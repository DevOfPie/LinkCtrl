package ratelimit

import (
	"testing"
	"time"
)

// An open breaker lets exactly one request through per cooldown.
//
// The comment on this type promised "an outage costs the timeout a few times and
// then nothing until the cooldown lets one request through to check", and the
// code read openUntil without writing it — so every concurrent request was let
// through the moment the cooldown lapsed, each paying the full read timeout
// (F123). At the shipped 50ms that is a one per cent duty cycle and cosmetic; at
// a timeout an operator has raised, which the documentation invites, it is the
// herd the comment said could not happen.
func TestAnOpenBreakerAdmitsOneProbePerCooldown(t *testing.T) {
	now := time.Now()
	b := &breaker{nowOverride: func() time.Time { return now }}

	for range breakerThreshold {
		b.recordFailure()
	}
	if b.allow() {
		t.Fatal("the breaker is not open after the failure threshold; this test is " +
			"asserting nothing")
	}

	// The cooldown lapses. Exactly one caller gets through, and the rest are
	// refused without touching Redis.
	now = now.Add(breakerCooldown + time.Millisecond)
	admitted := 0
	for range 20 {
		if b.allow() {
			admitted++
		}
	}
	if admitted != 1 {
		t.Errorf("%d of 20 requests were admitted after the cooldown lapsed, want 1. "+
			"Each one pays the full read timeout against a server that is still "+
			"down, which is the herd the breaker exists to prevent", admitted)
	}

	// A failed probe keeps it shut for another cooldown, without having to reach
	// the failure threshold again.
	b.recordFailure()
	if b.allow() {
		t.Error("a failed probe left the breaker open to the next caller")
	}

	// And a probe that succeeds reopens the path entirely.
	now = now.Add(breakerCooldown + time.Millisecond)
	if !b.allow() {
		t.Fatal("the second cooldown admitted nobody")
	}
	b.succeed()
	for range 5 {
		if !b.allow() {
			t.Error("the breaker is still refusing after a successful probe")
		}
	}
}
