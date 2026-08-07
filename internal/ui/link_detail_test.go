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
