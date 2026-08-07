package ui

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// identityStub satisfies what the layout and pages ask of an identity — Can,
// IsAPIKey, and the display fields — without importing the auth package, which
// this package must not depend on even in tests.
type identityStub struct {
	Email string
	Name  string
	Role  string
	// UserID is read by the dispute queue's reviewer roster, which declines to
	// draw a Withdraw control against the signed-in account (M45).
	UserID string
	perms  map[string]bool
}

func (s *identityStub) Can(p string) bool { return s.perms[p] }
func (s *identityStub) IsAPIKey() bool    { return false }

func owner() *identityStub {
	return &identityStub{
		Email: "o@example.com", Name: "Owner", Role: "owner",
		UserID: "0198c9c5-0000-7000-8000-000000000001",
		perms: map[string]bool{
			// **links.read was missing until M48**, and three sections of the
			// link page were therefore rendered by nothing: `link_qr`,
			// `link_rules` and `link_split` are each guarded on it, so
			// TestEveryPageRenders had been exercising a link page with its QR
			// code, its routing rules and its split test all absent since M47
			// decomposed the page. Every owner holds links.read — an owner who
			// could create a link and not read one is not a state this product
			// has — so the fixture was wrong rather than narrow.
			//
			// Found by M48, which could not assert that its panel renders on the
			// link page until the section carrying it did.
			"links.read": true, "links.create": true, "links.update": true, "links.delete": true,
			// The identity menu's three administrative entries are drawn from
			// the shell on every page, so an owner holds all three permissions
			// here and every page exercises those branches.
			"members.read": true, "members.write": true, "workspace.write": true,
			// The review queue's entry. Held by no role since M45 (D98): it is an
			// instance-level grant, and this fixture's owner is the account that
			// claimed the instance, so it holds all three of the queue's
			// permissions and every branch of the page is exercised.
			"destinations.review": true,
			"destinations.decide": true,
			"instance.admin":      true,
			// Held by the owner role and by nothing else (D16), which is what
			// draws the organization form on the workspaces page.
			"orgs.create": true,
		},
	}
}

// twoWorkspaces is the switcher's data.
//
// Two of them, because the control draws nothing when there is one — which is
// every instance today — and a fixture with a single entry would leave the
// partial unexercised on every page it renders on.
func twoWorkspaces() []map[string]any {
	return []map[string]any{
		{
			"ID": "0198c9c5-0000-7000-8000-000000000010", "Name": "Default", "Slug": "default",
			"OrganizationID":   "0198c9c5-0000-7000-8000-000000000020",
			"OrganizationName": "Owner", "IsPersonal": true,
			"Current": true, "Default": false,
		},
		{
			"ID": "0198c9c5-0000-7000-8000-000000000011", "Name": "Marketing", "Slug": "marketing",
			"OrganizationID":   "0198c9c5-0000-7000-8000-000000000021",
			"OrganizationName": "Acme", "IsPersonal": false,
			"Current": false, "Default": false,
		},
	}
}

// pageData returns representative data for every page, so the test renders
// each one for real. A page added without an entry here fails the test, which
// is the point: an unexercised template is a 500 waiting for a visitor.
func pageData(t *testing.T) map[string]any {
	t.Helper()
	now := time.Now()
	ownerUserID := "0198c9c5-0000-7000-8000-000000000001"
	consumed := int64(416)
	lnk := map[string]any{
		"ID": "0198c9c5-0000-7000-8000-000000000001", "Alias": "demo",
		"ShortURL": "http://links.test/demo", "URL": "https://example.com/x",
		"Title": "A demo", "Description": "", "Status": "active",
		"Tags":       []map[string]any{{"Name": "launch"}},
		"ClickCount": int64(1234), "LastClickAt": &now,
		"CreatedAt": now, "UpdatedAt": now,
		// A gated link, so the click-limit control renders the branch M47
		// rewrote rather than its no-budget fallback. The figure is the one from
		// the blind task that produced the rewrite: the owner had 416 clicks and
		// could not tell whether a limit of 50 or of 466 was the right answer.
		// `withBudget` leaves this nil for a link with no gate, which is why the
		// template has a branch for that at all.
		"ClicksConsumed": &consumed,
	}
	stats := map[string]any{
		"Totals": map[string]int64{"Clicks": 40, "UniqueVisitors": 12, "BotClicks": 3},
		"Caveat": "estimates",
		"Dimensions": map[string]any{
			"device":   []map[string]any{{"Value": "mobile", "Clicks": int64(30)}},
			"browser":  []map[string]any{{"Value": "Chrome", "Clicks": int64(25)}},
			"os":       []map[string]any{{"Value": "Android", "Clicks": int64(20)}},
			"referrer": []map[string]any{},
			"language": []map[string]any{{"Value": "en", "Clicks": int64(35)}},
			// Three countries and a territory the 110m map has no shape for, so
			// every page render exercises the choropleth, its legend and its
			// "counted but not drawn" line rather than only its empty state.
			"country": []map[string]any{
				{"Value": "US", "Clicks": int64(24), "UniqueVisitors": int64(9)},
				{"Value": "GB", "Clicks": int64(10), "UniqueVisitors": int64(2)},
				{"Value": "ZA", "Clicks": int64(4), "UniqueVisitors": int64(1)},
				{"Value": "HK", "Clicks": int64(2), "UniqueVisitors": int64(1)},
			},
		},
	}
	series := []DayCount{
		{Day: "2026-07-28", Clicks: 10, Visitors: 4},
		{Day: "2026-07-29", Clicks: 0},
		{Day: "2026-07-30", Clicks: 30, Visitors: 8, Bots: 3},
	}
	data := map[string]any{
		"login": map[string]any{
			"Title": "Sign in", "Nav": "", "Identity": (*identityStub)(nil),
			"Email": "", "Next": "/links", "Error": "Nope.", "Notice": "",
		},
		"setup": map[string]any{
			"Title": "Set up", "Nav": "", "Identity": (*identityStub)(nil),
			"Name": "", "Email": "", "Error": "",
		},
		"error": map[string]any{
			"Title": "Not found", "Nav": "", "Identity": (*identityStub)(nil),
			"Code": 404, "Heading": "Not found", "Message": "Gone.",
		},
		"dashboard": map[string]any{
			"Title": "Dashboard", "Nav": "dashboard", "Identity": owner(),
			"Overview": map[string]any{
				"Totals": map[string]int64{"Clicks": 100, "UniqueVisitors": 40, "BotClicks": 5},
				"Caveat": "estimates",
			},
			"Series":     series,
			"Recent":     []map[string]any{lnk},
			"TotalLinks": func() *int64 { n := int64(3); return &n }(),
		},
		"links": map[string]any{
			"Title": "Links", "Nav": "links", "Identity": owner(),
			"Links": []map[string]any{lnk}, "HasMore": true,
			"NextURL": "/links?cursor=abc",
			"Total":   func() *int64 { n := int64(1); return &n }(),
			"Search":  "demo", "Status": "", "Sort": "newest", "Filtered": true,
			// The three conditional filters, each drawn only when the workspace
			// has any of that thing. Populated here so every control in M46's
			// filter panel renders on every run: without them the panel held two
			// selects, and the milestone's claim is about what happens to six.
			"Folder": "", "Campaign": "", "Domain": "",
			"FolderOptions": []map[string]any{
				{"ID": "0198c9c5-0000-7000-8000-000000000041", "Label": "‒ Summer", "Selected": false},
			},
			"CampaignOptions": []map[string]any{
				{"ID": "0198c9c5-0000-7000-8000-000000000050", "Label": "Summer 2026", "Selected": false},
			},
			"DomainOptions": []map[string]any{
				{"ID": "0198c9c5-0000-7000-8000-000000000051", "Hostname": "go.example.com", "Selected": false},
			},
			"Form":        map[string]string{"URL": "", "Alias": ""},
			"FieldErrors": map[string]string{"url": "bad"},
			// The appeal affordance, drawn only after a low-confidence refusal.
			// Set here so the branch is exercised on every run rather than only
			// by the test that reads it.
			"DisputeURL": "https://bit.ly/xyz",
			"Notice":     "Created.", "Error": "",
		},
		// The folder tree (M38), rendered mid-move so both states of every row
		// are exercised in one pass: the folder being moved, a destination the
		// service would accept, and one it would refuse (which offers no button
		// at all). Nested two levels deep, because the recursion in
		// `folder_rows` is the part of this template that can break silently.
		"folders": map[string]any{
			"Title": "Folders", "Nav": "links", "Identity": owner(),
			"Count": 3, "UnfiledURL": "/links?folder=none",
			"MaxDepth": 8, "MaxFolders": 500,
			"Nodes": []map[string]any{
				{
					"ID": "0198c9c5-0000-7000-8000-000000000040", "Name": "Campaigns",
					"LinkCount": int64(4), "Moving": false, "CanReceive": false,
					"Children": []map[string]any{{
						"ID": "0198c9c5-0000-7000-8000-000000000041", "Name": "Summer",
						"LinkCount": int64(2), "Moving": true, "CanReceive": false,
						"Children": []map[string]any{},
					}},
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000042", "Name": "Docs",
					"LinkCount": int64(0), "Moving": false, "CanReceive": true,
					"Children": []map[string]any{},
				},
			},
			"ParentOptions": []map[string]any{
				{"ID": "0198c9c5-0000-7000-8000-000000000040", "Label": "Campaigns", "Selected": false},
				{"ID": "0198c9c5-0000-7000-8000-000000000042", "Label": "‒ Docs", "Selected": true},
			},
			"FormName": "", "FormParent": "", "FieldErrors": map[string]string{},
			"MovingID":   "0198c9c5-0000-7000-8000-000000000041",
			"MovingName": "Summer", "CanMoveToRoot": true,
			"CanCreate": true, "CanUpdate": true, "CanDelete": true,
			"Notice": "Folder created.", "Error": "",
		},
		// Campaigns (M41). Two rows, one of them open for editing and one of
		// them scheduled, because the page's three interesting states — the
		// list, the inline editor and a schedule that has or has not started —
		// are all conditional markup that a single plain row would leave
		// unexercised.
		"campaigns": map[string]any{
			"Title": "Campaigns", "Nav": "links", "Identity": owner(),
			"Count": 2, "NoCampaignURL": "/links?campaign=none", "MaxCampaigns": 500,
			"Rows": []map[string]any{
				{
					"ID": "0198c9c5-0000-7000-8000-000000000050", "Name": "Summer 2026",
					"Slug": "summer-2026", "Description": "The June push",
					"LinkCount": int64(6), "LinksURL": "/links?campaign=0198c9c5-0000-7000-8000-000000000050",
					"Schedule": "1 Jun 2026 to 31 Aug 2026", "Active": true,
					"Editing": true, "StartsAt": "2026-06-01", "EndsAt": "2026-08-31",
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000051", "Name": "Evergreen",
					"Slug": "evergreen", "Description": "",
					"LinkCount": int64(0), "LinksURL": "/links?campaign=0198c9c5-0000-7000-8000-000000000051",
					"Schedule": "", "Active": false,
					"Editing": false, "StartsAt": "", "EndsAt": "",
				},
			},
			"FormName": "", "FormSlug": "", "FormDescription": "",
			"FormStartsAt": "", "FormEndsAt": "",
			"EditingID": "0198c9c5-0000-7000-8000-000000000050",
			"CanCreate": true, "CanUpdate": true, "CanDelete": true,
			"Notice": "Campaign created.", "Error": "",
		},
		// Webhooks (M42), with every state the page has to draw in one render:
		// an enabled registration with its editor open, a paused one with its
		// delivery log expanded, a delivered attempt beside an abandoned one
		// that never got a response, and the once-only signing secret above
		// them. A render with only the happy row would not exercise the
		// "no answer" cell, which is the cell somebody debugging reads first.
		"webhooks": map[string]any{
			"Title": "Webhooks", "Nav": "webhooks", "Identity": owner(),
			"Count": 2, "MaxWebhooks": 20, "MaxAttempts": 7, "Retention": 30,
			"Events": []map[string]any{
				{"Name": "link.created", "Checked": true},
				{"Name": "destination.blocked", "Checked": false},
			},
			"Secret":    "9f2c1d0e5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d",
			"SecretFor": "0198c9c5-0000-7000-8000-000000000060",
			"Rows": []map[string]any{
				{
					"ID":  "0198c9c5-0000-7000-8000-000000000060",
					"URL": "https://hooks.example.com/linkctrl", "Description": "Ops channel",
					"Events": []string{"link.created", "link.updated"},
					"EventChoices": []map[string]any{
						{"Name": "link.created", "Checked": true},
						{"Name": "destination.blocked", "Checked": false},
					},
					"Enabled": true, "CreatedAt": "2026-08-01",
					"Editing": true, "ShowLog": false,
					"Deliveries": []map[string]any{},
				},
				{
					"ID":  "0198c9c5-0000-7000-8000-000000000061",
					"URL": "https://audit.example.net/events", "Description": "",
					"Events": []string{"destination.blocked"},
					"EventChoices": []map[string]any{
						{"Name": "link.created", "Checked": false},
						{"Name": "destination.blocked", "Checked": true},
					},
					"Enabled": false, "CreatedAt": "2026-07-14",
					"Editing": false, "ShowLog": true,
					"Deliveries": []map[string]any{
						{
							"Event": "link.created", "Status": "delivered", "Attempts": int32(1),
							"Code": "200", "Error": "", "When": "2026-08-02 09:41",
							"Next": "", "OK": true,
						},
						{
							"Event": "destination.blocked", "Status": "abandoned", "Attempts": int32(7),
							"Code": "no answer", "Error": "dial tcp: i/o timeout",
							"When": "2026-08-01 22:03", "Next": "", "OK": false,
						},
					},
				},
			},
			"FormURL": "", "FormDescription": "",
			"EditingID": "0198c9c5-0000-7000-8000-000000000060",
			"OpenLogID": "0198c9c5-0000-7000-8000-000000000061",
			"CanRead":   true, "CanWrite": true,
			"Notice": "Webhook registered. Copy the signing secret now — it is shown " +
				"once and cannot be read back.",
			"Error": "",
		},
		// Automation rules (M43). Both switch states in one render, for the
		// reason the webhooks fixture has both: a page where every row is
		// enabled never draws the paused pill or the Resume button, so a
		// regression in either is invisible.
		//
		// One row is open in the editor and one is not, so both the list body
		// and the inline form are exercised — and the open one carries a
		// threshold above one, which is the only state that renders the "after
		// N" fragment.
		"automation": map[string]any{
			"Title": "Automation", "Nav": "automation", "Identity": owner(),
			"Count": 2, "MaxRules": 20, "MaxActions": 3,
			"RulesPerRun": 100, "MatchesPerRule": 25, "IntervalMins": 1,
			"Triggers": []map[string]any{
				{"Name": "link.expired", "Checked": true},
				{"Name": "link.max_clicks", "Checked": false},
				{"Name": "destination.blocked", "Checked": false},
			},
			"Actions": []map[string]any{
				{"Name": "notify", "Checked": true, "LinkOnly": false},
				{"Name": "webhook", "Checked": false, "LinkOnly": false},
				{"Name": "archive_link", "Checked": false, "LinkOnly": true},
			},
			"Rows": []map[string]any{
				{
					"ID":      "0198c9c5-0000-7000-8000-000000000070",
					"Name":    "Tell the team when a campaign link expires",
					"Trigger": "link.expired",
					"Actions": []string{"notify", "webhook"},
					"Enabled": true, "MinCount": 1,
					"LastFired": "2026-08-03 09:12", "CreatedAt": "2026-07-30",
					"TriggerChoices": []map[string]any{
						{"Name": "link.expired", "Checked": true},
						{"Name": "destination.blocked", "Checked": false},
					},
					"ActionChoices": []map[string]any{
						{"Name": "notify", "Checked": true, "LinkOnly": false},
						{"Name": "archive_link", "Checked": false, "LinkOnly": true},
					},
					"Editing": false,
				},
				{
					"ID":      "0198c9c5-0000-7000-8000-000000000071",
					"Name":    "Chase a refused destination after three attempts",
					"Trigger": "destination.blocked",
					"Actions": []string{"notify"},
					"Enabled": false, "MinCount": 3,
					"LastFired": "2026-08-01 14:00", "CreatedAt": "2026-07-14",
					"TriggerChoices": []map[string]any{
						{"Name": "link.expired", "Checked": false},
						{"Name": "destination.blocked", "Checked": true},
					},
					"ActionChoices": []map[string]any{
						{"Name": "notify", "Checked": true, "LinkOnly": false},
						{"Name": "archive_link", "Checked": false, "LinkOnly": true},
					},
					"Editing": true,
				},
			},
			"FormName": "", "FormMinCount": "",
			"EditingID": "0198c9c5-0000-7000-8000-000000000071",
			"CanRead":   true, "CanWrite": true,
			"Notice": "Rule created and armed. It acts on what happens from now on, " +
				"never on what already happened.",
			"Error": "",
		},
		// Registered hostnames (M39), with all three ownership states in one
		// render: the instance default, which nobody manages from this page; a
		// hostname this workspace owns and may change; and one owned elsewhere
		// in the organization, which draws no controls.
		//
		// None of them is verified, which is the milestone's whole claim — the
		// row has to say so, and a fixture where everything were verified would
		// never render the sentence that carries it.
		"domains": map[string]any{
			"Title": "Domains", "Nav": "domains", "Identity": owner(),
			"CanRegister": true, "FormHostname": "",
			"Rows": []map[string]any{
				{
					"ID": "0198c9c5-0000-7000-8000-000000000050", "Hostname": "default",
					"ScopeLabel": "This instance", "IsDefault": true, "Verified": true,
					"LinkCount": int64(21), "Manageable": false,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000051", "Hostname": "go.example.com",
					"ScopeLabel": "This workspace", "IsDefault": false, "Verified": false,
					"LinkCount": int64(0), "Manageable": true,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000052", "Hostname": "l.example.org",
					"ScopeLabel": "This organization", "IsDefault": false, "Verified": false,
					"LinkCount": int64(0), "Manageable": false,
				},
			},
			"Notice": "Hostname registered to this workspace.", "Error": "",
		},
		// The review queue, with every state it can draw in one render: an open
		// dispute that can be allowed, an open one that cannot (a heuristic has
		// no blocklist row to remove), and both decided outcomes.
		//
		// The two defanged strings are what the service actually produces —
		// link.Defang's output — because the test that reads this page asserts
		// against the rendered HTML and a fixture holding a live URL would let
		// the assertion pass for the wrong reason.
		//
		// The first item's Host and BlockedHost differ on purpose, and that is
		// F33's shape: somebody typed login.evil.example and the row that refused
		// them says evil.example, which is what Allow deletes. Making them equal
		// here would let the queue render only one of the two and still pass.
		"disputes": map[string]any{
			"Title": "Blocked destinations", "Nav": "disputes", "Identity": owner(),
			"OpenCount": int64(2), "OpenOnly": true,
			"Items": []map[string]any{
				{
					"ID": "0198c9c5-0000-7000-8000-000000000030", "Status": "open",
					"Host":        "login[.]evil[.]example",
					"BlockedHost": "evil[.]example",
					"Destination": "https[:]//login[.]evil[.]example/promo%3Cscript%3E",
					"ReasonCode":  "low_confidence.operator_blocklist",
					"FiledBy":     "editor@example.com", "CreatedAt": now,
					"Liftable": true, "DecidedAt": (*time.Time)(nil),
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000031", "Status": "open",
					"Host":        "xn--80ak6aa92e[.]com",
					"BlockedHost": "",
					"Destination": "https[:]//xn--80ak6aa92e[.]com/",
					"ReasonCode":  "low_confidence.punycode_homograph",
					"FiledBy":     "editor@example.com", "CreatedAt": now,
					"Liftable": false, "DecidedAt": (*time.Time)(nil),
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000032", "Status": "allowed",
					"Host":        "bit[.]ly",
					"BlockedHost": "bit[.]ly",
					"Destination": "https[:]//bit[.]ly/abc",
					"ReasonCode":  "low_confidence.shortener_chain",
					"FiledBy":     "editor@example.com", "CreatedAt": now,
					"Liftable": true, "DecidedBy": "o@example.com", "DecidedAt": &now,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000033", "Status": "upheld",
					"Host":        "phish[.]example",
					"BlockedHost": "phish[.]example",
					"Destination": "https[:]//phish[.]example/",
					"ReasonCode":  "low_confidence.operator_blocklist",
					"FiledBy":     "gone@example.com", "CreatedAt": now,
					"Liftable": true, "DecidedBy": "", "DecidedAt": &now,
				},
			},
			"NextCursor": "abc", "Notice": "", "Error": "",
			"CanDecide": true, "CanAdminister": true,
			// Two reviewers, because the roster's interesting branch is the row
			// that is *not* the reader: one carries a Withdraw control and the
			// other says "you". A single-row fixture would leave half the
			// section unrendered, which is the same trap twoWorkspaces avoids.
			"Reviewers": reviewerRoster(now, &ownerUserID),
			// Where the panel's forms return to when they are submitted from
			// this page rather than from the roster's own route (M48).
			"ReviewersReturn": "/disputes",
		},
		// The reviewer panel as a page (M48): the same block the popup on
		// /disputes renders, served at /disputes/reviewers. Both entries carry
		// the same roster, so a divergence between the two surfaces shows up as
		// one of them rendering something the other does not.
		"dispute_reviewers": map[string]any{
			"Title": "Who reviews disputes", "Nav": "disputes", "Identity": owner(),
			"Reviewers":       reviewerRoster(now, &ownerUserID),
			"ReviewersReturn": "/disputes/reviewers",
			"Notice":          "", "Error": "",
		},
		"link_detail": map[string]any{
			"Title": "/demo", "Nav": "links", "Identity": owner(),
			"Link": lnk, "Stats": stats, "Series": series,
			"RecentClicks": []map[string]any{{
				"OccurredAt": now, "Device": "mobile", "Browser": "Chrome",
				"OS": "Android", "Referrer": "", "IsBot": true,
			}},
			"Days": 30, "Windows": []int{7, 30, 90},
			// The map and the rings, laid out the way the handler lays them out
			// (M37). Built rather than stubbed, so a change to Choropleth's own
			// output has to survive every page-render test here.
			"Map": Choropleth(map[string]int64{"US": 24, "GB": 10, "ZA": 4, "HK": 2},
				"clicks", "estimates", true),
			"Donuts": map[string]Donut{
				"device":   DonutChart([]DimensionSlice{{Name: "mobile", Count: 30}}, 40, 100),
				"browser":  DonutChart([]DimensionSlice{{Name: "Chrome", Count: 25}}, 40, 100),
				"os":       DonutChart([]DimensionSlice{{Name: "Android", Count: 20}}, 40, 100),
				"referrer": DonutChart(nil, 40, 100),
				"language": DonutChart([]DimensionSlice{{Name: "en", Count: 35}}, 40, 100),
				"country": DonutChart([]DimensionSlice{
					{Name: "US", Count: 24}, {Name: "GB", Count: 10},
					{Name: "ZA", Count: 4}, {Name: "HK", Count: 2},
				}, 40, 100),
			},
			"Countries": []map[string]any{
				{"Value": "US", "Clicks": int64(24), "UniqueVisitors": int64(9)},
				{"Value": "GB", "Clicks": int64(10), "UniqueVisitors": int64(2)},
				{"Value": "ZA", "Clicks": int64(4), "UniqueVisitors": int64(1)},
				{"Value": "HK", "Clicks": int64(2), "UniqueVisitors": int64(1)},
			},
			// Routing rules and the split test, which the fixture has never
			// carried because the permission that draws them was missing from
			// `owner` — see the note there. Both sections render an empty state
			// and a populated one, and the populated one is the interesting
			// branch: a table, a toggle whose label flips on `Enabled`, and a
			// fallback row. One rule of each state, and one arm of each, for the
			// reason twoWorkspaces gives.
			"Rules": []map[string]any{
				{
					"Rule": map[string]any{
						"ID": "0198c9c5-0000-7000-8000-000000000060", "Priority": 10,
						"URL": "https://example.com/uk", "Enabled": true,
					},
					"Summary": "Country is GB",
				},
				{
					"Rule": map[string]any{
						"ID": "0198c9c5-0000-7000-8000-000000000061", "Priority": 20,
						"URL": "https://example.com/mobile", "Enabled": false,
					},
					"Summary": "Device is mobile",
				},
			},
			"RuleWeekdays":       []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
			"RuleHelp":           "One condition per line.",
			"ReturningAvailable": true,
			"Split": map[string]any{
				"Kind": "weighted",
				"Variants": []map[string]any{
					{
						"ID":  "0198c9c5-0000-7000-8000-000000000070",
						"URL": "https://example.com/a", "Weight": 60,
						"Enabled": true, "Share": 60.0,
					},
					{
						"ID":  "0198c9c5-0000-7000-8000-000000000071",
						"URL": "https://example.com/b", "Weight": 40,
						"Enabled": false, "Share": 0.0,
					},
				},
				"Fallback": map[string]any{
					"ID":  "0198c9c5-0000-7000-8000-000000000072",
					"URL": "https://example.com/fallback", "Enabled": true,
				},
			},
			"SplitKinds":        []string{"weighted", "sequential"},
			"SplitHelp":         "A weight is a share, not a percentage.",
			"MaxWeight":         100,
			"MinPasswordLength": 12,
			"GeoAvailable":      true,
			"GeoBase":           "/links/0198c9c5-0000-7000-8000-000000000001?days=30",
			"GeoList":           "/links/0198c9c5-0000-7000-8000-000000000001?days=30#countries",
			"GeoUnavailable":    GeoUnavailable,
			"Form": map[string]string{
				"URL": "https://example.com/x", "Alias": "demo", "Title": "A demo",
				"Description": "", "ExpiresAt": "", "Tags": "launch",
				"CampaignID": "0198c9c5-0000-7000-8000-000000000050",
				// The limit the blind task's owner was trying to set, beside the
				// 416 already spent on Link.ClicksConsumed. Both numbers, because
				// the sentence M47 replaced two elements with names both and
				// TestTheClickLimitNamesTheTotalAndWhatIsSpent reads it.
				"MaxClicks": "466",
			},
			// The campaign select and the QR panel (M41). The SVG is a stub
			// rather than a real code: this test exercises the template, and
			// internal/qr is where the drawing is tested against the encoder.
			"CampaignOptions": []map[string]any{
				{"ID": "0198c9c5-0000-7000-8000-000000000050", "Label": "Summer 2026", "Selected": true},
			},
			"QRSVG": template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" role="img"></svg>`),
			// The class is QRThumbClass rather than a copy of it: this stub is what
			// TestTheEditControlIsReachableWithoutScrolling measures the heading
			// row's height from, and a fixture free to state a different height
			// from the one internal/httpx renders would measure a page nobody is
			// served.
			"QRThumbSVG": template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" class="` +
				QRThumbClass + `" width="111" height="111" role="img"></svg>`),
			"QRContent": "http://links.test/demo?src=qr",
			"QRStyle": map[string]any{
				"Foreground": "#000000", "Background": "#ffffff",
				"Level": "M", "Margin": 4, "Scale": 8,
			},
			// Since M49 the form asks for one number in pixels and no longer
			// asks for a level, a quiet zone or a module size. 296 is what the
			// stub style above comes to for a 29-module code, so the fixture is
			// a state internal/qr could actually produce rather than a number
			// picked to look plausible.
			"QRSize":        296,
			"QRMinSize":     64,
			"QRMaxSize":     2000,
			"QRStored":      true,
			"QRDownload":    "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr.svg",
			"QRDownloadPNG": "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr.png",
			"QRSourceLabel": "qr",
			"QRReturn":      "/links/0198c9c5-0000-7000-8000-000000000001",
			// Two codes (M50), because the fixture has to render the state the
			// milestone exists for: a link with one code and a link with two are
			// different markup, and the theme scan, the overflow check and the
			// fold measurement all read whichever this fixture is. The first row
			// is the default code — no slug, and no remove control, because it is
			// the one every already-printed picture resolves to.
			"QRCodes": []map[string]any{
				{"Slug": "", "Label": "", "Name": "The original code", "Size": 296,
					"Default": true, "Selected": true,
					"Panel":       "/links/0198c9c5-0000-7000-8000-000000000001/qr",
					"Download":    "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr.svg",
					"DownloadPNG": "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr.png",
					"Clicks":      412, "Counted": true},
				{"Slug": "k7m2qh4b", "Label": "Autumn poster", "Name": "Autumn poster", "Size": 296,
					"Default": false, "Selected": false,
					"Panel":       "/links/0198c9c5-0000-7000-8000-000000000001/qr?code=k7m2qh4b",
					"Download":    "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/k7m2qh4b/image.svg",
					"DownloadPNG": "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/k7m2qh4b/image.png",
					"Clicks":      37, "Counted": true},
			},
			"QRSlug":      "",
			"QRLabel":     "",
			"QRMaxCodes":  20,
			"QRMaxLabel":  60,
			"FieldErrors": map[string]string{},
			"Notice":      "", "Error": "",
		},
		// The QR panel as a page (M48). Same fields as the link page's QR area,
		// because it is the same block: linkQRView is one struct embedded in two
		// page structs, and this fixture is the assertion that the second one is
		// not short of anything the block reads. QRReturn differs, and only
		// QRReturn, which is the panel's own route rather than the link page.
		"link_qr": map[string]any{
			"Title": "QR code · /demo", "Nav": "links", "Identity": owner(),
			"Link":  lnk,
			"QRSVG": template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" role="img"></svg>`),
			// The class is QRThumbClass rather than a copy of it: this stub is what
			// TestTheEditControlIsReachableWithoutScrolling measures the heading
			// row's height from, and a fixture free to state a different height
			// from the one internal/httpx renders would measure a page nobody is
			// served.
			"QRThumbSVG": template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" class="` +
				QRThumbClass + `" width="111" height="111" role="img"></svg>`),
			"QRContent": "http://links.test/demo?src=qr",
			"QRStyle": map[string]any{
				"Foreground": "#000000", "Background": "#ffffff",
				"Level": "M", "Margin": 4, "Scale": 8,
			},
			"QRSize":        296,
			"QRMinSize":     64,
			"QRMaxSize":     2000,
			"QRStored":      true,
			"QRDownload":    "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr.svg",
			"QRDownloadPNG": "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr.png",
			"QRSourceLabel": "qr",
			"QRReturn":      "/links/0198c9c5-0000-7000-8000-000000000001/qr",
			// Two codes (M50), because the fixture has to render the state the
			// milestone exists for: a link with one code and a link with two are
			// different markup, and the theme scan, the overflow check and the
			// fold measurement all read whichever this fixture is. The first row
			// is the default code — no slug, and no remove control, because it is
			// the one every already-printed picture resolves to.
			"QRCodes": []map[string]any{
				{"Slug": "", "Label": "", "Name": "The original code", "Size": 296,
					"Default": true, "Selected": true,
					"Panel":       "/links/0198c9c5-0000-7000-8000-000000000001/qr",
					"Download":    "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr.svg",
					"DownloadPNG": "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr.png",
					"Clicks":      412, "Counted": false},
				{"Slug": "k7m2qh4b", "Label": "Autumn poster", "Name": "Autumn poster", "Size": 296,
					"Default": false, "Selected": false,
					"Panel":       "/links/0198c9c5-0000-7000-8000-000000000001/qr?code=k7m2qh4b",
					"Download":    "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/k7m2qh4b/image.svg",
					"DownloadPNG": "/api/v1/links/0198c9c5-0000-7000-8000-000000000001/qr/codes/k7m2qh4b/image.png",
					"Clicks":      37, "Counted": false},
			},
			"QRSlug":     "",
			"QRLabel":    "",
			"QRMaxCodes": 20,
			"QRMaxLabel": 60,
			"Notice":     "",
		},
		"keys": map[string]any{
			"Title": "API keys", "Nav": "keys", "Identity": owner(),
			"Keys": []map[string]any{{
				"ID": "0198c9c5-0000-7000-8000-000000000002", "Name": "ci",
				"Prefix": "lk_live_abcdefgh", "Scopes": []string{"links.read"},
				"LastUsedAt": &now, "ExpiresAt": (*time.Time)(nil),
				"RevokedAt": (*time.Time)(nil), "CreatedAt": now, "Expired": false,
			}},
			"ScopeOptions": []string{"links.read", "links.create"},
			"Created": map[string]any{
				"Key": "lk_live_abcdefgh_0123456789012345678901234567890123456789012",
			},
			"Form":        map[string]string{"Name": ""},
			"FieldErrors": map[string]string{},
			"Notice":      "", "Error": "",
		},
		"account": map[string]any{
			"Title": "Account", "Nav": "account", "Identity": owner(),
			"FieldErrors": map[string]string{},
			"Notice":      "", "Error": "",
			// Nothing pinned, which is the state every account is in until
			// somebody chooses otherwise, and the one the control has to show
			// as *Last-Used*.
			"WorkspacePinned": false,
		},
		// Every state in one render. A read notification and an unread one, so
		// the dot, "Mark read" and "Mark unread" are each exercised; and since
		// M48 one item that leads somewhere beside one that does not, because
		// those are two different headings — a submit button and an <h2>. Unread
		// is non-zero so the nav badge renders too: it is drawn from the shell on
		// every page, and a template error there would break every page rather
		// than this one.
		"notifications": map[string]any{
			"Title": "Notifications", "Nav": "notifications", "Identity": owner(),
			"Unread": int64(1),
			"Items": []map[string]any{
				{
					"ID": "0198c9c5-0000-7000-8000-000000000003", "Kind": "audit.growth",
					"Title":  "The audit log has passed its size threshold",
					"Body":   "audit_logs now uses 5.2 GiB on disk.",
					"Data":   map[string]any{"bytes": int64(5583457484)},
					"Target": "",
					"ReadAt": (*time.Time)(nil), "CreatedAt": now,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000004", "Kind": "automation.fired",
					"Title": "Automation rule fired: Expiring soon", "Body": "",
					"Target": "/automation",
					"ReadAt": &now, "CreatedAt": now,
				},
			},
			"NextCursor": "abc",
			"Notice":     "", "Error": "",
		},
		// The disclosure with both channels live, which is the state that has
		// something to disclose about each. The other three are covered on their
		// own by TestTheDisclosureAnswersTheSameQuestionInEveryState, which
		// renders all four and reads the words — this entry exists so the
		// branches with a third party's name and a webhook count in them are
		// exercised by the every-page render too.
		"feeds": map[string]any{
			"Title": "Reputation feeds and webhooks", "Nav": "feeds", "Identity": owner(),
			"Disclosure": map[string]any{
				"Enabled": true, "Name": "Example Reputation",
				"Endpoint": "https://feed.example/v1/check",
				"Method":   "POST", "TimeoutSeconds": 2.0,
				"Webhooks": map[string]any{"Receiving": true, "Count": 2},
			},
		},
		// Every invitation state in one render — pending with a Revoke button,
		// and the three that have none — plus the freshly created panel, which
		// is the only place the token is ever shown.
		"invites": map[string]any{
			"Title": "Invitations", "Nav": "invites", "Identity": owner(),
			"Invitations": []map[string]any{
				{
					"ID": "0198c9c5-0000-7000-8000-000000000007", "Email": "new@example.com",
					"Role": "editor", "InvitedBy": "o@example.com", "Status": "pending",
					"CreatedAt": now, "ExpiresAt": now.Add(168 * time.Hour),
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000008", "Email": "gone@example.com",
					"Role": "viewer", "InvitedBy": "", "Status": "revoked",
					"CreatedAt": now, "ExpiresAt": now,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-000000000009", "Email": "old@example.com",
					"Role": "viewer", "InvitedBy": "o@example.com", "Status": "expired",
					"CreatedAt": now, "ExpiresAt": now,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-00000000000a", "Email": "joined@example.com",
					"Role": "admin", "InvitedBy": "o@example.com", "Status": "redeemed",
					"CreatedAt": now, "ExpiresAt": now,
				},
			},
			"RoleOptions": []map[string]any{
				{"Slug": "admin", "Name": "Admin", "Description": "Manage links and members.", "Rank": 20},
				{"Slug": "editor", "Name": "Editor", "Description": "Create and edit links.", "Rank": 30},
			},
			"Created": map[string]any{
				"Email": "new@example.com", "Role": "editor", "Emailed": false,
				"URL": "http://links.test/invite/2ZQ3jd0eGkEaBcDeFgHiJkLmNoPqRsTuVwXyZ012",
			},
			"Form":        map[string]string{"Email": "", "Role": "editor"},
			"FieldErrors": map[string]string{},
			"Notice":      "", "Error": "", "MailConfigured": false,
		},
		// Every branch the member list has, in one render: a row the viewer may
		// manage, their own row (never manageable, which is what "an admin
		// cannot demote themselves" looks like on the page), a row above them,
		// and a workspace-scoped membership — the one whose "reaches" column
		// says something other than "every workspace".
		"members": map[string]any{
			"Title": "Members", "Nav": "members", "Identity": owner(),
			"CanWrite": true,
			"Members": []map[string]any{
				{
					"ID": "0198c9c5-0000-7000-8000-00000000000b", "UserID": "0198c9c5-0000-7000-8000-00000000001b",
					"Email": "o@example.com", "Name": "Owner", "Role": "owner", "RoleRank": 10,
					"WorkspaceID": nil, "WorkspaceName": "",
					"Manageable": true, "IsSelf": true, "CreatedAt": now,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-00000000000c", "UserID": "0198c9c5-0000-7000-8000-00000000001c",
					"Email": "admin@example.com", "Name": "", "Role": "admin", "RoleRank": 20,
					"WorkspaceID": nil, "WorkspaceName": "",
					"Manageable": true, "IsSelf": false, "CreatedAt": now,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-00000000000d", "UserID": "0198c9c5-0000-7000-8000-00000000001c",
					"Email": "admin@example.com", "Name": "", "Role": "editor", "RoleRank": 30,
					"WorkspaceID": "0198c9c5-0000-7000-8000-000000000011", "WorkspaceName": "Marketing",
					"Manageable": true, "IsSelf": false, "CreatedAt": now,
				},
				{
					"ID": "0198c9c5-0000-7000-8000-00000000000e", "UserID": "0198c9c5-0000-7000-8000-00000000001e",
					"Email": "untouchable@example.com", "Name": "", "Role": "owner", "RoleRank": 10,
					"WorkspaceID": nil, "WorkspaceName": "",
					"Manageable": false, "IsSelf": false, "CreatedAt": now,
				},
			},
			"RoleOptions": []map[string]any{
				{"Slug": "admin", "Name": "Admin", "Description": "Manage links and members.", "Rank": 20},
				{"Slug": "editor", "Name": "Editor", "Description": "Create and edit links.", "Rank": 30},
			},
			// The page's own list, in the page's own type. Keyed apart from the
			// shell's Workspaces on purpose: while both were called Workspaces
			// the loop below overwrote this entry with the switcher's shape, so
			// the test rendered a struct production never builds and both team
			// pages shipped answering 500 (F20).
			"OrgWorkspaces": []map[string]any{
				{"ID": "0198c9c5-0000-7000-8000-000000000010", "Name": "Default", "Slug": "default",
					"Current": true, "Manageable": true},
			},
			"GrantTargets": []map[string]any{
				{"UserID": "0198c9c5-0000-7000-8000-00000000001c", "Email": "admin@example.com", "Role": "admin"},
			},
			"FieldErrors": map[string]string{},
			"Notice":      "", "Error": "",
		},
		// A workspace the reader may manage and one they may not, plus the
		// organization form — which only an account holding orgs.create ever
		// sees, and which this fixture therefore turns on.
		"workspaces": map[string]any{
			"Title": "Workspaces", "Nav": "workspaces", "Identity": owner(),
			"CanWrite": true, "CanCreateOrganization": true,
			// org.delete is the owner's alone, and M28.5 gave it its first
			// operation. Turned on here so the deletion section renders — it is
			// the only place in the product that offers it.
			"CanDeleteOrganization": true,
			"OrganizationID":        "0198c9c5-0000-7000-8000-000000000020",
			"OrganizationName":      "Owner",
			// Two of them, in the page's own type, alongside the shell's two in
			// the switcher's — which is exactly the pair the product builds and
			// the pair this fixture used to collapse into one. See the note on
			// the members entry above.
			"OrgWorkspaces": []map[string]any{
				{"ID": "0198c9c5-0000-7000-8000-000000000010", "Name": "Default", "Slug": "default",
					"Current": true, "Manageable": true},
				{"ID": "0198c9c5-0000-7000-8000-000000000011", "Name": "Marketing", "Slug": "marketing",
					"Current": false, "Manageable": false},
			},
			"Form":           map[string]string{"Name": "", "OrganizationName": ""},
			"FieldErrors":    map[string]string{"name": "a workspace in this organization already uses that name"},
			"OrgFieldErrors": map[string]string{},
			"Notice":         "", "Error": "",
		},
		// The page for an account that belongs to nothing (D36). Its shell is
		// the one place HasOrganization is false, which is fixed up below the
		// loop: the header has to draw without its destinations, and every other
		// page has to keep drawing with them.
		"organization_new": map[string]any{
			"Title": "Create an organization", "Nav": "", "Identity": owner(),
			"Form":        map[string]string{"Name": ""},
			"FieldErrors": map[string]string{},
			"Error":       "",
		},
		// The public half. No identity at all, which is the state it is designed
		// for: whoever opens it may have no account yet.
		"invite": map[string]any{
			"Title": "Invitation", "Nav": "", "Identity": (*identityStub)(nil),
			"Token": "2ZQ3jd0eGkEaBcDeFgHiJkLmNoPqRsTuVwXyZ012",
			"Valid": true,
			"Offer": map[string]any{
				"OrganizationName": "Acme", "Role": "editor",
				"ExpiresAt": now.Add(168 * time.Hour),
			},
			"Form":        map[string]string{"Email": "", "Name": ""},
			"FieldErrors": map[string]string{},
			"Error":       "", "NewAccounts": true,
		},
		// Self-serve signup (M29), and no identity either: whoever opens this
		// has no account by definition. The form state rather than the "check
		// your inbox" state, because it is the branch with everything in it.
		"signup": map[string]any{
			"Title": "Create an account", "Nav": "", "Identity": (*identityStub)(nil),
			"Sent": false, "Email": "",
			"Form":        map[string]string{"Email": "someone@example.com", "Name": ""},
			"FieldErrors": map[string]string{"password": "the password must be at least 12 characters"},
			"Error":       "That email address is already registered.",
		},
		// The confirmation the emailed link lands on, in the state that has the
		// button: an error renders a link instead, which is two lines.
		"verify": map[string]any{
			"Title": "Confirm your address", "Nav": "", "Identity": (*identityStub)(nil),
			"Token": "2ZQ3jd0eGkEaBcDeFgHiJkLmNoPqRsTuVwXyZ012",
			"Error": "",
		},
	}

	// Shell fields every page carries, supplied once rather than in each entry
	// so a page added later cannot forget one and fail for a reason that has
	// nothing to do with the page. Theme is on the layout, Path on the
	// appearance and workspace controls, Workspaces on the switcher,
	// UnreadPreview on the notification bell.
	//
	// The preview is populated on every page rather than only where it is the
	// subject, because the bell is drawn from the shell everywhere: a template
	// error inside it breaks every page, not one.
	for _, d := range data {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		m["Theme"] = ""
		m["Path"] = "/dashboard"
		m["Workspaces"] = twoWorkspaces()
		m["UnreadPreview"] = unreadPreview(now)
		m["HasOrganization"] = true
	}
	// The one exception, set after the loop rather than inside the entry so it
	// cannot be quietly overwritten by it: this page exists precisely because an
	// account can belong to nothing, and rendering it with a full header would
	// exercise the wrong half of the layout.
	organizationNew, _ := data["organization_new"].(map[string]any)
	organizationNew["HasOrganization"] = false
	organizationNew["Workspaces"] = []map[string]any{}

	// The links area's own bar reads .Path for its active entry (M46), and the
	// loop above gives every page /dashboard. Left at that, all four links pages
	// would render the bar with nothing current — which renders fine and asserts
	// nothing, so the fixture carries the path each page is actually served at.
	// /links/{id} collapses to /links on the shell (switchTarget), which is also
	// the right answer for this bar: a link's page is inside Links.
	for page, path := range map[string]string{
		"links":       "/links",
		"link_detail": "/links",
		"campaigns":   "/campaigns",
		"folders":     "/folders",
	} {
		m, _ := data[page].(map[string]any)
		m["Path"] = path
	}
	return data
}

// reviewerRoster is the instance's dispute reviewers, as both surfaces that
// render them see them (M45; two surfaces since M48).
//
// Two rows, because the roster's interesting branch is the one that is *not* the
// reader: it carries a Withdraw control and the other says "you". A single-row
// fixture would leave half the section unrendered, which is the same trap
// twoWorkspaces avoids.
//
// A function rather than a literal in each entry, so the queue's summary and the
// panel's page are rendered from the same rows. Two copies would let one drift
// and still render.
func reviewerRoster(now time.Time, grantedBy *string) []map[string]any {
	return []map[string]any{
		{
			"UserID": "0198c9c5-0000-7000-8000-000000000001",
			"Email":  "o@example.com", "Name": "Owner",
			"GrantedAt": now, "GrantedBy": (*string)(nil), "CanDecide": true,
		},
		{
			"UserID": "0198c9c5-0000-7000-8000-000000000002",
			"Email":  "admin@example.com", "Name": "Admin",
			"GrantedAt": now, "GrantedBy": grantedBy, "CanDecide": true,
		},
	}
}

// unreadPreview is the bell's data: two unread notifications, one with a body
// and one without, so both branches of the item template render.
//
// **Their Targets differ, and that is the point of the pair since M48.** One
// leads somewhere and renders as a submit button; the other leads nowhere and
// stays text, which is the branch a kind like audit.growth actually takes. A
// fixture where both had a destination would render half the template.
func unreadPreview(now time.Time) []map[string]any {
	return []map[string]any{
		{
			"ID": "0198c9c5-0000-7000-8000-000000000005", "Kind": "audit.growth",
			"Title":  "The audit log has passed its size threshold",
			"Body":   "audit_logs now uses 5.2 GiB on disk.",
			"Target": "", "CreatedAt": now,
		},
		{
			"ID": "0198c9c5-0000-7000-8000-000000000006", "Kind": "dispute.filed",
			"Title": "A notice with no body", "Body": "",
			"Target":    "/disputes?all=1#dispute-0198c9c5-0000-7000-8000-000000000030",
			"CreatedAt": now,
		},
	}
}

func TestEveryPageRenders(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	data := pageData(t)

	for _, page := range r.Pages() {
		t.Run(page, func(t *testing.T) {
			d, ok := data[page]
			if !ok {
				t.Fatalf("no test data for page %q; add an entry to pageData so the template is exercised", page)
			}
			rec := httptest.NewRecorder()
			if err := r.Render(rec, http.StatusOK, page, d); err != nil {
				t.Fatalf("render %s: %v", page, err)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "<!doctype html>") {
				t.Error("page did not go through the layout")
			}
			if !strings.Contains(body, "</html>") {
				t.Error("page is truncated")
			}
		})
	}
}

func TestRenderBuffersFailures(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	// The dashboard's data shape is wrong on purpose. The failure must surface
	// as an error with nothing written, not as a 200 with half a page.
	if err := r.Render(rec, http.StatusOK, "dashboard", struct{}{}); err == nil {
		t.Fatal("rendering with the wrong data shape reported success")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a failed render wrote %d bytes; partial HTML reached the client", rec.Body.Len())
	}
}

func TestRenderPartialProducesFragment(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	err = r.RenderPartial(rec, http.StatusOK, "links", "links_table", pageData(t)["links"])
	if err != nil {
		t.Fatalf("render partial: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Error("partial rendered the whole layout; htmx would nest a page inside the page")
	}
	if !strings.Contains(body, `id="links-table"`) {
		t.Error("partial did not include the swap target")
	}
}

func TestStaticHandler(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	h := r.StaticHandler("/static/")

	get := func(path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	url := r.AssetURL("js/htmx.min.js")
	if !strings.Contains(url, "?v=") {
		t.Fatalf("asset URL %q carries no fingerprint", url)
	}

	rec := get(url, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fingerprinted asset returned %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("fingerprinted URL Cache-Control = %q, want immutable", cc)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		// text/plain here means browsers refuse to run it under nosniff.
		t.Errorf("Content-Type = %q", ct)
	}

	plain := get("/static/js/htmx.min.js", nil)
	if cc := plain.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("unfingerprinted URL is immutable (%q); it cannot be cache-busted", cc)
	}

	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a static asset")
	}
	if rec := get(url, map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Errorf("matching If-None-Match returned %d, want 304", rec.Code)
	}

	if rec := get("/static/nope.css", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset returned %d, want 404", rec.Code)
	}
	// The Tailwind entry point is a build input, not something to serve.
	if rec := get("/static/css/input.css", nil); rec.Code != http.StatusNotFound {
		t.Errorf("input.css is being served (%d); only built output belongs here", rec.Code)
	}
}

func TestBarChartGeometry(t *testing.T) {
	points := []DayCount{
		{Day: "2026-07-01", Clicks: 10},
		{Day: "2026-07-02", Clicks: 0},
		{Day: "2026-07-03", Clicks: 7},
	}
	c := BarChart(points, 720, 160)

	if len(c.Bars) != 3 {
		t.Fatalf("bars = %d, want 3", len(c.Bars))
	}
	for _, b := range c.Bars {
		if b.X < 0 || b.X+b.W > c.W {
			t.Errorf("bar for %s overflows the viewBox: x=%d w=%d", b.Day, b.X, b.W)
		}
		if b.Y < 0 || b.Y+b.H > c.PlotH {
			t.Errorf("bar for %s overflows the plot: y=%d h=%d", b.Day, b.Y, b.H)
		}
	}
	if c.Bars[1].H != 0 {
		t.Error("a zero-click day drew a bar")
	}
	if c.Bars[0].H <= c.Bars[2].H {
		t.Error("10 clicks drew a shorter bar than 7")
	}
	if c.MaxY != 10 {
		t.Errorf("axis max = %d, want the nice ceiling 10", c.MaxY)
	}

	// One click against a huge axis must still be visible.
	tiny := BarChart([]DayCount{{Day: "2026-07-01", Clicks: 1}, {Day: "2026-07-02", Clicks: 10000}}, 720, 160)
	if tiny.Bars[0].H < 1 {
		t.Error("a nonzero day rendered invisibly")
	}

	if empty := BarChart(nil, 720, 160); len(empty.Bars) != 0 {
		t.Error("an empty series produced bars")
	}
}

func TestNiceCeil(t *testing.T) {
	cases := map[int64]int64{
		0: 1, 1: 1, 2: 2, 3: 5, 7: 10, 10: 10, 11: 20,
		49: 50, 51: 100, 100: 100, 8347: 10000,
	}
	for in, want := range cases {
		if got := niceCeil(in); got != want {
			t.Errorf("niceCeil(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestFmtInt(t *testing.T) {
	cases := map[int64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000",
		1234567: "1,234,567", -4321: "-4,321",
	}
	for in, want := range cases {
		if got := fmtInt(in); got != want {
			t.Errorf("fmtInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPct(t *testing.T) {
	if got := pct(1, 1000); got != 1 {
		t.Errorf("a tiny nonzero share = %d, want the 1%% floor", got)
	}
	if got := pct(500, 1000); got != 50 {
		t.Errorf("pct(500,1000) = %d", got)
	}
	if got := pct(5, 0); got != 0 {
		t.Errorf("division by zero total = %d, want 0", got)
	}
	if got := pct(2000, 1000); got != 100 {
		t.Errorf("over-total share = %d, want clamped 100", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo wörld", 6); got != "héllo…" {
		t.Errorf("truncate = %q; multibyte runes must not be split", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate lengthened a short string: %q", got)
	}
}
