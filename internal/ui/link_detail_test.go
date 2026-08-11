package ui

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The three claims M47 makes about the link page, each asserted where a reader
// can see it fail.

// linkDetailLineCap is what stops this page becoming a monolith again.
//
// **How the number was chosen, since m47.md says it has to be, and it was
// chosen from the decomposition rather than before it.** After the
// decomposition `pages/link_detail.html` is 50 lines: a header block, two
// flashes, and one guarded `{{template}}` line per section. The cap is 60 —
// ten lines of headroom, which is three more sections at a guard line and a
// line or two of the order comment each.
//
// **Ten, because the shortest section is twenty-one.** What the cap has to
// refuse is a section landing in this file instead of in a partial, and what
// that costs is the section's *body* — the markup between `{{define}}` and
// `{{end}}`, which is what a paste would carry. The shortest of the eleven
// bodies this page renders is `link_signed`'s at 21 lines, and the longest is
// `link_edit`'s at 201. So inlining even the smallest of them overruns by
// eleven lines and inlining the largest by 191, while ordinary growth has room.
// Measured, not estimated: inlining `link_signed` during the build took the
// file to 72 against a cap that then stood at 70, which is how this margin came
// to be stated in bodies rather than in whole files.
//
// Set at 90 the cap would enforce nothing; set at 52 it would forbid a comment,
// which is the failure m47.md's risk section names — a partial that exists only
// to satisfy a number.
//
// It bounds this page and no other, because this page is the one with a
// measurement behind it. A general cap across `pages/` would be a number nobody
// derived, applied to templates nobody complained about.
const linkDetailLineCap = 60

// TestTheLinkPageStaysDecomposed is M24.5's idiom applied to structure rather
// than to colour: a scan over what actually ships, failing the build rather
// than relying on review noticing that a section grew back.
//
// Over the embedded copy, like every other scan in this package, because the
// embed is what the binary serves.
func TestTheLinkPageStaysDecomposed(t *testing.T) {
	const page = "templates/pages/link_detail.html"

	b, err := fs.ReadFile(files, page)
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	if n := len(strings.Split(strings.TrimRight(string(b), "\n"), "\n")); n > linkDetailLineCap {
		t.Errorf("%s is %d lines, and the cap is %d.\n\n"+
			"This page was 805 lines before M47 took it apart, and the cap is what "+
			"keeps it apart: it sits below the length of the shortest partial the "+
			"page renders, so no section can be inlined here without breaking it. "+
			"Put the new section in internal/ui/templates/partials/ and invoke it "+
			"from the ordered list. Raising the cap is a decision about the page's "+
			"structure and belongs in decisions.md, not in this constant.",
			page, n, linkDetailLineCap)
	}
}

// unboundedHeight are the tags whose rendered height cannot be reasoned about
// from markup at all: a table is as tall as its rows, a list as tall as its
// items, an image as tall as whatever the server sends back.
//
// Text is deliberately absent. A paragraph's height is bounded by how much text
// is in it, and the character budget below is what bounds that.
//
// **`<svg` was on this list and has been taken off it, and that is a narrowing
// and not a hole.** The reason the list refuses a tag is written into the
// failure message — *"its height cannot be read off the markup"* — and that
// sentence is false of an `<svg>` carrying a height class, which states the
// rendered box in the markup exactly the way a character count states a
// paragraph's. So the refusal becomes a rule about what the tag says rather
// than about the tag: statedHeight below is that rule, and it is stricter than
// membership of this list in the one direction that matters, because it also
// adds up what it finds. The owner set this on 2026-08-07 against a milestone
// that wanted an exemption for one element; decisions.md carries why the
// difference is worth the paragraph.
var unboundedHeight = []string{
	"<table", "<img", "<iframe", "<video", "<canvas", "<ul", "<ol", "<details",
}

// svgTag captures the attributes of an `<svg>` opening tag.
var svgTag = regexp.MustCompile(`<svg\b([^>]*)>`)

// classList pulls the class attribute out of a stretch of attributes.
var classList = regexp.MustCompile(`\bclass="([^"]*)"`)

// statedHeight reads the rendered height an `<svg>`'s classes declare, in CSS
// pixels, and reports whether they declare one at all.
//
// **Only the utilities that name a fixed length count.** `h-24` is six rem and
// is six rem on every page that draws it; `h-full`, `h-screen`, `h-auto`,
// `h-min`, `h-max` and `h-fit` are each a height decided by something outside
// the element — the parent, the viewport, the content — which is the situation
// the guard exists to refuse, spelled differently. Tailwind arbitrary values
// (`h-[6rem]`) are refused too, and for a smaller reason: they are a second
// syntax to parse here for a box the spacing scale can already state.
//
// The scale is Tailwind's: one unit is 0.25rem, and rem is 16px because nothing
// in this product changes the root font size. `h-px` is the one exception the
// scale itself makes, and it is a pixel.
func statedHeight(attrs string) (int, bool) {
	m := classList.FindStringSubmatch(attrs)
	if m == nil {
		return 0, false
	}
	for _, class := range strings.Fields(m[1]) {
		n, ok := strings.CutPrefix(class, "h-")
		if !ok {
			continue
		}
		if n == "px" {
			return 1, true
		}
		units, err := strconv.ParseFloat(n, 64)
		if err != nil {
			continue
		}
		return int(units * 4), true
	}
	return 0, false
}

// linkDetailPrefixPixelBudget bounds the declared height of the pictures the
// page may draw in front of the destination box, and it is the second half of
// the rule that replaced `<svg`'s place on the list above.
//
// **A rule that read a height and then ignored it would be a rule that asks for
// a number for its own sake.** So the heights are added up and compared to what
// the measurement says there is room for. The QR thumbnail declares 96px
// (`ui.QRThumbClass`, h-24) and is the only picture up there.
//
// **Re-measured with it in place, which M48 did rather than assume.** At
// 1280×800, in Blink, Gecko and WebKit identically: the destination box's top
// edge sits at **349px** where M47 measured 327, and the alias field's bottom
// edge at **465px** where M47 measured 443 — 22px of movement for a 96px
// picture, because it sits *beside* the heading rather than above it and the
// heading block was already 74px of text. Both controls are inside the viewport
// with **335px** to spare.
//
// 160 spends 64px of that slack and keeps 271px, which is the same shape of
// margin the character budget keeps. It refuses a second thumbnail beside the
// first (192px), it refuses one picture more than two-thirds again as tall as
// this one, and it does not have to be raised for a taller code — the class is
// the same for every link, which is the whole reason the class is what is read.
const linkDetailPrefixPixelBudget = 160

// linkDetailPrefixBudget bounds the visible text the page may draw in front of
// the destination box.
//
// 400 characters. The page draws 93 today — the back link, the alias, the
// status, the short URL, the destination, and the edit card's two labels — and
// `<main>` is 1120px of content at 1280px, which at text-sm is roughly 160
// characters a line. So 400 leaves room for a long alias and a long destination
// and refuses a paragraph of new prose, which is the thing that would quietly
// push the control down.
//
// It bounds the *markup*, measured against this package's fixture. It is not a
// bound on data: a two-thousand-character destination URL wraps to about ten
// lines and pushes the control down by roughly 200px, which the measurement
// below has 470px of room for.
const linkDetailPrefixBudget = 400

var htmlTag = regexp.MustCompile(`(?s)<[^>]*>`)

// TestTheEditControlIsReachableWithoutScrolling is M47's first bullet.
//
// **The pixel claim was measured, and this test is not the measurement.**
// At 1280×800, in Blink, Gecko and WebKit identically: the destination box's top
// edge moved from **1883px to 327px** and the alias field's bottom edge sat at
// **443px**, so both are inside the viewport where before they were a screen and
// a half below it.
//
// **Measured a second time at M48**, when the QR thumbnail went into the heading
// row, because a claim that survives a change survives it by measurement and not
// by argument: 327 became **349px** and 443 became **465px**, in all three
// engines, leaving 335px of viewport below the alias. M47 is not reopened — its
// claim is what was re-checked, and it holds.
//
// The harness that took both sets of figures is not committed, for the reason
// M46's was not — `tools/render-verify` is opt-in and reachable from no gate, so
// a pixel assertion living there would protect nothing between the two times
// somebody ran it. decisions.md carries the numbers.
//
// What this test asserts is the structural property that measurement rests on,
// in the three directions that can regress:
//
//   - the destination and the alias are the **first two** controls the page
//     draws, so nothing interactive can be put in front of them;
//   - what is in front of them is text, under a character budget, so no
//     paragraph can be put there either;
//   - and a picture in front of them states its own height in a class and stays
//     inside a pixel budget, so it cannot grow with the data it draws.
//
// Stated rather than implied, in m46.md's idiom: this does not check pixels and
// cannot. A section whose own height grew would pass it. The cap on this file's
// length and the ordering above are what make that unlikely; the measurement is
// what makes it false today.
func TestTheEditControlIsReachableWithoutScrolling(t *testing.T) {
	page := mainOf(t, renderPage(t, "link_detail", nil))

	got := namedControls(page)
	if len(got) < 2 || got[0] != "url" || got[1] != "alias" {
		t.Fatalf("the first controls on the link page are %v, want url and alias "+
			"first; changing where a link points is the most ordinary thing anybody "+
			"does to a link and it took ~35 seconds in a blind task because it was "+
			"1883px down the page", first(got, 4))
	}

	at := strings.Index(page, `id="url"`)
	if at < 0 {
		t.Fatal("the link page draws no destination box")
	}
	prefix := page[:at]

	for _, tag := range unboundedHeight {
		if strings.Contains(prefix, tag) {
			t.Errorf("a %s> is rendered in front of the destination box. Its height "+
				"cannot be read off the markup, so it can push the control below the "+
				"fold without failing anything here. The analytics are below the "+
				"configuration on this page and that is the milestone; put it there.",
				tag)
		}
	}

	// The narrowed rule: a drawing may go up there, and it has to say how tall
	// it is and stay inside the budget.
	pixels := 0
	for _, m := range svgTag.FindAllStringSubmatch(prefix, -1) {
		px, ok := statedHeight(m[1])
		if !ok {
			t.Errorf("an <svg> is rendered in front of the destination box with no "+
				"height class:\n  <svg%s>\n\nIts height is then whatever the drawing "+
				"turns out to be — for a QR code, whatever version the URL encodes "+
				"to — so it can push the control below the fold without failing "+
				"anything here. Give it a fixed height utility (h-24, and w-24 "+
				"beside it if it is square), or put the picture below the "+
				"configuration where the analytics are.", m[1])
			continue
		}
		pixels += px
	}
	if pixels > linkDetailPrefixPixelBudget {
		t.Errorf("the link page draws %dpx of pictures in front of the destination "+
			"box and the budget is %dpx.\n\nThe box was measured at 349px from the "+
			"top of a 1280×800 viewport with one 96px thumbnail up there. Every "+
			"pixel added here moves it down by one, and the budget is what stops "+
			"the heading row becoming a section.", pixels, linkDetailPrefixPixelBudget)
	}

	text := strings.TrimSpace(htmlTag.ReplaceAllString(prefix, " "))
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > linkDetailPrefixBudget {
		t.Errorf("the link page draws %d characters of text in front of the "+
			"destination box and the budget is %d:\n  %s\n\nAt 1280px that is more "+
			"than two extra lines of prose above a control the milestone measured at "+
			"327px from the top of the viewport.",
			len(text), linkDetailPrefixBudget, text)
	}
}

// first is the head of a slice, for an error message that does not print
// forty control names.
func first(ss []string, n int) []string {
	if len(ss) < n {
		return ss
	}
	return ss[:n]
}

// sentences splits a stretch of visible text on the punctuation that ends a
// sentence. Crude on purpose: it is used to assert that two numbers are in the
// *same* one, and the only way to be wrong about that is to under-split, which
// this cannot do — every terminator is a boundary.
var sentences = regexp.MustCompile(`[.!?]\s`)

// TestTheClickLimitNamesTheTotalAndWhatIsSpent is the one label M47 changed,
// and the reason it changed.
//
// The field read *Click limit (empty = none)* with *"416 used so far"* in a
// separate element six lines below it. In the blind task the owner set a limit
// and wrote: *"Unsure if done properly as nothing notes whether the field is
// setting for additional clicks or total clicks. Current clicks was 416, was
// setting the field to 50 or 466 correct?"*
//
// **Both facts were already on the page, adjacent, and still did not compose.**
// So the assertion is not that both numbers appear — they did before — but that
// they appear in **one sentence**, which is the only arrangement in which a
// reader does not have to relate two elements themselves.
//
// The gate's behaviour is not asserted here because it did not change:
// `MaxClicks` stays absolute, and changing it would silently redefine every
// limit already set.
func TestTheClickLimitNamesTheTotalAndWhatIsSpent(t *testing.T) {
	page := mainOf(t, renderPage(t, "link_detail", nil))

	at := strings.Index(page, `id="max_clicks"`)
	if at < 0 {
		t.Fatal("the link page draws no click-limit control")
	}
	// **Past the input's own closing bracket**, not from the attribute. The box
	// carries `value="466"`, so a region starting inside the tag would let the
	// limit be "named" by markup the reader never sees — which is the assertion
	// passing for exactly the reason the defect existed.
	_, after, ok := strings.Cut(page[at:], ">")
	if !ok {
		t.Fatal("the click-limit box's tag is never closed")
	}
	end := strings.Index(after, "</div>")
	if end < 0 {
		t.Fatal("the click-limit control is never closed")
	}
	text := strings.Join(strings.Fields(
		htmlTag.ReplaceAllString(after[:end], " ")), " ")

	var found string
	for _, s := range sentences.Split(text, -1) {
		if strings.Contains(s, "416") && strings.Contains(s, "466") {
			found = s
			break
		}
	}
	switch {
	case found == "":
		t.Errorf("no single sentence beside the click-limit box names both the 416 "+
			"already spent and the 466 the box is setting. What it says is:\n  %s\n\n"+
			"Both numbers were on the page before M47, in two elements, and the "+
			"owner still could not tell whether the field wanted 50 or 466. Two "+
			"adjacent facts a reader has to relate is the defect; one sentence is "+
			"the fix.", text)
	case !strings.Contains(found, "total"):
		// A sentence holding both numbers and nothing between them would pass the
		// check above and answer nothing: 466 and 416 in one breath is what the
		// page already did across two elements.
		t.Errorf("the sentence names both numbers without saying the limit is a "+
			"total:\n  %s", found)
	}
}

// TestTheLinkPageDrawsOneSectionAtATime is the reopened M47's central bullet.
//
// F205: the first pass re-ordered nine sections and left the page a
// nine-section vertical stack, so ordering decided which section is expensive
// to reach and never how many are. The claim now is structural — the page
// renders **one** section panel, selected by a tab strip — and it is asserted
// in both directions for every tab: the active section's marker is present and
// every other section's is absent, because a dispatch that quietly rendered two
// panels would pass any one-sided check and rebuild the stack a tab at a time.
//
// The QR tab's marker is the section's own prose, not the panel's, because the
// panel deliberately renders on every tab: the thumbnail in the heading row
// invokes it from wherever the reader is standing, and a popover is not in any
// tab's flow. Recent activity is asserted as the Analytics tab's tail — eight
// sections behind seven tabs is the owner-set fold, and a strip that grew an
// eighth tab or an activity table that went unrendered would both surface here.
func TestTheLinkPageDrawsOneSectionAtATime(t *testing.T) {
	markers := map[string]string{
		"edit":      `id="url"`,
		"qr":        "Scans are counted as ordinary clicks",
		"routing":   ">Routing rules</h2>",
		"split":     ">Split test</h2>",
		"signed":    ">Signed links</h2>",
		"analytics": ">Clicks per day</h2>",
		"danger":    ">Danger zone</h2>",
	}

	for _, tab := range linkDetailTabs {
		page := mainOf(t, renderPage(t, "link_detail", map[string]any{"Tab": tab}))

		if !strings.Contains(page, markers[tab]) {
			t.Errorf("the %s tab does not render its own section (missing %q)",
				tab, markers[tab])
		}
		for other, marker := range markers {
			if other != tab && strings.Contains(page, marker) {
				t.Errorf("the %s tab also renders the %s section (%q). One panel at "+
					"a time is the milestone; two is the stack growing back a tab at "+
					"a time.", tab, other, marker)
			}
		}

		// The strip itself survives every selection: all seven tabs, in the
		// owner-set order, and exactly one of them marked current.
		last := -1
		for _, id := range linkDetailTabs {
			at := strings.Index(page, `?tab=`+id+`"`)
			if at < 0 {
				t.Errorf("on the %s tab the strip offers no %s tab", tab, id)
				continue
			}
			if at < last {
				t.Errorf("on the %s tab the strip draws %s out of the owner-set order", tab, id)
			}
			last = at
		}
		if !strings.Contains(page, `?tab=`+tab+`" aria-current="page"`) {
			t.Errorf("the %s tab's own entry in the strip is not marked current", tab)
		}
		if n := strings.Count(page, `aria-current="page"`); n != 1 {
			t.Errorf("the %s tab marks %d strip entries current, want exactly 1", tab, n)
		}

		// Activity folds into Analytics: present exactly there.
		if has := strings.Contains(page, ">Recent activity</h2>"); has != (tab == "analytics") {
			t.Errorf("the %s tab renders recent activity = %v; it is the Analytics "+
				"tab's tail and nobody else's", tab, has)
		}
	}

	// The mechanics the design names: the click is an htmx swap in the
	// workspace switcher's pattern, each tab is a real URL, and the strip
	// scrolls sideways instead of wrapping. What that *looks like* at 360px is
	// the kept spec's to assert (tools/agent-browser/specs/link-tabs.spec.mjs);
	// what is asserted here is that the mechanism is in the markup at all.
	page := mainOf(t, renderPage(t, "link_detail", nil))
	for _, want := range []string{
		`hx-target="#link-tabs"`, `hx-select="#link-tabs"`,
		`hx-swap="outerHTML"`, `hx-push-url="true"`,
		`aria-label="Link sections" class="flex overflow-x-auto whitespace-nowrap`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the strip is missing %s", want)
		}
	}
}

// linkTabBadgeSize is the glyph height the strip's icon badges declare, as a
// Tailwind fixed-length utility.
//
// **The number is measured, not chosen.** The wireframes drew 9px and 11px;
// the prefix pixel budget above refuses arbitrary utilities, so the real
// choices were h-2.5 (10px) and h-3 (12px), and which one is M46.5's
// three-engine browser check's to answer. The answer — h-3, in Blink, Gecko
// and WebKit, with what was seen — is recorded in decisions.md under M47.5.
// This constant is what ties that recorded answer to the tree: change the
// class in icons.html and this fails until the new size is re-measured and
// re-recorded.
const linkTabBadgeSize = "h-3"

// tabAnchor is one tab's rendered anchor, strip entry and badge included.
func tabAnchor(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `?tab=`+id+`"`)
	if i < 0 {
		t.Fatalf("the strip offers no %s tab", id)
	}
	open := strings.LastIndex(page[:i], "<a ")
	end := strings.Index(page[i:], "</a>")
	if open < 0 || end < 0 {
		t.Fatalf("the %s tab's anchor is not a closed element", id)
	}
	return page[open : i+end]
}

// stripTabs builds the fixture strip with one tab's badge overridden, so a
// state the package fixture does not hold — an empty count, a cross, a
// sequential split — can be rendered without inventing a second strip.
func stripTabs(badge map[string]map[string]any) []map[string]any {
	tabs := linkDetailTabsFixture()
	for _, tab := range tabs {
		id, _ := tab["ID"].(string)
		if over, ok := badge[id]; ok {
			for k, v := range over {
				tab[k] = v
			}
		}
	}
	return tabs
}

// TestEveryTabCarriesItsState is M47.5's central bullet: state lives on the
// tab, because the badge is then both the answer and the way in.
//
// The assertions follow the vocabulary rather than the pixels:
//
//   - **A count of zero is a muted `0`, never a missing badge.** A missing
//     badge and a badge reading zero are different claims and a reader cannot
//     tell them apart — so both states must render a chip, in the same box
//     (`h-4 min-w-4`), which is also the structural half of "every tab holds
//     the same width whether set or empty". The pixel half is the kept spec's
//     (tools/agent-browser/specs/link-tabs.spec.mjs).
//   - **Split has three states, counted from the source**: weighted,
//     sequential, and the cross for a link with none. Two glyphs and a cross,
//     never a check — the section is not binary.
//   - **Signed is the strip's one true binary and its one colour.** The check
//     renders in the ok tokens exactly when the badge is a check, and no other
//     badge in the strip carries them.
//   - **The cross means the section is empty, not no** — asserted as its
//     accessible name, because the name is where a third meaning would lie.
//   - **Danger takes no badge.** It has no state; deletability is a
//     permission.
//   - **Every icon badge is a titled image at the measured size** — role,
//     aria-label, <title>, and the linkTabBadgeSize utility the pixel-budget
//     scan reads.
func TestEveryTabCarriesItsState(t *testing.T) {
	// The package fixture: five protections, two codes, two rules, a weighted
	// split, signature required, forty clicks.
	set := mainOf(t, renderPage(t, "link_detail", nil))

	// The same link unconfigured: zero counts, no split, no signature.
	empty := mainOf(t, renderPage(t, "link_detail", map[string]any{
		"Tabs": stripTabs(map[string]map[string]any{
			"edit":      {"Badge": "count", "Count": int64(0)},
			"qr":        {"Badge": "count", "Count": int64(0)},
			"routing":   {"Badge": "count", "Count": int64(0)},
			"split":     {"Badge": "cross"},
			"signed":    {"Badge": "cross"},
			"analytics": {"Badge": "count", "Count": int64(0)},
		}),
	}))

	const chip = `inline-flex h-4 min-w-4 items-center justify-center rounded-full`

	for _, id := range []string{"edit", "qr", "routing", "analytics"} {
		on, off := tabAnchor(t, set, id), tabAnchor(t, empty, id)
		if !strings.Contains(on, chip) || !strings.Contains(off, chip) {
			t.Errorf("the %s tab does not draw its chip in both states — same box "+
				"set or empty is what keeps the strip from reflowing as a link is "+
				"configured", id)
		}
		if !strings.Contains(off, `text-subtle">0<`) {
			t.Errorf("the %s tab's empty state is not a muted 0. A missing badge "+
				"and a badge reading zero are different claims:\n  %s", id, off)
		}
		if strings.Contains(on, `>0<`) || !strings.Contains(on, "text-ink") {
			t.Errorf("the %s tab's set state does not read as a set count:\n  %s", id, on)
		}
	}

	// Split: the glyph pair, then the cross — and its accessible name is the
	// mode, because the two modes are the whole mark.
	if got := tabAnchor(t, set, "split"); !strings.Contains(got, `aria-label="Weighted split"`) {
		t.Errorf("a weighted split's tab does not carry the weighted glyph:\n  %s", got)
	}
	sequential := mainOf(t, renderPage(t, "link_detail", map[string]any{
		"Tabs": stripTabs(map[string]map[string]any{"split": {"Badge": "sequential"}}),
	}))
	if got := tabAnchor(t, sequential, "split"); !strings.Contains(got, `aria-label="Sequential split"`) {
		t.Errorf("a sequential split's tab does not carry the sequential glyph:\n  %s", got)
	}
	if got := tabAnchor(t, empty, "split"); !strings.Contains(got, `aria-label="Empty"`) {
		t.Errorf("a link with no split does not carry the cross, whose name is "+
			"*empty* — the cross means the section is empty, not no:\n  %s", got)
	}

	// Signed: check with the ok tokens when required, cross when not, and the
	// check is the only colour in the strip.
	if got := tabAnchor(t, set, "signed"); !strings.Contains(got, `aria-label="Signed access required"`) ||
		!strings.Contains(got, "text-ok-ink") {
		t.Errorf("a link requiring signed access does not carry the coloured check:\n  %s", got)
	}
	if got := tabAnchor(t, empty, "signed"); !strings.Contains(got, `aria-label="Empty"`) {
		t.Errorf("a link not requiring signed access does not carry the cross:\n  %s", got)
	}
	for state, page := range map[string]string{"set": set, "empty": empty} {
		nav := stripOf(t, page)
		want := 0
		if state == "set" {
			want = 1
		}
		if n := strings.Count(nav, "text-ok-ink"); n != want {
			t.Errorf("the %s strip carries colour on %d badges, want %d — the check "+
				"is the only badge carrying colour, because signed access is worth "+
				"reading at a glance and colour spent everywhere reads nowhere",
				state, n, want)
		}
	}

	// Danger: no badge, in every state.
	for state, page := range map[string]string{"set": set, "empty": empty} {
		if got := tabAnchor(t, page, "danger"); strings.Contains(got, "<span") ||
			strings.Contains(got, "<svg") {
			t.Errorf("the danger tab carries a badge in the %s state; it has no "+
				"state to carry — deletability is a permission:\n  %s", state, got)
		}
	}

	// Every glyph in the strip is a titled image at the measured size.
	for _, page := range []string{set, empty, sequential} {
		nav := stripOf(t, page)
		for _, m := range svgTag.FindAllStringSubmatch(nav, -1) {
			if !strings.Contains(m[1], linkTabBadgeSize) {
				t.Errorf("a strip glyph does not declare the measured size %s:\n  <svg%s>",
					linkTabBadgeSize, m[1])
			}
			if !strings.Contains(m[1], `role="img"`) || !strings.Contains(m[1], `aria-label="`) {
				t.Errorf("a strip glyph is not an accessible image:\n  <svg%s>", m[1])
			}
		}
		if strings.Count(nav, "<svg") != strings.Count(nav, "<title>") {
			t.Error("a strip glyph is missing its <title>")
		}
	}
}

// stripOf is the strip's <nav> element, so claims about badges are read out of
// the strip rather than out of a page that draws other pictures.
func stripOf(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `aria-label="Link sections"`)
	if i < 0 {
		t.Fatal("the page draws no tab strip")
	}
	end := strings.Index(page[i:], "</nav>")
	if end < 0 {
		t.Fatal("the strip is not a closed <nav>")
	}
	return page[i : i+end]
}

// checkboxNamed pulls one checkbox out of a rendered page and reports whether it
// is ticked.
//
// By tag rather than by substring, because the attribute F167 counted —
// `name="one_time" value="1" checked` — is not what the template emits: the
// class sits between the value and the checked, so a literal search returns 0
// whichever limb ran. A search that cannot tell the two states apart is how six
// branches went unrendered under tests that read this page byte by byte.
func checkboxNamed(t *testing.T, page, name string) (tag string, checked, present bool) {
	t.Helper()
	m := regexp.MustCompile(`<input\b[^>]*\bname="` + name + `"[^>]*>`).FindString(page)
	if m == "" {
		return "", false, false
	}
	return strings.Join(strings.Fields(m), " "), strings.Contains(m, "checked"), true
}

// TestEveryGateOnTheEditFormDrawsBothStates is F167.
//
// Six `{{if}}` branches on the product's largest form had never been rendered by
// any test in this repository, because the fixture passed `Form` as a
// `map[string]string` where production passes a struct with `bool` fields. A
// missing key in a map yields `""`, `{{if}}` reads that as false, and so every
// one of them took its else limb on every run — while `TestEveryPageRenders`,
// `TestWideElementsScrollInsideTheirOwnContainer`,
// `TestTemplatesUseThemeTokensOnly` and M47's three tests all read this page.
// Nothing shipped wrongly; a browser renders all six correctly. What was missing
// is any way for a change that breaks one of them to fail `make check`.
//
// Three states, because the six branches are not six independent booleans: the
// *Remove the password* control exists only inside `HasPassword`, so its own
// unticked limb needs a state where the password is set and the request to
// remove it is not.
func TestEveryGateOnTheEditFormDrawsBothStates(t *testing.T) {
	base := linkForm{URL: "https://example.com/x", Alias: "demo", MaxClicks: "466"}

	on := base
	on.ForwardQuery, on.ForwardPath = true, true
	on.HasPassword, on.ClearPassword = true, true
	on.OneTime, on.RequireSignature = true, true

	kept := on
	kept.ClearPassword = false

	pages := map[string]string{
		// The package's own fixture first, because it is the one the finding is
		// about: every other test in this package renders it, and it is what put
		// all six branches on their else limb.
		"the package fixture": renderPage(t, "link_detail", nil),
		"every gate on":       renderPage(t, "link_detail", map[string]any{"Form": on}),
		"a password kept":     renderPage(t, "link_detail", map[string]any{"Form": kept}),
		"every gate off":      renderPage(t, "link_detail", map[string]any{"Form": base}),
	}
	ticked := map[string]bool{
		"the package fixture": true, "every gate on": true,
		"a password kept": true, "every gate off": false,
	}

	// The four checkboxes whose only state is their own boolean.
	for _, name := range []string{
		"forward_query", "forward_path", "one_time", "require_signature",
	} {
		for state, page := range pages {
			tag, checked, present := checkboxNamed(t, page, name)
			if !present {
				t.Errorf("%s: the %s checkbox is not on the page at all", state, name)
				continue
			}
			if checked != ticked[state] {
				t.Errorf("%s: %s is checked=%v, want %v:\n  %s",
					state, name, checked, ticked[state], tag)
			}
		}
	}

	// The password box says which of two things it is for, and the sentence is
	// the only thing that distinguishes them — the value is always empty,
	// because the password is stored as an argon2id hash and there is nothing
	// to render.
	for state, page := range pages {
		set, want := "Set — type to replace", ticked[state]
		if strings.Contains(page, set) != want {
			t.Errorf("%s: the password box %s say %q. Its value is empty in both "+
				"states, so the placeholder is the only thing telling a reader "+
				"whether this link has a password",
				state, map[bool]string{true: "does not", false: "does"}[want], set)
		}
		if strings.Contains(page, "No password") == want {
			t.Errorf("%s: the password box's empty-state placeholder is wrong", state)
		}
	}

	// And the control that exists only when there is a password to remove.
	for state, page := range pages {
		tag, checked, present := checkboxNamed(t, page, "clear_password")
		wantPresent := ticked[state]
		if present != wantPresent {
			t.Errorf("%s: the clear_password checkbox present=%v, want %v. It is "+
				"the one control on this form that is drawn or not drawn rather "+
				"than ticked or not, and it was counted 0 times on the page this "+
				"package renders", state, present, wantPresent)
			continue
		}
		if !present {
			if strings.Contains(page, "Remove the password") {
				t.Errorf("%s: the page offers to remove a password this link does not have", state)
			}
			continue
		}
		if !strings.Contains(page, "Remove the password") {
			t.Errorf("%s: the clear_password checkbox has no label", state)
		}
		// Ticked in the state that came back from a rejected save with the box
		// already checked; unticked when the password is simply set.
		if want := state != "a password kept"; checked != want {
			t.Errorf("%s: clear_password is checked=%v, want %v:\n  %s",
				state, checked, want, tag)
		}
	}
}

// TestTheFolderSelectDrawsAndSaysWhereTheLinkIsFiled is F192's first half,
// and it is F167's finding at a different key.
//
// **A fixture that omits a slice yields the zero value, `{{range}}` over it
// emits nothing, and the branch inside is unreachable.** The link page's
// fixture carried no `FolderOptions`, and the only one in this package sits on
// the `links` filter panel — a different template — so link_edit.html's whole
// folder control, the option loop and the `{{if eq .Form.FolderID ""}} selected`
// branch inside it were rendered by no test in the repository. Nothing shipped
// wrongly; a browser draws it correctly. What was missing is any way for a
// change that breaks it to fail `make check`, which is the same sentence F167
// earned.
//
// Three states, because the control has three: filed, unfiled, and a workspace
// that has never made a folder — where the select is absent rather than empty,
// which is the template's own decision and worth holding.
func TestTheFolderSelectDrawsAndSaysWhereTheLinkIsFiled(t *testing.T) {
	const summer = "0198c9c5-0000-7000-8000-000000000041"
	unfiled := []map[string]any{
		{"ID": summer, "Label": "‒ Summer", "Selected": false},
	}

	unfiledForm := linkForm{URL: "https://example.com/x", Alias: "demo"}

	// The package's own fixture first, because it is the one the finding is
	// about: every rendered-page scan here reads it, and it drew no select.
	filed := renderPage(t, "link_detail", nil)
	loose := renderPage(t, "link_detail", map[string]any{
		"FolderOptions": unfiled, "Form": unfiledForm,
	})
	none := renderPage(t, "link_detail", map[string]any{
		"FolderOptions": []map[string]any{}, "Form": unfiledForm,
	})

	for state, page := range map[string]string{"filed": filed, "unfiled": loose} {
		if !strings.Contains(page, `name="folder_id"`) {
			t.Fatalf("%s: the folder select is not on the page at all, so neither "+
				"branch below is being read", state)
		}
		if !strings.Contains(page, `>No folder</option>`) {
			t.Errorf("%s: the select has no empty entry. This form posts every "+
				"field, so an empty value is the only way a link is taken out of a "+
				"folder", state)
		}
		if !strings.Contains(page, `>‒ Summer</option>`) {
			t.Errorf("%s: the workspace's folder is not among the options", state)
		}
	}

	// Exactly one option carries `selected`, and which one is the whole claim:
	// a select that marks both, or neither, tells the reader nothing about
	// where the link actually is. Read out of the folder select alone — the page
	// carries several, and counting across all of them would answer about the
	// campaign and the date range as well.
	filedSelect := folderSelect(t, filed)
	looseSelect := folderSelect(t, loose)

	if n := strings.Count(filedSelect, " selected>"); n != 1 {
		t.Errorf("the folder select marks %d options selected, want 1:\n  %s",
			n, filedSelect)
	}
	if !strings.Contains(filedSelect, `value="`+summer+`" selected>`) {
		t.Errorf("the link is filed in %s and no option says so; the select opens "+
			"on \"No folder\" for a link that is in one:\n  %s", summer, filedSelect)
	}

	if n := strings.Count(looseSelect, " selected>"); n != 1 {
		t.Errorf("an unfiled link's folder select marks %d options selected, want "+
			"1:\n  %s", n, looseSelect)
	}
	if !strings.Contains(looseSelect, `value="" selected>No folder`) {
		t.Errorf("an unfiled link does not open the select on \"No folder\". That is "+
			"the `{{if eq .Form.FolderID \"\"}}` branch, and it was rendered by "+
			"nothing in this package before F192:\n  %s", looseSelect)
	}
	// And a workspace with no folders carries no control at all, rather than one
	// holding a single meaningless option.
	if strings.Contains(none, `name="folder_id"`) {
		t.Error("a workspace that has never made a folder is shown a folder select " +
			"whose only entry is \"No folder\"")
	}
	if strings.Contains(none, ">Folder<") {
		t.Error("the folder label is drawn without the select it labels")
	}
}

// folderSelect is the `<select name="folder_id">` element and its options.
//
// The page draws several selects and the assertions above are about one of
// them, so they are read out of the element rather than out of the document.
func folderSelect(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `name="folder_id"`)
	if i < 0 {
		t.Fatal("the folder select is not on the page")
	}
	open := strings.LastIndex(page[:i], "<select")
	end := strings.Index(page[i:], "</select>")
	if open < 0 || end < 0 {
		t.Fatal("the folder select is not a closed <select> element")
	}
	return page[open : i+end+len("</select>")]
}
