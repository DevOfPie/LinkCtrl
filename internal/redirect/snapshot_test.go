package redirect

import (
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

var testNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

func TestDecideCoversEveryState(t *testing.T) {
	past := testNow.Add(-time.Hour)
	future := testNow.Add(time.Hour)

	cases := []struct {
		name string
		snap *Snapshot
		want Outcome
	}{
		{"nil snapshot", nil, OutcomeNotFound},
		{"negative entry", notFoundSnapshot(), OutcomeNotFound},
		{"active", &Snapshot{Status: "active"}, OutcomeRedirect},
		{"active with future expiry", &Snapshot{Status: "active", ExpiresAt: &future}, OutcomeRedirect},

		// Two roads to Gone, deliberately: a timestamp in the past decides 410
		// even while the rollup has not yet flipped the status column, and a
		// status of "expired" decides 410 even without the timestamp.
		{"expiry passed, status not yet flipped", &Snapshot{Status: "active", ExpiresAt: &past}, OutcomeGone},
		{"expiry is exactly now", &Snapshot{Status: "active", ExpiresAt: &testNow}, OutcomeGone},
		{"status expired", &Snapshot{Status: "expired"}, OutcomeGone},

		// Archived and disabled are 404, not 410 or 403: telling a scanner an
		// alias exists but is switched off is information it has no use for.
		{"archived", &Snapshot{Status: "archived"}, OutcomeNotFound},
		{"disabled", &Snapshot{Status: "disabled"}, OutcomeNotFound},
		{"unknown status string", &Snapshot{Status: "surprise"}, OutcomeNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snap.Decide(testNow); got != tc.want {
				t.Errorf("Decide() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCacheTTLClampsToExpiry(t *testing.T) {
	base := 24 * time.Hour
	negative := time.Minute

	t.Run("no expiry uses the base TTL", func(t *testing.T) {
		s := &Snapshot{Status: "active"}
		if got := s.CacheTTL(testNow, base, negative); got != base {
			t.Errorf("CacheTTL = %v, want %v", got, base)
		}
	})

	t.Run("soon-expiring link is clamped", func(t *testing.T) {
		// The production bug this prevents: cached for 24h, expiring in five
		// minutes, served for hours past its expiry.
		at := testNow.Add(5 * time.Minute)
		s := &Snapshot{Status: "active", ExpiresAt: &at}
		if got := s.CacheTTL(testNow, base, negative); got != 5*time.Minute {
			t.Errorf("CacheTTL = %v, want the 5m to expiry", got)
		}
	})

	t.Run("already-expired link still caches for the 1s floor", func(t *testing.T) {
		// Without the floor, an expired link's TTL is negative, nothing caches
		// it, and every request for it becomes a database query — an expired
		// link that was popular stays expensive forever.
		at := testNow.Add(-time.Hour)
		s := &Snapshot{Status: "active", ExpiresAt: &at}
		if got := s.CacheTTL(testNow, base, negative); got != time.Second {
			t.Errorf("CacheTTL = %v, want the 1s floor", got)
		}
	})

	t.Run("negative entries use the negative TTL", func(t *testing.T) {
		if got := notFoundSnapshot().CacheTTL(testNow, base, negative); got != negative {
			t.Errorf("CacheTTL = %v, want %v", got, negative)
		}
		var nilSnap *Snapshot
		if got := nilSnap.CacheTTL(testNow, base, negative); got != negative {
			t.Errorf("nil CacheTTL = %v, want %v", got, negative)
		}
	})
}

// The wire format is part of the cache contract: a value written by this
// version must read back identically, and the negative flag must survive.
func TestSnapshotRoundTrips(t *testing.T) {
	at := testNow.Add(time.Hour)
	max := int64(5)
	in := &Snapshot{
		LinkID: [16]byte{1}, WorkspaceID: [16]byte{2},
		URL: "https://example.com/x", Status: "active",
		ExpiresAt: &at, ForwardQuery: true, HasPassword: true,
		MaxClicks: &max, OneTime: true,
	}
	b, err := in.encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	if *out.ExpiresAt != *in.ExpiresAt {
		t.Errorf("ExpiresAt did not survive: %v != %v", out.ExpiresAt, in.ExpiresAt)
	}
	out.ExpiresAt, in.ExpiresAt = nil, nil
	if *out.MaxClicks != *in.MaxClicks {
		t.Errorf("MaxClicks did not survive")
	}
	out.MaxClicks, in.MaxClicks = nil, nil
	if *out != *in {
		t.Errorf("round trip changed the snapshot: %+v != %+v", out, in)
	}

	neg, err := decodeSnapshot([]byte(`{"n":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !neg.NotFound || neg.Decide(testNow) != OutcomeNotFound {
		t.Error("a negative entry did not decode as not-found")
	}
}

// TestBotPolicySurvivesTheWire is the claim M32.5 made when it added two fields
// without bumping CacheKeyVersion.
//
// Two halves, and the second is the one that matters. A payload written by the
// previous build has neither field, and it must decode as "no blocking" — that
// is what makes the omitted version bump safe, and if it ever stopped being
// true, a rolling upgrade would start refusing traffic on the strength of a
// field nobody set.
func TestBotPolicySurvivesTheWire(t *testing.T) {
	in := &Snapshot{
		URL: "https://example.com/x", Status: "active",
		BotPolicy: domain.BotAllow, DomainBotPolicy: domain.DomainBotsEnforced,
	}
	b, err := in.encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.BotPolicy != in.BotPolicy || out.DomainBotPolicy != in.DomainBotPolicy {
		t.Errorf("bot policy did not survive the round trip: (%q, %q) != (%q, %q)",
			out.BotPolicy, out.DomainBotPolicy, in.BotPolicy, in.DomainBotPolicy)
	}

	// Exactly what the previous build wrote for an active link.
	old, err := decodeSnapshot([]byte(`{"i":"00000000-0000-0000-0000-000000000000",` +
		`"w":"00000000-0000-0000-0000-000000000000","u":"https://example.com/x","s":"active"}`))
	if err != nil {
		t.Fatal(err)
	}
	if domain.BlocksBots(old.BotPolicy, old.DomainBotPolicy) {
		t.Errorf("a snapshot written before bot blocking existed decoded as blocking "+
			"(%q, %q); the cache key version was left alone on the strength of this "+
			"not happening", old.BotPolicy, old.DomainBotPolicy)
	}
}
