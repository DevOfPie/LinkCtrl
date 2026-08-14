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
// contrast 391, logo 719. The bound of 900 is owner-set over 1200 and 1500,
// knowing that the four obvious cuts reach only 1081 and that the rest has to
// come out of the size-and-contrast paragraph, whose *contrast* half explains a
// refusal no control on the tab states.
const qrProseBound = 900

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
			"over the bound of %d (D188). It was 1906 before M50.7. Cut a "+
			"paragraph, or raise the bound here and say in the commit what the "+
			"words bought", total, len(each), qrProseBound)
	}
}
