//go:build integration

package integration

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Campaigns end to end (M41).
//
// Against ruleFixture, for the reason folders_test.go gives.
//
// The test that matters most is TestDeletingACampaignKeepsItsLinks, and it is
// the same failure M38 named for folders with one difference that makes it
// sharper: the schema does *not* rescue this one. `links.campaign_id` is ON
// DELETE SET NULL, but the delete is soft, so the cascade never fires and the
// unlabelling is application code — which means it is a thing that can be
// forgotten rather than a thing the database refuses to get wrong.

func (f *ruleFixture) addCampaign(name, slug string) *domain.Campaign {
	f.t.Helper()
	c, err := f.links.CreateCampaign(f.t.Context(), f.owner,
		link.CreateCampaignInput{Name: name, Slug: slug})
	if err != nil {
		f.t.Fatalf("create campaign %q: %v", name, err)
	}
	return c
}

// campaignOf reads a link's campaign straight from the column, so an assertion
// about what deleting a campaign did to a link is not routed through the same
// service that might be wrong about it.
func (f *ruleFixture) campaignOf(linkID uuid.UUID) *uuid.UUID {
	f.t.Helper()
	var got *uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT campaign_id FROM links WHERE id = $1`, linkID).Scan(&got); err != nil {
		f.t.Fatalf("read campaign_id of %s: %v", linkID, err)
	}
	return got
}

func (f *ruleFixture) labelLink(linkID, campaignID uuid.UUID) {
	f.t.Helper()
	if _, err := f.links.Update(f.t.Context(), f.owner, linkID,
		link.UpdateInput{CampaignID: &campaignID}); err != nil {
		f.t.Fatalf("label link: %v", err)
	}
}

// --- the named failure --------------------------------------------------------

// TestDeletingACampaignKeepsItsLinks.
//
// Deleting a label deletes no link. Nothing in the schema enforces it here, so
// this is the assertion that stands between "tidy up a finished campaign" and
// "lose a campaign's worth of links".
func TestDeletingACampaignKeepsItsLinks(t *testing.T) {
	f := newRules(t)
	f.claim()

	c := f.addCampaign("Summer 2026", "")
	other := f.addCampaign("Evergreen", "")

	var labelled []uuid.UUID
	for _, alias := range []string{"kept-one", "kept-two", "kept-three"} {
		id := f.createLink(alias, "https://example.com/"+alias)
		f.labelLink(id, c.ID)
		labelled = append(labelled, id)
	}
	untouched := f.createLink("kept-four", "https://example.com/other")
	f.labelLink(untouched, other.ID)

	if err := f.links.DeleteCampaign(t.Context(), f.owner, c.ID); err != nil {
		t.Fatalf("delete campaign: %v", err)
	}

	for _, id := range labelled {
		if !f.linkExists(id) {
			t.Fatalf("link %s was deleted with its campaign", id)
		}
		if got := f.campaignOf(id); got != nil {
			t.Errorf("link %s still points at campaign %s, which no query returns; "+
				"the campaign filter can reach it and the campaign list cannot "+
				"explain it", id, got)
		}
	}
	// The other campaign's link is untouched, so the unlabelling is scoped
	// rather than a blanket update.
	if got := f.campaignOf(untouched); got == nil || *got != other.ID {
		t.Errorf("deleting one campaign unlabelled another's link")
	}
}

// TestADeletedCampaignIsGoneFromEverySurface. A soft delete that some query
// forgets to filter is a row that reappears in a picker.
func TestADeletedCampaignIsGoneFromEverySurface(t *testing.T) {
	f := newRules(t)
	f.claim()
	c := f.addCampaign("Ended", "")

	if err := f.links.DeleteCampaign(t.Context(), f.owner, c.ID); err != nil {
		t.Fatal(err)
	}
	list, err := f.links.Campaigns(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range list {
		if got.ID == c.ID {
			t.Fatal("a deleted campaign is still in the list")
		}
	}
	if _, err := f.links.Campaign(t.Context(), f.owner, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("reading a deleted campaign returned %v, want not found", err)
	}
	if err := f.links.DeleteCampaign(t.Context(), f.owner, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleting it twice returned %v, want not found", err)
	}
	// And the slug is free again, which is what makes the partial unique index
	// worth having.
	if _, err := f.links.CreateCampaign(t.Context(), f.owner,
		link.CreateCampaignInput{Name: "Ended", Slug: "ended"}); err != nil {
		t.Errorf("a deleted campaign's slug is still taken: %v", err)
	}
}

// --- the slug rule ------------------------------------------------------------

// TestASlugIsUniquePerWorkspaceCaseInsensitively is the milestone's explicit
// bullet. The service check makes the refusal a sentence; the index (00600) is
// what makes it true, which is why the last case goes through the database.
func TestASlugIsUniquePerWorkspaceCaseInsensitively(t *testing.T) {
	f := newRules(t)
	f.claim()
	f.addCampaign("Summer 2026", "")

	for _, slug := range []string{"summer-2026", "Summer-2026", "SUMMER-2026"} {
		_, err := f.links.CreateCampaign(t.Context(), f.owner,
			link.CreateCampaignInput{Name: "Another", Slug: slug})
		var ve domain.ValidationErrors
		if !errors.As(err, &ve) {
			t.Fatalf("slug %q was accepted twice: %v", slug, err)
		}
		if ve[0].Field != "slug" || ve[0].Code != "conflict" {
			t.Errorf("slug %q refused as %s/%s, want slug/conflict", slug, ve[0].Field, ve[0].Code)
		}
	}
}

// TestASlugIsDerivedFromTheNameWhenNoneIsGiven, and is reduced to something a
// query string can carry.
func TestASlugIsDerivedFromTheNameWhenNoneIsGiven(t *testing.T) {
	f := newRules(t)
	f.claim()

	cases := []struct{ name, want string }{
		{"Summer 2026", "summer-2026"},
		{"  Q4 — Paid Search  ", "q4-paid-search"},
		{"Ads/Display (EU)", "ads-display-eu"},
	}
	for i, tc := range cases {
		c, err := f.links.CreateCampaign(t.Context(), f.owner,
			link.CreateCampaignInput{Name: tc.name})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if c.Slug != tc.want {
			t.Errorf("%q slugged to %q, want %q", tc.name, c.Slug, tc.want)
		}
	}
	// A name with nothing sluggable is asked for a slug rather than handed a
	// percent-encoded one.
	_, err := f.links.CreateCampaign(t.Context(), f.owner,
		link.CreateCampaignInput{Name: "日本語"})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || ve[0].Field != "slug" {
		t.Errorf("a name with no ASCII slugged to something: %v", err)
	}
}

// --- the filter ---------------------------------------------------------------

// TestTheLinksListFiltersByCampaign, including the third question a nullable id
// cannot ask: which links carry none.
func TestTheLinksListFiltersByCampaign(t *testing.T) {
	f := newRules(t)
	f.claim()

	c := f.addCampaign("Launch", "")
	inside := f.createLink("inside", "https://example.com/in")
	f.labelLink(inside, c.ID)
	outside := f.createLink("outside", "https://example.com/out")

	if got := f.listIDs(domain.LinkFilter{CampaignID: &c.ID}); len(got) != 1 || got[0] != inside {
		t.Errorf("?campaign=%s returned %v, want [%s]", c.ID, got, inside)
	}
	none := f.listIDs(domain.LinkFilter{Uncampaigned: true})
	if len(none) != 1 || none[0] != outside {
		t.Errorf("?campaign=none returned %v, want [%s]", none, outside)
	}
	if got := f.listIDs(domain.LinkFilter{}); len(got) != 2 {
		t.Errorf("no filter returned %d links, want 2", len(got))
	}
	// A request asking for both has contradicted itself, and the empty answer is
	// the honest one.
	if got := f.listIDs(domain.LinkFilter{CampaignID: &c.ID, Uncampaigned: true}); len(got) != 1 ||
		got[0] != outside {
		t.Errorf("both filters at once returned %v", got)
	}
}

// TestTheTotalMatchesTheFilteredPage. A count that ignores the filter reads
// "1 of 40 links" over a list of one, which is the bug the folder and tag
// filters each had to be taught not to have.
func TestTheTotalMatchesTheFilteredPage(t *testing.T) {
	f := newRules(t)
	f.claim()
	c := f.addCampaign("Counted", "")
	id := f.createLink("counted", "https://example.com/x")
	f.labelLink(id, c.ID)
	f.createLink("other", "https://example.com/y")

	page, err := f.links.List(t.Context(), f.owner, domain.LinkFilter{
		CampaignID: &c.ID, IncludeTotal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total == nil || *page.Total != 1 {
		t.Errorf("the filtered page reports a total of %v, want 1", page.Total)
	}
}

// TestACampaignFromAnotherWorkspaceIsNotAValidLabel. The foreign key points at
// campaigns(id) and says nothing about tenancy, so this is the only check.
func TestACampaignFromAnotherWorkspaceIsNotAValidLabel(t *testing.T) {
	f := newRules(t)
	f.claim()
	id := f.createLink("mine", "https://example.com/x")

	stranger := uuid.Must(uuid.NewV7())
	_, err := f.links.Update(t.Context(), f.owner, id, link.UpdateInput{CampaignID: &stranger})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("a campaign id from nowhere was accepted: %v", err)
	}
	if ve[0].Field != "campaign_id" || ve[0].Code != "not_found" {
		t.Errorf("refused as %s/%s, want campaign_id/not_found", ve[0].Field, ve[0].Code)
	}
}

// --- the schedule -------------------------------------------------------------

// TestTheScheduleDescribesAndEnforcesNothing is decision-as-test. A link in a
// campaign that ended yesterday still redirects, because expiry belongs to the
// link and a second weaker one would give two answers to "why did this stop".
func TestTheScheduleDescribesAndEnforcesNothing(t *testing.T) {
	f := newRules(t)
	f.claim()

	past := time.Now().UTC().AddDate(0, 0, -30)
	ended := time.Now().UTC().AddDate(0, 0, -1)
	c, err := f.links.CreateCampaign(t.Context(), f.owner, link.CreateCampaignInput{
		Name: "Finished", StartsAt: &past, EndsAt: &ended,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := f.createLink("finished", "https://example.com/still-here")
	f.labelLink(id, c.ID)

	if got := f.location("/finished", nil); got != "https://example.com/still-here" {
		t.Fatalf("a link in a finished campaign redirected to %q", got)
	}
	if c.Active(time.Now()) {
		t.Error("a campaign that ended yesterday reports itself active")
	}
}

// TestACampaignEndsAfterItStarts, checked against the row as it will be rather
// than against the request, so moving one bound past a stored one is judged
// correctly.
func TestACampaignEndsAfterItStarts(t *testing.T) {
	f := newRules(t)
	f.claim()

	start := time.Now().UTC()
	end := start.AddDate(0, 0, 7)
	if _, err := f.links.CreateCampaign(t.Context(), f.owner, link.CreateCampaignInput{
		Name: "Backwards", StartsAt: &end, EndsAt: &start,
	}); err == nil {
		t.Fatal("a campaign ending before it starts was accepted")
	}

	c, err := f.links.CreateCampaign(t.Context(), f.owner, link.CreateCampaignInput{
		Name: "Forwards", StartsAt: &start, EndsAt: &end,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only the end date moves, and it moves before the stored start.
	earlier := start.AddDate(0, 0, -1)
	if _, err := f.links.UpdateCampaign(t.Context(), f.owner, c.ID,
		link.UpdateCampaignInput{EndsAt: &earlier}); err == nil {
		t.Error("an end date was moved before the stored start date")
	}
	// Removing the start makes the same end date fine.
	if _, err := f.links.UpdateCampaign(t.Context(), f.owner, c.ID,
		link.UpdateCampaignInput{EndsAt: &earlier, ClearStartsAt: true}); err != nil {
		t.Errorf("clearing the start and moving the end was refused: %v", err)
	}
}

// --- the surfaces -------------------------------------------------------------

// TestBothSurfacesManageCampaigns is the inherited rule — every UI feature has
// API support and both call one service — checked by doing the same three things
// through each.
func TestBothSurfacesManageCampaigns(t *testing.T) {
	f := newRules(t)
	f.claim()

	// Through the API.
	resp := f.postJSON("/api/v1/campaigns", `{"name":"From the API","description":"x"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /campaigns = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Through the dashboard.
	f.postCampaignForm(t, "/campaigns", url.Values{
		"name": {"From the form"}, "slug": {"from-the-form"},
		"starts_at": {"2026-06-01"}, "ends_at": {"2026-08-31"},
	})

	list, err := f.links.Campaigns(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("%d campaigns after one of each, want 2", len(list))
	}

	page := f.getHTML("/campaigns")
	for _, want := range []string{"From the API", "From the form", "from-the-form"} {
		if !strings.Contains(page, want) {
			t.Errorf("the campaigns page does not show %q", want)
		}
	}
	// The page's link through to the filtered list, which is the whole of what a
	// campaign does.
	for _, c := range list {
		if !strings.Contains(page, "/links?campaign="+c.ID.String()) {
			t.Errorf("the page does not link through to %q's links", c.Name)
		}
	}
	if !strings.Contains(page, "/links?campaign=none") {
		t.Error("the page does not offer the links in no campaign")
	}

	// And the form's editor and delete.
	target := list[0]
	f.postCampaignForm(t, "/campaigns/"+target.ID.String(), url.Values{
		"name": {"Renamed"}, "slug": {target.Slug},
		"description": {""}, "starts_at": {""}, "ends_at": {""},
	})
	after, err := f.links.Campaign(t.Context(), f.owner, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "Renamed" {
		t.Errorf("the edit form stored %q", after.Name)
	}
	// The form posts empty date boxes, which is how a form says "remove this
	// bound" — the third state the API spells with a clear flag.
	if after.StartsAt != nil || after.EndsAt != nil {
		t.Error("emptying the date boxes did not remove the schedule")
	}

	f.postCampaignForm(t, "/campaigns/"+target.ID.String()+"/delete", url.Values{})
	if _, err := f.links.Campaign(t.Context(), f.owner, target.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the delete form left the campaign readable: %v", err)
	}
}

// TestALinkCanCarryAFolderAndACampaign. They are different questions — where the
// link lives, and what it is for — so neither displaces the other.
func TestALinkCanCarryAFolderAndACampaign(t *testing.T) {
	f := newRules(t)
	f.claim()

	folder := f.addFolder("Product", nil)
	campaign := f.addCampaign("Launch", "")
	id := f.createLink("both", "https://example.com/x")

	if _, err := f.links.Update(t.Context(), f.owner, id, link.UpdateInput{
		FolderID: &folder.ID, CampaignID: &campaign.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.folderOf(id); got == nil || *got != folder.ID {
		t.Errorf("the folder was lost: %v", got)
	}
	if got := f.campaignOf(id); got == nil || *got != campaign.ID {
		t.Errorf("the campaign was lost: %v", got)
	}

	// And each can be removed without the other.
	if _, err := f.links.Update(t.Context(), f.owner, id,
		link.UpdateInput{ClearCampaign: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.campaignOf(id); got != nil {
		t.Error("the campaign was not removed")
	}
	if got := f.folderOf(id); got == nil || *got != folder.ID {
		t.Error("removing the campaign also unfiled the link")
	}
}

// TestCampaignAnalyticsIsNotStarted is a boundary test, and it is deliberate.
//
// m41.md draws a hard line: campaign rollups would stack new load on the job
// M37 has just fixed, and that fix should prove itself at scale first. A table
// or a rollup pass appearing here would be that line being crossed quietly.
func TestCampaignAnalyticsIsNotStarted(t *testing.T) {
	f := newRules(t)

	var n int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name LIKE 'campaign%daily'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d campaign rollup tables exist; campaign analytics stays Phase 2+", n)
	}
	if err := f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM link_dimension_daily WHERE dimension = 'campaign'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d campaign dimension rows exist; the rollup gained a pass it "+
			"was not meant to have", n)
	}
}

// --- helpers ------------------------------------------------------------------

func (f *ruleFixture) postCampaignForm(t *testing.T, path string, vals url.Values) {
	t.Helper()
	resp := f.postForm(path, vals)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s = %d, want 303", path, resp.StatusCode)
	}
}
