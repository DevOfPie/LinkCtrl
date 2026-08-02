package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

// countingGeo is a MaxMind database that records what it was asked.
//
// The counters are the point. Region and City are new work on a path with a
// 20ms budget, and m34.md's claim is that they happen only when a link's rules
// need them — which is a claim about calls that do *not* happen and can only be
// tested by counting.
type countingGeo struct {
	country, region, city    string
	countryN, regionN, cityN int
}

func (g *countingGeo) Country(netip.Addr) string { g.countryN++; return g.country }
func (g *countingGeo) Region(netip.Addr) string  { g.regionN++; return g.region }
func (g *countingGeo) City(netip.Addr) string    { g.cityN++; return g.city }

func ruleRequest(t *testing.T, target, ua, lang, referer string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	if lang != "" {
		r.Header.Set("Accept-Language", lang)
	}
	if referer != "" {
		r.Header.Set("Referer", referer)
	}
	// The client address arrives through the RealIP middleware in production.
	return r.WithContext(auth.WithClientIP(r.Context(), netip.MustParseAddr("203.0.113.5")))
}

func ruleSnapshot(rules ...redirect.SnapshotRule) *redirect.Snapshot {
	return &redirect.Snapshot{
		URL:          "https://example.com/default",
		Status:       "active",
		Destinations: []string{"https://example.com/gb", "https://example.com/mobile"},
		Rules:        rules,
	}
}

const chromeMobileUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) " +
	"AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/120.0 Mobile/15E148 Safari/604.1"

// TestRouteEvaluatesAgainstARealRequest walks a condition of each family
// through the handler rather than through a hand-built subject, so what is
// tested is the wiring — which header each condition reads, and whether the
// classifier and the geo resolver are the ones the rest of the product uses.
func TestRouteEvaluatesAgainstARealRequest(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC) // a Monday

	cases := []struct {
		name string
		cond domain.RuleConditions
		req  *http.Request
		want bool
	}{
		{"country", domain.RuleConditions{Country: []string{"GB"}},
			ruleRequest(t, "/x", chromeMobileUA, "", ""), true},
		{"region", domain.RuleConditions{Region: []string{"ENG"}},
			ruleRequest(t, "/x", chromeMobileUA, "", ""), true},
		{"city", domain.RuleConditions{City: []string{"Fictionbury"}},
			ruleRequest(t, "/x", chromeMobileUA, "", ""), true},
		{"the wrong city", domain.RuleConditions{City: []string{"Beispielstadt"}},
			ruleRequest(t, "/x", chromeMobileUA, "", ""), false},

		{"device", domain.RuleConditions{Device: []string{"mobile"}},
			ruleRequest(t, "/x", chromeMobileUA, "", ""), true},
		{"browser", domain.RuleConditions{Browser: []string{"Chrome"}},
			ruleRequest(t, "/x", chromeMobileUA, "", ""), true},
		{"os", domain.RuleConditions{OS: []string{"iOS"}},
			ruleRequest(t, "/x", chromeMobileUA, "", ""), true},

		{"language reads the first subtag", domain.RuleConditions{Language: []string{"en"}},
			ruleRequest(t, "/x", chromeMobileUA, "en-GB,en;q=0.9", ""), true},
		{"language does not match the region", domain.RuleConditions{Language: []string{"en-GB"}},
			ruleRequest(t, "/x", chromeMobileUA, "en-GB,en;q=0.9", ""), false},

		{"referrer is host-only", domain.RuleConditions{Referrer: []string{"news.example.com"}},
			ruleRequest(t, "/x", chromeMobileUA, "", "https://news.example.com/a/story?utm=1"), true},

		{"query", domain.RuleConditions{Query: map[string][]string{"plan": {"pro"}}},
			ruleRequest(t, "/x?plan=pro", chromeMobileUA, "", ""), true},
		{"utm through the prefix", domain.RuleConditions{UTM: map[string][]string{"source": {"newsletter"}}},
			ruleRequest(t, "/x?utm_source=newsletter", chromeMobileUA, "", ""), true},

		{"time", domain.RuleConditions{Time: &domain.RuleTime{Days: []string{"mon"}, From: "09:00", To: "17:00"}},
			ruleRequest(t, "/x", chromeMobileUA, "", ""), true},
		{"time on another day", domain.RuleConditions{Time: &domain.RuleTime{Days: []string{"sat"}}},
			ruleRequest(t, "/x", chromeMobileUA, "", ""), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &RedirectHandler{Geo: &countingGeo{country: "GB", region: "ENG", city: "Fictionbury"}}
			snap := ruleSnapshot(redirect.SnapshotRule{Dest: 0, Cond: tc.cond})
			got, ok := h.route(tc.req, snap, now)
			if ok != tc.want {
				t.Errorf("route matched = %v, want %v (destination %q)", ok, tc.want, got)
			}
			if ok && got != "https://example.com/gb" {
				t.Errorf("route returned %q", got)
			}
		})
	}
}

// TestGeographyIsResolvedOnlyWhenARuleAsks is the privacy and the budget claim
// in one test.
//
// Region and City are more identifying than a country and are the expensive
// half of this milestone. They must be resolved for a link whose rules name
// them and for no other link on the instance, even with a database mounted.
func TestGeographyIsResolvedOnlyWhenARuleAsks(t *testing.T) {
	t.Run("a device rule resolves no geography", func(t *testing.T) {
		geo := &countingGeo{country: "GB", region: "ENG", city: "Fictionbury"}
		h := &RedirectHandler{Geo: geo}
		h.route(ruleRequest(t, "/x", chromeMobileUA, "", ""), ruleSnapshot(
			redirect.SnapshotRule{Dest: 1, Cond: domain.RuleConditions{Device: []string{"mobile"}}},
		), time.Now())
		if geo.countryN+geo.regionN+geo.cityN != 0 {
			t.Errorf("a device-only rule performed %d country, %d region and %d city lookups",
				geo.countryN, geo.regionN, geo.cityN)
		}
	})

	t.Run("a city rule resolves a city and not a region", func(t *testing.T) {
		geo := &countingGeo{country: "GB", region: "ENG", city: "Fictionbury"}
		h := &RedirectHandler{Geo: geo}
		h.route(ruleRequest(t, "/x", chromeMobileUA, "", ""), ruleSnapshot(
			redirect.SnapshotRule{Dest: 0, Cond: domain.RuleConditions{City: []string{"Fictionbury"}}},
		), time.Now())
		if geo.cityN != 1 {
			t.Errorf("City was resolved %d times, want exactly 1", geo.cityN)
		}
		if geo.regionN != 0 || geo.countryN != 0 {
			t.Errorf("a city rule resolved %d regions and %d countries", geo.regionN, geo.countryN)
		}
	})

	t.Run("several rules on the same value resolve it once", func(t *testing.T) {
		geo := &countingGeo{country: "DE", region: "ENG", city: "Fictionbury"}
		h := &RedirectHandler{Geo: geo}
		// Three rules, all asking about the country, none of them matching. The
		// mmap walk must happen once, not three times — that is what the
		// subject's memoization is for.
		h.route(ruleRequest(t, "/x", chromeMobileUA, "", ""), ruleSnapshot(
			redirect.SnapshotRule{Dest: 0, Cond: domain.RuleConditions{Country: []string{"GB"}}},
			redirect.SnapshotRule{Dest: 0, Cond: domain.RuleConditions{Country: []string{"IE"}}},
			redirect.SnapshotRule{Dest: 0, Cond: domain.RuleConditions{Country: []string{"FR"}}},
		), time.Now())
		if geo.countryN != 1 {
			t.Errorf("Country was resolved %d times for three country rules, want 1", geo.countryN)
		}
	})
}

// With no MaxMind database a geographic rule must not match. Not "matches
// everybody", which is what a nil-tolerant resolver returning "" would produce
// if the comparison were the other way round — and which would route the whole
// instance's traffic somewhere on the strength of a missing file.
func TestAGeographicRuleNeverMatchesWithoutADatabase(t *testing.T) {
	h := &RedirectHandler{} // no Geo
	for _, cond := range []domain.RuleConditions{
		{Country: []string{"GB"}},
		{Region: []string{"ENG"}},
		{City: []string{"Fictionbury"}},
	} {
		if got, ok := h.route(ruleRequest(t, "/x", chromeMobileUA, "", ""),
			ruleSnapshot(redirect.SnapshotRule{Dest: 0, Cond: cond}), time.Now()); ok {
			t.Errorf("%+v matched with no database, routing to %q", cond, got)
		}
	}
}

// With no Redis the returning-visitor check answers "not returning". A rule
// looking for a returning visitor never fires; one looking for a new visitor
// fires for everybody. Both are documented, and both are the degradation the
// phase-wide "cache is optional" rule requires.
func TestReturningVisitorWithoutRedis(t *testing.T) {
	h := &RedirectHandler{} // no Returning
	req := ruleRequest(t, "/x", chromeMobileUA, "", "")

	returning := true
	if _, ok := h.route(req, ruleSnapshot(
		redirect.SnapshotRule{Dest: 0, Cond: domain.RuleConditions{Returning: &returning}},
	), time.Now()); ok {
		t.Error("a returning-visitor rule matched with no Redis configured")
	}

	fresh := false
	if _, ok := h.route(req, ruleSnapshot(
		redirect.SnapshotRule{Dest: 0, Cond: domain.RuleConditions{Returning: &fresh}},
	), time.Now()); !ok {
		t.Error("a new-visitor rule did not match with no Redis configured; " +
			"without a cache every visitor is new")
	}
}

// tracksReturning is what tells the click pipeline to maintain the set. It has
// to be true for exactly the links whose rules ask, or the set is either
// maintained for the whole instance or never written at all.
func TestTracksReturningFollowsTheRules(t *testing.T) {
	if tracksReturning(&redirect.Snapshot{URL: "https://example.com/x"}) {
		t.Error("a link with no rules asked for the returning-visitor set to be maintained")
	}
	if tracksReturning(ruleSnapshot(
		redirect.SnapshotRule{Dest: 0, Cond: domain.RuleConditions{Country: []string{"GB"}}},
	)) {
		t.Error("a country rule asked for the returning-visitor set to be maintained")
	}
	returning := true
	if !tracksReturning(ruleSnapshot(
		redirect.SnapshotRule{Dest: 0, Cond: domain.RuleConditions{Returning: &returning}},
	)) {
		t.Error("a returning-visitor rule did not ask for the set to be maintained")
	}
}

// TestDeepLinkForwardingFollowsTheRoutedDestination is the reason rules are
// evaluated before the path is joined.
//
// Joining onto the link's own URL and swapping the destination afterwards would
// send /{alias}/pricing to the rule's destination without the /pricing — a URL
// nobody configured and nobody asked for.
func TestDeepLinkForwardingFollowsTheRoutedDestination(t *testing.T) {
	snap := &redirect.Snapshot{
		URL: "https://example.com/default", Status: "active", ForwardPath: true,
	}
	req := httptest.NewRequest(http.MethodGet, "/abc/pricing", nil)

	got, ok := forwardable(snap, "https://example.com/gb", req)
	if !ok {
		t.Fatal("forwardable refused a joinable deep link")
	}
	if got != "https://example.com/gb/pricing" {
		t.Errorf("forwardable joined onto the wrong destination: %q", got)
	}

	// And the bare alias still gets the destination unchanged.
	bare := httptest.NewRequest(http.MethodGet, "/abc", nil)
	if got, _ := forwardable(snap, "https://example.com/gb", bare); got != "https://example.com/gb" {
		t.Errorf("a bare alias returned %q", got)
	}
}
