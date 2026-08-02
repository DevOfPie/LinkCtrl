package redirect

import (
	"reflect"
	"strings"
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
	returning := true
	in := &Snapshot{
		LinkID: [16]byte{1}, WorkspaceID: [16]byte{2},
		URL: "https://example.com/x", Status: "active",
		ExpiresAt: &at, ForwardQuery: true, HasPassword: true,
		MaxClicks: &max, OneTime: true,
		// M34's fields are in the round trip because they are the reason the
		// cache key moved to v2. A rule that survives encoding but loses its
		// conditions would route every visitor to the rule's destination, which
		// is the worst possible failure of this payload.
		Destinations: []string{"https://example.com/gb", "https://example.com/de"},
		Rules: []SnapshotRule{
			{Dest: 0, Cond: domain.RuleConditions{
				Country:   []string{"GB"},
				Device:    []string{"mobile"},
				Time:      &domain.RuleTime{Days: []string{"mon"}, From: "09:00", To: "17:00", TZ: "Europe/London"},
				Returning: &returning,
				UTM:       map[string][]string{"source": {"newsletter"}},
			}},
			{Dest: 1, Cond: domain.RuleConditions{Country: []string{"DE"}}},
		},
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
	// DeepEqual rather than ==: the snapshot stopped being a comparable struct
	// when it grew the destination list, and that is the field most worth
	// comparing.
	if !reflect.DeepEqual(out, in) {
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

// TestForwardPathSurvivesTheWire is M33's version of the same claim, and the
// second half is again the one that matters.
//
// A payload written by the previous build has no `fp`, and it must decode as
// "do not forward". That is what makes the omitted CacheKeyVersion bump safe
// here — not an argument that nobody could have set the column yet, which a
// rolling restart falsifies. The zero value is the behaviour this alias had
// before the milestone existed, so a stale entry costs a visitor a 404 for at
// most REDIRECT_TTL and never sends them somewhere unconfigured.
//
// The claim holds only while the key is v1 on both sides of an upgrade. M34
// bumps it, which is why M33 lands first.
func TestForwardPathSurvivesTheWire(t *testing.T) {
	in := &Snapshot{URL: "https://example.com/x", Status: "active", ForwardPath: true}
	b, err := in.encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	if !out.ForwardPath {
		t.Error("ForwardPath did not survive the round trip")
	}

	// Exactly what the previous build wrote for an active forwarding-capable
	// link: every field M32.5 knew about, and nothing M33 added.
	old, err := decodeSnapshot([]byte(`{"i":"00000000-0000-0000-0000-000000000000",` +
		`"w":"00000000-0000-0000-0000-000000000000","u":"https://example.com/x",` +
		`"s":"active","q":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if old.ForwardPath {
		t.Error("a snapshot written before path forwarding existed decoded as forwarding; " +
			"the cache key version was left alone on the strength of the absent field " +
			"meaning 'do not forward'")
	}
	if !old.ForwardQuery {
		t.Error("decoding the older payload lost a field it did carry, so the " +
			"zero-decode above proves nothing")
	}
}

// --- routing rules (M34) -----------------------------------------------------

// staticSubject answers every question the same way. The rules being evaluated
// are what varies in these tests, not the request.
type staticSubject struct {
	country, device string
	now             time.Time
	returning       bool
}

func (s staticSubject) Country() string            { return s.country }
func (s staticSubject) Region() string             { return "" }
func (s staticSubject) City() string               { return "" }
func (s staticSubject) Language() string           { return "en" }
func (s staticSubject) Browser() string            { return "Chrome" }
func (s staticSubject) OS() string                 { return "Windows" }
func (s staticSubject) Device() string             { return s.device }
func (s staticSubject) ReferrerHost() string       { return "" }
func (s staticSubject) QueryParam(string) []string { return nil }
func (s staticSubject) Returning() bool            { return s.returning }
func (s staticSubject) Now() time.Time             { return s.now }

// countingSubject fails the test if it is consulted at all.
//
// It is how "a link with no rules resolves through the unchanged fast path" is
// asserted rather than described: Route must return before it touches anything.
type countingSubject struct {
	staticSubject
	t *testing.T
}

func (s countingSubject) Country() string {
	s.t.Error("Route consulted the request for a link with no rules")
	return ""
}

// TestRouteTakesTheFirstMatch is the whole of first-match evaluation.
//
// The snapshot's slice order *is* the priority order — the query applied
// priority and creation order before the list was built — so this is also the
// test that nothing re-sorts it on the way through.
func TestRouteTakesTheFirstMatch(t *testing.T) {
	snap := &Snapshot{
		URL:          "https://example.com/default",
		Destinations: []string{"https://example.com/gb", "https://example.com/mobile"},
		Rules: []SnapshotRule{
			{Dest: 0, Cond: domain.RuleConditions{Country: []string{"GB"}}},
			{Dest: 1, Cond: domain.RuleConditions{Device: []string{"mobile"}}},
		},
	}

	// Both rules match. The first one wins and the second is never reached.
	got, ok := snap.Route(staticSubject{country: "GB", device: "mobile", now: testNow})
	if !ok || got != "https://example.com/gb" {
		t.Errorf("first match = (%q, %v), want the GB destination", got, ok)
	}

	// Only the second matches.
	got, ok = snap.Route(staticSubject{country: "DE", device: "mobile", now: testNow})
	if !ok || got != "https://example.com/mobile" {
		t.Errorf("second match = (%q, %v), want the mobile destination", got, ok)
	}

	// Neither. The caller falls back to the link's own destination, which is why
	// Route reports *whether* a rule decided rather than returning a URL.
	if got, ok = snap.Route(staticSubject{country: "DE", device: "desktop", now: testNow}); ok {
		t.Errorf("no rule matches, but Route returned %q", got)
	}
}

// Two rules pointing at the same place cost one copy of the string. That is the
// reason Dest is an index rather than a URL, so it is worth asserting that the
// indirection resolves.
func TestRouteResolvesSharedDestinations(t *testing.T) {
	snap := &Snapshot{
		Destinations: []string{"https://example.com/eu"},
		Rules: []SnapshotRule{
			{Dest: 0, Cond: domain.RuleConditions{Country: []string{"FR"}}},
			{Dest: 0, Cond: domain.RuleConditions{Country: []string{"DE"}}},
		},
	}
	for _, country := range []string{"FR", "DE"} {
		got, ok := snap.Route(staticSubject{country: country, now: testNow})
		if !ok || got != "https://example.com/eu" {
			t.Errorf("%s routed to (%q, %v)", country, got, ok)
		}
	}
}

// A rule whose destination index is not in the list this payload carries. The
// only way to write one is a corrupt or hand-edited cache entry, and the hot
// path must survive it — a panic on a redirect is not a survivable answer, and
// an empty Location header is not one either.
func TestRouteSurvivesADanglingDestinationIndex(t *testing.T) {
	snap := &Snapshot{
		URL:          "https://example.com/default",
		Destinations: []string{"https://example.com/gb"},
		Rules: []SnapshotRule{
			{Dest: 7, Cond: domain.RuleConditions{Country: []string{"GB"}}},
			{Dest: 0, Cond: domain.RuleConditions{Country: []string{"GB"}}},
		},
	}
	got, ok := snap.Route(staticSubject{country: "GB", now: testNow})
	if !ok || got != "https://example.com/gb" {
		t.Errorf("a dangling index should be skipped, not honoured: (%q, %v)", got, ok)
	}

	only := &Snapshot{Destinations: nil, Rules: []SnapshotRule{{Dest: 0}}}
	if got, ok := only.Route(staticSubject{now: testNow}); ok {
		t.Errorf("a rule with no destination at all returned %q", got)
	}
}

// The fast path, asserted structurally: a link with no rules must not consult
// the request at all.
func TestRouteDoesNothingForALinkWithNoRules(t *testing.T) {
	snap := &Snapshot{URL: "https://example.com/x"}
	if _, ok := snap.Route(countingSubject{t: t}); ok {
		t.Error("a link with no rules reported a routed destination")
	}
	var nilSnap *Snapshot
	if _, ok := nilSnap.Route(countingSubject{t: t}); ok {
		t.Error("a nil snapshot reported a routed destination")
	}
}

// RuleNeeds is what the click recorder reads to decide whether the
// returning-visitor set must be maintained for this link.
func TestSnapshotRuleNeeds(t *testing.T) {
	none := (&Snapshot{URL: "https://example.com/x"}).RuleNeeds()
	if none.Returning || none.Geo() || none.UserAgent {
		t.Errorf("a link with no rules needs nothing, got %+v", none)
	}

	snap := &Snapshot{
		Destinations: []string{"https://example.com/a"},
		Rules: []SnapshotRule{
			{Dest: 0, Cond: domain.RuleConditions{City: []string{"Fictionbury"}}},
			{Dest: 0, Cond: domain.RuleConditions{Returning: func() *bool { b := true; return &b }()}},
		},
	}
	needs := snap.RuleNeeds()
	if !needs.City || !needs.Returning {
		t.Errorf("RuleNeeds missed a lookup the rules ask for: %+v", needs)
	}
	if needs.Country || needs.UserAgent {
		t.Errorf("RuleNeeds claimed a lookup no rule asks for: %+v", needs)
	}
}

// TestARuleFreeLinkEncodesExactlyAsItDidBeforeM34 is the SLO's half of the
// snapshot change.
//
// The overwhelming majority of links have no rules, and their cached payload
// must not have grown by so much as a pair of empty arrays — this value is
// serialized on every cache write and parsed on every miss.
func TestARuleFreeLinkEncodesExactlyAsItDidBeforeM34(t *testing.T) {
	snap := &Snapshot{
		LinkID: [16]byte{1}, WorkspaceID: [16]byte{2},
		URL: "https://example.com/x", Status: "active",
	}
	b, err := snap.encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"d"`, `"r"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("a link with no rules carries %s in its cached payload: %s", key, b)
		}
	}
}
