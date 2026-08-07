package ui

import (
	"io/fs"
	"regexp"
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
// from markup at all: a table is as tall as its rows, an SVG as tall as its
// viewBox, a list as tall as its items.
//
// Text is deliberately absent. A paragraph's height is bounded by how much text
// is in it, and the character budget below is what bounds that.
var unboundedHeight = []string{
	"<table", "<svg", "<img", "<iframe", "<video", "<canvas", "<ul", "<ol", "<details",
}

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
// **The pixel claim was measured, once, and this test is not the measurement.**
// At 1280×800, in Blink, Gecko and WebKit identically: the destination box's top
// edge moved from **1883px to 327px** and the alias field's bottom edge sits at
// **443px**, so both are inside the viewport with 357px to spare where before
// they were a screen and a half below it. The harness that took those figures is
// not committed, for the reason M46's was not — `tools/render-verify` is opt-in
// and reachable from no gate, so a pixel assertion living there would protect
// nothing between the two times somebody ran it. decisions.md carries the
// numbers.
//
// What this test asserts is the structural property that measurement rests on,
// in the two directions that can regress:
//
//   - the destination and the alias are the **first two** controls the page
//     draws, so nothing interactive can be put in front of them;
//   - what is in front of them is text and only text, under a character budget,
//     so nothing tall can be put there either.
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
