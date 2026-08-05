//go:build integration

package integration

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Split testing end to end (M36).
//
// Written against the ruleFixture rather than a fixture of its own, because a
// split is a routing decision: the arms live in the same table as the rules, are
// evaluated on the same path, and their only observable effect is where the
// redirect tree sends somebody. A second fixture would be a second wiring of the
// same three components that could drift from this one.

// strval reads a nullable text column, so a NULL and an empty string report the
// same way — the assertions below are about which column a value landed in.
func strval(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// destinationOf resolves the destinations row a rule points at.
func (f *ruleFixture) destinationOf(ruleID uuid.UUID) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT destination_id FROM routing_rules WHERE id = $1`, ruleID).Scan(&id); err != nil {
		f.t.Fatalf("resolve the destination of rule %s: %v", ruleID, err)
	}
	return id
}

func (f *ruleFixture) addVariant(linkID uuid.UUID, in link.CreateVariantInput) *domain.Variant {
	f.t.Helper()
	v, err := f.links.CreateVariant(f.t.Context(), f.owner, linkID, in)
	if err != nil {
		f.t.Fatalf("create %s variant: %v", in.Kind, err)
	}
	return v
}

// --- weighted ----------------------------------------------------------------

// TestWeightedSplitFollowsTheWeights is the headline for percentage splits.
//
// Asserted statistically, because the mechanism is random and a test that
// demanded exact proportions would be asserting the PRNG rather than the
// routing. The bounds are wide — a 75/25 split over 400 requests, checked to
// within fifteen points — so that only a real defect fails it: an arm never
// chosen, an arm always chosen, or the weights being ignored and the two arms
// splitting evenly. Each of those is well outside the band; ordinary variance is
// nowhere near it.
func TestWeightedSplitFollowsTheWeights(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("abtest", "https://example.com/control")

	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/a", Weight: 75, Enabled: true,
	})
	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/b", Weight: 25, Enabled: true,
	})

	const n = 400
	counts := map[string]int{}
	for range n {
		counts[f.location("/abtest", nil)]++
	}

	// The link's own destination must never be reached: every visitor a rule did
	// not claim now belongs to an arm.
	if got := counts["https://example.com/control"]; got != 0 {
		t.Errorf("%d of %d requests reached the link's own destination; a split with "+
			"live arms must claim all of them", got, n)
	}

	share := float64(counts["https://example.com/a"]) * 100 / n
	if share < 60 || share > 90 {
		t.Errorf("the 75%%-weighted arm took %.1f%% of %d requests (a=%d, b=%d); "+
			"the weights are not being applied",
			share, n, counts["https://example.com/a"], counts["https://example.com/b"])
	}
}

// TestDisabledArmIsTheFeatureFlag is the `enabled` half of the milestone.
//
// Switching an arm off must take it out of the rotation on the next request —
// not on the next cache expiry — and the remaining arms must re-share its
// traffic rather than the link falling back to its own destination.
func TestDisabledArmIsTheFeatureFlag(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("flagged", "https://example.com/control")

	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/kept", Weight: 50, Enabled: true,
	})
	parked := f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/parked", Weight: 50, Enabled: true,
	})

	off := false
	if _, err := f.links.UpdateVariant(t.Context(), f.owner, id, parked.ID,
		link.UpdateVariantInput{Enabled: &off}); err != nil {
		t.Fatalf("disable an arm: %v", err)
	}

	for i := range 40 {
		if got := f.location("/flagged", nil); got != "https://example.com/kept" {
			t.Fatalf("request %d went to %q after the other arm was disabled", i, got)
		}
	}

	// And back on. The toggle is reversible, which is what makes it a flag
	// rather than a delete.
	on := true
	if _, err := f.links.UpdateVariant(t.Context(), f.owner, id, parked.ID,
		link.UpdateVariantInput{Enabled: &on}); err != nil {
		t.Fatalf("re-enable an arm: %v", err)
	}
	seen := map[string]bool{}
	for range 60 {
		seen[f.location("/flagged", nil)] = true
	}
	if !seen["https://example.com/parked"] {
		t.Error("a re-enabled arm never received a visitor")
	}
}

// TestEveryArmAtZeroFallsThrough is the edge the weighted picker is written
// around.
//
// Parking every arm at weight zero must not break the link and must not invent
// traffic for arms whose owner set them to receive none. It falls through to the
// link's own destination, which is what the link did before the split existed.
func TestEveryArmAtZeroFallsThrough(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("allzero", "https://example.com/control")

	for _, u := range []string{"https://example.com/a", "https://example.com/b"} {
		f.addVariant(id, link.CreateVariantInput{
			Kind: domain.RuleKindWeighted, URL: u, Weight: 0, Enabled: true,
		})
	}
	if got := f.location("/allzero", nil); got != "https://example.com/control" {
		t.Errorf("with every arm at zero the link went to %q, want its own destination", got)
	}
}

// --- sequential --------------------------------------------------------------

// TestSequentialSplitIsStrict is D8, asserted rather than promised.
//
// Three arms, nine requests, and the answer must be the rotation repeated three
// times exactly. This is the one property "approximately sequential" would fail
// and a statistical test would not notice.
// HEAD chooses an arm and does not advance the rotation.
//
// A link checker or an unfurler probing a sequentially split link used to move
// the durable counter on every probe, re-phasing every subsequent visitor's arm
// — and because HEAD writes no click event, the per-destination breakdown could
// not show why the arms were uneven (F100). That falsified two absolutes in one
// decision entry: "the budget is the only gate that consumes something" and
// "HEAD never consumes at all", both written before M36 put a second durable
// counter in the same table.
//
// The assertion is deliberately in two parts. The counter must not move — that
// is the defect — and the HEAD must still answer the arm a GET would, because
// returning early on HEAD would have a checker validate the link's own
// destination, a URL no visitor is ever sent to.
func TestHeadChoosesASplitArmWithoutAdvancingIt(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("headrota", "https://example.com/control")

	urls := []string{"https://example.com/one", "https://example.com/two"}
	for _, u := range urls {
		f.addVariant(id, link.CreateVariantInput{
			Kind: domain.RuleKindSequential, URL: u, Enabled: true,
		})
	}

	rotation := func() int64 {
		t.Helper()
		var n int64
		if err := f.pool.QueryRow(t.Context(),
			`SELECT coalesce(max(rotation), 0) FROM link_click_budget WHERE link_id = $1`,
			id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	headLocation := func() string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodHead,
			f.server.URL+"/headrota", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("User-Agent", humanUA)
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("HEAD /headrota = %d, want 302", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}

	// Probed before anything has clicked. The first arm is what the first
	// visitor will get, so that is what a checker must be shown.
	before := rotation()
	for range 5 {
		if got := headLocation(); got != urls[0] {
			t.Errorf("HEAD went to %q, want %q — the first arm, which is what the "+
				"next GET will answer", got, urls[0])
		}
	}
	if after := rotation(); after != before {
		t.Errorf("five HEAD probes moved the rotation from %d to %d. Every visitor "+
			"after them is re-phased, and no click event was written to explain it",
			before, after)
	}

	// And the GET those probes did not disturb still gets the first arm.
	if got := f.location("/headrota", nil); got != urls[0] {
		t.Errorf("the first real visitor went to %q, want %q — the probes moved the "+
			"rotation out from under them", got, urls[0])
	}
	// The second GET advances normally, so peeking has not broken advancing.
	if got := f.location("/headrota", nil); got != urls[1] {
		t.Errorf("the second visitor went to %q, want %q", got, urls[1])
	}
	// A HEAD after two clicks reports what the *third* visitor would get, which
	// is the first arm again on a two-armed rotation.
	if got := headLocation(); got != urls[0] {
		t.Errorf("HEAD after two clicks went to %q, want %q — the arm the next GET "+
			"would be given", got, urls[0])
	}
}

func TestSequentialSplitIsStrict(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("rota", "https://example.com/control")

	urls := []string{
		"https://example.com/one",
		"https://example.com/two",
		"https://example.com/three",
	}
	for _, u := range urls {
		f.addVariant(id, link.CreateVariantInput{
			Kind: domain.RuleKindSequential, URL: u, Enabled: true,
		})
	}

	for i := range 9 {
		want := urls[i%len(urls)]
		if got := f.location("/rota", nil); got != want {
			t.Fatalf("request %d went to %q, want %q — the rotation is not strict", i, got, want)
		}
	}

	// The counter is durable, and it is not the click budget. A one-time link
	// could not survive sharing one, which is why migration 02200 gave the
	// rotation a column of its own.
	var rotation, consumed int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT rotation, consumed FROM link_click_budget WHERE link_id = $1`, id).
		Scan(&rotation, &consumed); err != nil {
		t.Fatalf("read the rotation counter: %v", err)
	}
	if rotation != 9 {
		t.Errorf("rotation = %d after nine requests, want 9", rotation)
	}
	if consumed != 0 {
		t.Errorf("consumed = %d; a rotation must not spend a link's click budget", consumed)
	}
}

// TestASequentialSplitBehindAPasswordAdvancesOncePerVisit is F87.
//
// The password gate is the only gate that manufactures a guaranteed second
// request: the challenge exists to be posted back, so one visit arrives as two.
// While the split ran ahead of it unconditionally, each visit consumed two
// positions and served the second — so at any even arm count half the arms were
// served to nobody, and at **two** arms `arms[0]` was served to nobody, ever.
//
// Two arms deliberately, which is the count that makes the failure total rather
// than partial. `TestSequentialSplitIsStrict` uses three, an odd count that masks
// it: at N=3 the served positions cycle 2, 4, 6 → arms 1, 0, 2, which visits
// every arm and only gets the order wrong.
func TestASequentialSplitBehindAPasswordAdvancesOncePerVisit(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("rota-pw", "https://example.com/control")

	urls := []string{"https://example.com/one", "https://example.com/two"}
	for _, u := range urls {
		f.addVariant(id, link.CreateVariantInput{
			Kind: domain.RuleKindSequential, URL: u, Enabled: true,
		})
	}
	const password = "the-link-password"
	pw := password
	if _, err := f.links.Update(t.Context(), f.owner, id, link.UpdateInput{Password: &pw}); err != nil {
		t.Fatalf("put a password on the link: %v", err)
	}

	// Two visits, each of them a challenge and then the POST that answers it.
	// The arms must come out in order, starting with the first.
	for i, want := range urls {
		resp := f.get("/rota-pw", nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("visit %d: the challenge answered %d, want 200", i, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Fatalf("visit %d: the challenge carried Location %q", i, loc)
		}

		// The rotation has not moved yet. Asserted between the two halves rather
		// than only at the end, because a fix that advanced twice and then
		// rewound would satisfy an end-state check and still hand two visitors
		// the same arm under concurrency.
		if got := rotationOf(t, f, id); got != int64(i) {
			t.Fatalf("visit %d: the rotation is %d after the challenge alone, want %d — "+
				"a request that is going to be answered with the challenge must not "+
				"choose an arm", i, got, i)
		}

		resp = f.postForm("/rota-pw", url.Values{"password": {password}})
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("visit %d: the verified password answered %d, want 303", i, resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != want {
			t.Errorf("visit %d went to %q, want %q", i, got, want)
		}
	}

	if got := rotationOf(t, f, id); got != 2 {
		t.Errorf("rotation = %d after two visits, want 2", got)
	}
}

// rotationOf reads a link's durable rotation counter, which is 0 before the
// first arm is chosen because the row does not exist yet.
func rotationOf(t *testing.T, f *ruleFixture, id uuid.UUID) int64 {
	t.Helper()
	var rotation int64
	err := f.pool.QueryRow(t.Context(),
		`SELECT rotation FROM link_click_budget WHERE link_id = $1`, id).Scan(&rotation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read the rotation counter: %v", err)
	}
	return rotation
}

// TestSequentialAndAClickBudgetDoNotShareACounter is the concrete failure the
// separate column exists to prevent.
//
// A one-time link carrying a sequential split must still be followable once. If
// the rotation wrote `consumed`, the first visitor would advance it to 1, the
// gate would then find the budget spent, and the link would 410 on the request
// that was supposed to work.
func TestSequentialAndAClickBudgetDoNotShareACounter(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("once-rota", "https://example.com/control")
	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindSequential, URL: "https://example.com/one", Enabled: true,
	})
	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindSequential, URL: "https://example.com/two", Enabled: true,
	})

	oneTime := true
	if _, err := f.links.Update(t.Context(), f.owner, id, link.UpdateInput{OneTime: &oneTime}); err != nil {
		t.Fatalf("make the link one-time: %v", err)
	}

	if got := f.location("/once-rota", nil); got != "https://example.com/one" {
		t.Errorf("the first visit to a one-time link with a rotation went to %q", got)
	}
	resp := f.get("/once-rota", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("the second visit returned %d, want 410", resp.StatusCode)
	}
}

// --- fallback ----------------------------------------------------------------

// TestFallbackStandsInForTheLinksOwnDestination is what the third kind is for.
//
// A fallback must catch whoever no rule and no arm claimed, without the link's
// own destination having been edited — and switching it off must put the link
// back exactly where it was, which is what makes it a reversible act.
func TestFallbackStandsInForTheLinksOwnDestination(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("fallb", "https://example.com/original")

	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/gb", Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})
	fallback := f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindFallback, URL: "https://example.com/catch-all", Enabled: true,
	})

	if got := f.location("/fallb", nil); got != "https://example.com/catch-all" {
		t.Errorf("an unmatched visitor went to %q, want the fallback", got)
	}

	off := false
	if _, err := f.links.UpdateVariant(t.Context(), f.owner, id, fallback.ID,
		link.UpdateVariantInput{Enabled: &off}); err != nil {
		t.Fatalf("disable the fallback: %v", err)
	}
	if got := f.location("/fallb", nil); got != "https://example.com/original" {
		t.Errorf("with the fallback off the link went to %q, want its own destination "+
			"unchanged", got)
	}
}

// TestAMatchingRuleBeatsASplit is the precedence between the two milestones.
//
// A rule is a statement about *who*; a split is a statement about *how many*.
// The rule wins, because it names this visitor and the split does not.
func TestAMatchingRuleBeatsASplit(t *testing.T) {
	f := newRulesOn(t, newDB(t), nil, fixedGeo{country: "GB"}, false)
	f.claim()
	id := f.createLink("both", "https://example.com/control")

	f.addRule(id, link.CreateRuleInput{
		URL: "https://example.com/gb", Enabled: true,
		Conditions: domain.RuleConditions{Country: []string{"GB"}},
	})
	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/a", Weight: 50, Enabled: true,
	})
	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/b", Weight: 50, Enabled: true,
	})

	for i := range 20 {
		if got := f.location("/both", nil); got != "https://example.com/gb" {
			t.Fatalf("request %d went to %q; a matching rule must beat the split", i, got)
		}
	}
}

// --- the fast path -----------------------------------------------------------

// TestALinkWithoutRulesIsUnchanged is the compatibility bullet, and it is
// asserted against the wire rather than against a snapshot.
//
// A link with no rules and no split must answer exactly what it answered in
// Phase 1: the same status, the same Location, the same headers, and a cached
// payload byte-identical to the one a link that never had rules would carry.
// M36 changed the shape of the destination list, so this is the test that says
// the change costs such a link nothing.
func TestALinkWithoutRulesIsUnchanged(t *testing.T) {
	f := newRules(t)
	f.claim()
	f.createLink("plain", "https://example.com/plain")

	resp := f.get("/plain", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /plain = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/plain" {
		t.Errorf("Location = %q", got)
	}
	for header, want := range map[string]string{
		"Cache-Control":          "private, no-store, max-age=0",
		"Referrer-Policy":        "unsafe-url",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// The trigger that mirrors the primary destination into links.primary_url is
	// still the thing the redirect path reads, and it still moves when the link
	// is edited. A split arm is a destination row on the same link, so this is
	// the assertion that adding one cannot move the primary.
	id := f.createLink("triggered", "https://example.com/before")
	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/arm", Weight: 10, Enabled: false,
	})
	var primary string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT primary_url FROM links WHERE id = $1`, id).Scan(&primary); err != nil {
		t.Fatal(err)
	}
	if primary != "https://example.com/before" {
		t.Errorf("adding a split arm moved links.primary_url to %q", primary)
	}
	after := "https://example.com/after"
	if _, err := f.links.Update(t.Context(), f.owner, id, link.UpdateInput{URL: &after}); err != nil {
		t.Fatalf("edit the link: %v", err)
	}
	if err := f.pool.QueryRow(t.Context(),
		`SELECT primary_url FROM links WHERE id = $1`, id).Scan(&primary); err != nil {
		t.Fatal(err)
	}
	if primary != after {
		t.Errorf("primary_url = %q after an edit, want %q; the sync trigger no longer "+
			"follows the primary destination", primary, after)
	}
}

// --- attribution -------------------------------------------------------------

// TestClicksCarryTheDestinationTheyWereSentTo is the milestone's named risk,
// tested the only way it can be.
//
// `destination_id` was added to the binary COPY encoder's column list. pgx sends
// values by position, so a column list and a row slice that disagree about order
// do not fail — they write a browser into a country and a latency into a
// language, silently. The assertion therefore reads the row back **column by
// column** and checks every field the ingester writes, not only the new one: a
// test that looked at destination_id alone would pass on exactly the shift this
// risk is about.
func TestClicksCarryTheDestinationTheyWereSentTo(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("attributed", "https://example.com/control")
	arm := f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/only-arm", Weight: 10, Enabled: true,
	})
	// The arm's own id is the *rule*; a click is attributed to the destination
	// row it points at. Resolved here rather than returned by CreateVariant,
	// because a client editing a split addresses the rule and only the analytics
	// ever names the destination.
	armDest := f.destinationOf(arm.ID)

	if got := f.location("/attributed", map[string]string{
		"Accept-Language": "en-GB,en;q=0.9",
		"Referer":         "https://news.example.org/story",
	}); got != "https://example.com/only-arm" {
		t.Fatalf("the only arm did not receive the visitor: %q", got)
	}
	waitForClicks(t, f.pool, id, 1)

	var (
		linkID, workspaceID uuid.UUID
		destID              *uuid.UUID
		occurred            time.Time
		visitor             []byte
		firstVisit, isBot   bool
		device, browser     *string
		os, language        *string
		referrer            *string
		latency             *int32
	)
	if err := f.pool.QueryRow(t.Context(), `
		SELECT link_id, workspace_id, occurred_at, visitor_hash, is_first_visit,
		       device, browser, os, language, referrer_host, is_bot, latency_us,
		       destination_id
		  FROM click_events WHERE link_id = $1`, id).
		Scan(&linkID, &workspaceID, &occurred, &visitor, &firstVisit,
			&device, &browser, &os, &language, &referrer, &isBot, &latency, &destID); err != nil {
		t.Fatalf("read the click back: %v", err)
	}

	if destID == nil || *destID != armDest {
		t.Errorf("destination_id = %v, want the arm's destination %v", destID, armDest)
	}
	// Every other column, because the risk is a *shift* rather than an omission.
	for _, check := range []struct {
		column string
		got    any
		want   any
	}{
		{"link_id", linkID, id},
		{"is_first_visit", firstVisit, false},
		{"is_bot", isBot, false},
		{"device", strval(device), "desktop"},
		{"language", strval(language), "en"},
		{"referrer_host", strval(referrer), "news.example.org"},
	} {
		if fmt.Sprint(check.got) != fmt.Sprint(check.want) {
			t.Errorf("%s = %v, want %v — a value has landed in the wrong column, "+
				"which is what a COPY column list out of step with the row slice "+
				"does and never reports", check.column, check.got, check.want)
		}
	}
	if len(visitor) == 0 {
		t.Error("visitor_hash is empty; a value has landed in the wrong column")
	}
	if occurred.IsZero() {
		t.Error("occurred_at is zero; a value has landed in the wrong column")
	}
	if latency == nil {
		t.Error("latency_us is null; a value has landed in the wrong column")
	}
}

// TestAnUnattributedClickMeansTheLinksOwnDestination is the other half of the
// column's contract.
//
// NULL is load-bearing rather than a gap: it is where every click on every link
// without a split goes, and the breakdown reads it as the link's own
// destination. Storing an id for it would be a per-row copy of what the link
// already says.
func TestAnUnattributedClickMeansTheLinksOwnDestination(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("unattributed", "https://example.com/plain")
	f.location("/unattributed", nil)
	waitForClicks(t, f.pool, id, 1)

	var nulls, total int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT count(*) FILTER (WHERE destination_id IS NULL), count(*)
		  FROM click_events WHERE link_id = $1`, id).Scan(&nulls, &total); err != nil {
		t.Fatal(err)
	}
	if total == 0 || nulls != total {
		t.Errorf("%d of %d clicks on a link with no split carry a destination_id; "+
			"they must all be NULL", total-nulls, total)
	}
}

// --- destinations ------------------------------------------------------------

// TestVariantDestinationsGoThroughEveryTier is what M36 owes M30 for adding the
// sixth and seventh destination-writing surfaces.
//
// A split arm is somewhere a browser is sent, chosen by somebody who is not the
// visitor. An arm receiving 5% of the traffic is still that destination, served
// by this instance under this workspace's alias. The source scan in
// internal/link/surfaces_test.go fails the build if either surface stops going
// through checkDestination; this is the assertion about behaviour rather than
// about structure.
func TestVariantDestinationsGoThroughEveryTier(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("variant-tiers", "https://example.com/ok")

	refused := []struct {
		name string
		url  string
	}{
		{"the cloud metadata endpoint", "http://169.254.169.254/latest/meta-data/"},
		{"loopback", "http://127.0.0.1:8080/admin"},
		{"localhost by name", "http://localhost/admin"},
		{"a private address", "http://10.0.0.5/"},
		{"an obfuscated decimal address", "http://2130706433/"},
		{"a scheme outside the allowlist", "javascript:alert(1)"},
		{"a host on the seeded blocklist", "https://bit.ly/whatever"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.links.CreateVariant(t.Context(), f.owner, id, link.CreateVariantInput{
				Kind: domain.RuleKindWeighted, URL: tc.url, Weight: 50, Enabled: true,
			})
			var ve domain.ValidationErrors
			if !errors.As(err, &ve) {
				t.Fatalf("an arm pointing at %s was accepted (err=%v)", tc.url, err)
			}
			if ve[0].Field != "url" {
				t.Errorf("the refusal was reported against %q, not the url field", ve[0].Field)
			}
		})
	}

	// And on an edit, which is the seventh surface.
	arm := f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/fine", Weight: 50, Enabled: true,
	})
	bad := "http://169.254.169.254/"
	_, err := f.links.UpdateVariant(t.Context(), f.owner, id, arm.ID,
		link.UpdateVariantInput{URL: &bad})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Errorf("editing an arm to point at the metadata endpoint was accepted (err=%v)", err)
	}

	// Recorded, with this surface named, exactly as every other destination
	// refusal is.
	var n int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM audit_logs
		 WHERE action = 'destination.blocked'
		   AND metadata->>'surface' = 'link.split_variant'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no refusal was recorded against the split-variant surface")
	}
}

// --- the shape of a split ----------------------------------------------------

// TestALinksArmsAreAllOneKind is the refusal that keeps the redirect path from
// needing a precedence rule between two kinds.
func TestALinksArmsAreAllOneKind(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("mixed", "https://example.com/control")

	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/a", Weight: 50, Enabled: true,
	})
	_, err := f.links.CreateVariant(t.Context(), f.owner, id, link.CreateVariantInput{
		Kind: domain.RuleKindSequential, URL: "https://example.com/b", Enabled: true,
	})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("a sequential arm was added to a weighted split (err=%v)", err)
	}
	if ve[0].Code != "conflict" {
		t.Errorf("the refusal carried code %q, want conflict", ve[0].Code)
	}

	// A second fallback is refused for the same reason: only one thing can be
	// the answer when nothing else applies.
	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindFallback, URL: "https://example.com/catch", Enabled: true,
	})
	_, err = f.links.CreateVariant(t.Context(), f.owner, id, link.CreateVariantInput{
		Kind: domain.RuleKindFallback, URL: "https://example.com/catch-2", Enabled: true,
	})
	if !errors.As(err, &ve) {
		t.Errorf("a second fallback was accepted (err=%v)", err)
	}
}

// TestSplitSharesAreComputedAgainstTheEnabledArms is why `share` is not stored.
//
// It changes whenever any *other* arm changes, so a stored copy would be wrong
// the moment one did — and the number a person reads off the page is the one
// that decides how they interpret the breakdown beside it.
func TestSplitSharesAreComputedAgainstTheEnabledArms(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("shares", "https://example.com/control")

	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/a", Weight: 60, Enabled: true,
	})
	f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/b", Weight: 20, Enabled: true,
	})
	third := f.addVariant(id, link.CreateVariantInput{
		Kind: domain.RuleKindWeighted, URL: "https://example.com/c", Weight: 20, Enabled: true,
	})

	split, err := f.links.GetSplit(t.Context(), f.owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(split.Variants) != 3 || split.Kind != domain.RuleKindWeighted {
		t.Fatalf("split = %+v", split)
	}
	if got := split.Variants[0].Share; got < 59.9 || got > 60.1 {
		t.Errorf("the 60-weighted arm's share is %.2f%%, want 60%%", got)
	}

	// Disable the third. The other two must now divide the whole, on the same
	// weights.
	off := false
	if _, err := f.links.UpdateVariant(t.Context(), f.owner, id, third.ID,
		link.UpdateVariantInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	split, err = f.links.GetSplit(t.Context(), f.owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := split.Variants[0].Share; got < 74.9 || got > 75.1 {
		t.Errorf("after disabling an arm the 60-weighted one's share is %.2f%%, want 75%%; "+
			"shares are being computed against arms that receive nothing", got)
	}
	if got := split.Variants[2].Share; got != 0 {
		t.Errorf("a disabled arm reported a share of %.2f%%", got)
	}
}
