package ui

import (
	"regexp"
	"strings"
	"testing"
)

// The QR tab's prose, bounded by a number (M50.7, D188).
//
// **The bound is a gate on writing and it is meant to be.** It fails when
// somebody adds a sentence to this tab, which is the point: what the owner
// reported about this surface is that it costs more attention than it returns,
// and "we removed some words" is not a claim anybody can check a year later. The
// number is here, with its reasoning, rather than in a rule nobody can find — so
// a later milestone that needs a sentence raises the bound deliberately, in this
// file, and says what it bought.
//
// **Measured 2026-08-13 at `d7107b1`: 1906 characters across seven paragraphs** —
// analytics 208, code meta 153, last-code 171, second-code 179, cap 85, size and
// contrast 391, logo 719. The bound of 900 was owner-set over 1200 and 1500,
// knowing that the four obvious cuts reach only 1081 and that the rest has to
// come out of the size-and-contrast paragraph, whose *contrast* half explains a
// refusal no control on the tab states.
//
// **750 at M50.8**, and the number is arrived at by a rule rather than chosen,
// because this is a gate being moved by the milestone that has to meet it. Two
// more paragraphs went — the last-code sentence, whose reason moved onto the
// remove control itself (F238g), and *"What a second code buys is knowing which
// one people scanned"*, which the owner judged help-page material (F238e). The
// tab measured **715** across four paragraphs after them: analytics 100, code
// meta 153, contrast 191, logo 271. The bound is that measurement rounded **up
// to the next fifty**, so it stays a number a later milestone has to work at
// rather than one it clears by accident, and so that what set it is arithmetic
// rather than whatever the build happened to land on.
//
// **300 since that milestone's reopening**, by the same rule and from the
// fourth report. Three more removals, all owner-set: the analytics sentence
// whole (F244c, −100, and docs/usage.md still states it), the meta line's
// *"the default — a scan carrying no code of its own is counted against this
// one"* (F244d, 153 → 61), and the logo's limits paragraph whole (F244f, −271,
// on the argument that the refusal names the limit with the reader's own
// numbers in it). The tab measures **252** across two paragraphs: code meta 61
// and contrast 191. Rounded up to the next fifty, 300.
//
// **Two of the four paragraphs the bound was written for are gone**, and what
// is left is one label and one refusal-explaining sentence — which is why the
// headroom is 48 characters rather than the 35 the last move left. A later
// milestone adding a sentence to this tab now fails this test on the first one,
// which after four reports asking this surface to say less is the intended
// setting rather than an oversight.
const qrProseBound = 300

// qrProseMinimum is where a paragraph stops being prose and starts being a
// label. Forty characters is about a short sentence; "PNG or JPEG, at most N
// bytes" is a caption on the control beside it and counting it would make the
// bound a tax on labelling controls, which is the opposite of what the milestone
// wants.
const qrProseMinimum = 40

var (
	// Go template comments, which carry most of the words in this file and none
	// of the words a reader sees. Stripped first, because a comment that
	// discusses a paragraph would otherwise be measured as one.
	qrTmplComment = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	qrHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	// A paragraph, and only a <p>: the method counts prose, and every heading,
	// label and button on this tab is a control's name rather than an
	// explanation.
	//
	// The `(\s[^>]*)?` rather than `[^>]*` is load-bearing and cost a wrong
	// measurement to find: `<p[^>]*>` also matches `<path d="…">`, so every
	// inline icon opened a paragraph that ran to the next real `</p>` and the
	// file measured 2012 instead of 1906.
	qrParagraph = regexp.MustCompile(`(?s)<p(\s[^>]*)?>(.*?)</p>`)
	// What the reader never sees inside one — the template's own actions, and
	// the inline tags around emphasis.
	qrAction    = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	qrTag       = regexp.MustCompile(`(?s)<[^>]*>`)
	qrWhitespce = regexp.MustCompile(`\s+`)
)

// qrProse returns the tab's paragraphs, measured by the method m50.7.md states:
// the text inside <p> elements with template actions, comments and tags removed
// and whitespace collapsed, dropping anything under qrProseMinimum.
func qrProse(t *testing.T) (total int, each []string) {
	t.Helper()
	raw, err := files.ReadFile("templates/partials/link_qr.html")
	if err != nil {
		t.Fatalf("read the QR partial: %v", err)
	}
	src := qrHTMLComment.ReplaceAllString(
		qrTmplComment.ReplaceAllString(string(raw), ""), "")
	for _, m := range qrParagraph.FindAllStringSubmatch(src, -1) {
		text := qrTag.ReplaceAllString(qrAction.ReplaceAllString(m[2], ""), "")
		text = strings.TrimSpace(qrWhitespce.ReplaceAllString(text, " "))
		if len([]rune(text)) < qrProseMinimum {
			continue
		}
		each = append(each, text)
		total += len([]rune(text))
	}
	return total, each
}

// TestTheQRTabsProseIsUnderItsBound is the number, asserted.
//
// It fails loudly with every paragraph and its length, because the useful
// failure is not "you are over" but "here is what you are carrying" — the fix is
// always a specific paragraph, and a reader of the failure should not have to
// re-derive the measurement to find it.
func TestTheQRTabsProseIsUnderItsBound(t *testing.T) {
	total, each := qrProse(t)
	if total > qrProseBound {
		for _, p := range each {
			t.Logf("%4d  %s", len([]rune(p)), p)
		}
		t.Errorf("the QR tab carries %d characters of prose across %d paragraphs, "+
			"over the bound of %d (D188, lowered at M50.8 and again at its "+
			"reopening). It was 1906 before M50.7, 715 after M50.8 and 252 after "+
			"the fourth report. Cut a paragraph, or raise the bound here and say "+
			"in the commit what the words bought",
			total, len(each), qrProseBound)
	}
}
