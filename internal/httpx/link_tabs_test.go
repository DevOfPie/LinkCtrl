package httpx

import (
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// TestAttachTabBadgesReadsWhatTheSectionsShow is M47.5's assembly, asserted at
// the seam where a badge would start to lie.
//
// The milestone's own risk section is the reason this exists in this package
// as well as in internal/ui: the badges "are only as good as the six values
// behind them", and the ui tests render values a fixture handed them, so a
// wrong assembly would render beautifully. The first wrong assembly was found
// by the kept spec within an hour of being written — GetSplit answers a link
// with no split with an *empty* Split, not a nil one, and a nil test drew five
// badges where the strip claims six. That case is pinned here by name.
func TestAttachTabBadgesReadsWhatTheSectionsShow(t *testing.T) {
	badge := func(data *linkDetailPageData, id string) linkTab {
		t.Helper()
		attachTabBadges(data)
		for _, tab := range data.Tabs {
			if tab.ID == id {
				return tab
			}
		}
		t.Fatalf("no %s tab in the fixture strip", id)
		return linkTab{}
	}
	// The full strip, as linkTabs builds it for an identity holding every
	// permission. Stated literally because Identity's permission set is
	// unexported and rightly so; which tabs an identity is offered is the
	// integration tests' subject, and this test's is what the badges read.
	tabs := func() []linkTab {
		return []linkTab{
			{ID: "edit", Label: "Edit"}, {ID: "qr", Label: "QR"},
			{ID: "routing", Label: "Routing"}, {ID: "split", Label: "Split"},
			{ID: "signed", Label: "Signed"}, {ID: "analytics", Label: "Analytics"},
			{ID: "danger", Label: "Danger"},
		}
	}

	// A link nobody has configured: every count reads 0 — a claim, not a
	// blank — and both binaries read the cross. The Split here is the shape
	// GetSplit actually returns for such a link: empty, and not nil.
	bare := &linkDetailPageData{
		Link:  &domain.Link{},
		Split: &domain.Split{},
		Stats: &analytics.LinkStats{},
	}
	bare.Tabs = tabs()
	for id, want := range map[string]linkTab{
		"edit":      {Badge: "count", Count: 0},
		"qr":        {Badge: "count", Count: 0},
		"routing":   {Badge: "count", Count: 0},
		"split":     {Badge: "cross"},
		"signed":    {Badge: "cross"},
		"analytics": {Badge: "count", Count: 0},
		"danger":    {Badge: ""},
	} {
		got := badge(bare, id)
		if got.Badge != want.Badge || got.Count != want.Count {
			t.Errorf("unconfigured link, %s tab: badge %q count %d, want %q %d",
				id, got.Badge, got.Count, want.Badge, want.Count)
		}
	}

	// The same strip over a configured link: the counts are the sections'
	// own lengths, the split badge is the kind itself, and signed reads the
	// stored RequireSignature — not the transient minted URL, which is shown
	// once and never stored, and not the form in flight.
	set := &linkDetailPageData{
		Link: &domain.Link{
			HasPassword: true, OneTime: true, RequireSignature: true,
			ForwardQuery: true, ForwardPath: true,
		},
		Split: &domain.Split{Kind: domain.RuleKindSequential},
		Stats: &analytics.LinkStats{Totals: analytics.Totals{Clicks: 40}},
	}
	set.Rules = []ruleView{{}, {}}
	set.QRCodes = []qrCodeView{{}, {}}
	set.Tabs = tabs()
	for id, want := range map[string]linkTab{
		"edit":      {Badge: "count", Count: 5},
		"qr":        {Badge: "count", Count: 2},
		"routing":   {Badge: "count", Count: 2},
		"split":     {Badge: "sequential"},
		"signed":    {Badge: "check"},
		"analytics": {Badge: "count", Count: 40},
		"danger":    {Badge: ""},
	} {
		got := badge(set, id)
		if got.Badge != want.Badge || got.Count != want.Count {
			t.Errorf("configured link, %s tab: badge %q count %d, want %q %d",
				id, got.Badge, got.Count, want.Badge, want.Count)
		}
	}
}
