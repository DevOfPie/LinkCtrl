package ui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The horizontal-overflow guard (M46), in M24.5's idiom: a scan over what
// actually renders, failing the build rather than relying on somebody opening a
// phone.
//
// **What it enforces, exactly.** Every `<table>` and every `<pre>` on every page
// has an ancestor — or is itself an element — that scrolls on its own. Those two
// are the elements that cannot reflow: a table is as wide as its columns and a
// `<pre>` is as wide as its longest line, so at 360px each one either scrolls
// inside a box of its own or drags the whole document sideways with it.
//
// **What it does not enforce, stated rather than implied.** M46's bullet says
// "any element that can exceed the viewport", and that set cannot be enumerated
// from markup: a flex row of six controls, an unbroken URL in a table cell and a
// fixed-width SVG all can, and none of them is distinguishable here from markup
// that is fine. The milestone's own risk section is where this narrowing is
// licensed — narrow the bullet to what the scan checks and say what is left
// over, never the other way round. What is left over is the header bar, which
// has its own assertion in TestTheHeaderCannotPushThePageSideways, and
// everything that reflows on its own, which is text.
//
// Over the rendered pages rather than over the template sources, because a table
// and the container that wraps it need not be in the same file: links_table.html
// holds the one on the links page and pages/links.html renders it. A source scan
// would have to guess at that seam and would be wrong in whichever direction it
// guessed.

// scrollsItself matches the utilities that give an element its own horizontal
// scroll. Written as whole class names: "overflow-x-auto" is not a substring of
// anything today, and the check that silently stops applying is the one worth
// spelling out.
var scrollsItself = map[string]bool{
	"overflow-x-auto":   true,
	"overflow-x-scroll": true,
	"overflow-auto":     true,
	"overflow-scroll":   true,
	"overflow-hidden":   false, // clips instead of scrolling; not an answer
}

// wideElements are the two tags that cannot reflow.
var wideElements = map[string]bool{"table": true, "pre": true}

// containers are the tags this scan tracks as possible scroll ancestors.
//
// A whitelist rather than every tag, because the alternative is a full HTML
// parser: the templates carry inline SVG whose children self-close in a dozen
// shapes, and a depth count that mis-reads one of them would report an ancestor
// that is not there. Nothing outside this list is ever what wraps a table — the
// wrapper is written by hand, and it is a div.
var containers = map[string]bool{
	"body": true, "main": true, "div": true, "section": true, "article": true,
	"aside": true, "nav": true, "header": true, "footer": true, "figure": true,
	"form": true, "details": true, "fieldset": true, "td": true, "li": true,
	"table": true, "pre": true,
}

var anyTag = regexp.MustCompile(`(?s)<(/?)([a-zA-Z][a-zA-Z0-9-]*)\b([^>]*)>`)

// TestWideElementsScrollInsideTheirOwnContainer is the assertion above, run over
// every page.
//
// Both directions matter and only one of them is obvious. A page that gains a
// table fails until somebody wraps it, which is the point; a wrapper that loses
// its class fails too, which is the regression that would otherwise be invisible
// until an operator opened the members list on a phone.
func TestWideElementsScrollInsideTheirOwnContainer(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	data := pageData(t)

	for _, page := range r.Pages() {
		t.Run(page, func(t *testing.T) {
			d, ok := data[page]
			if !ok {
				t.Fatalf("no test data for page %q", page)
			}
			rec := httptest.NewRecorder()
			if err := r.Render(rec, http.StatusOK, page, d); err != nil {
				t.Fatalf("render %s: %v", page, err)
			}

			var stack []string // opening tags of the tracked ancestors
			for _, m := range anyTag.FindAllStringSubmatch(rec.Body.String(), -1) {
				closing, name, attrs := m[1] == "/", strings.ToLower(m[2]), m[3]
				if !containers[name] {
					continue
				}
				if closing {
					if n := len(stack); n > 0 {
						stack = stack[:n-1]
					}
					continue
				}
				if wideElements[name] && !scrolls(append(stack, m[0])) {
					t.Errorf("%s renders a <%s> that nothing scrolls:\n  %s\n\n"+
						"At 360px it is wider than the viewport and takes the whole "+
						"document sideways with it. Wrap it in "+
						`<div class="overflow-x-auto">, or put the class on the `+
						"element itself.", page, name, strings.TrimSpace(m[0]))
				}
				if strings.HasSuffix(strings.TrimSpace(attrs), "/") {
					continue // self-closed; never opens a level
				}
				stack = append(stack, m[0])
			}

			// The scan is only worth its output if it tracked the nesting it
			// claims to. An unbalanced stack means a tracked tag went unclosed and
			// every ancestor answer after it was guessed.
			if len(stack) != 0 {
				t.Errorf("%s leaves %d tracked element(s) unclosed, so this scan's "+
					"ancestor answers are unreliable; the innermost is %s",
					page, len(stack), strings.TrimSpace(stack[len(stack)-1]))
			}
		})
	}
}

// scrolls reports whether anything in the chain gives the innermost element its
// own horizontal scroll.
func scrolls(chain []string) bool {
	for _, tag := range chain {
		at := strings.Index(tag, `class="`)
		if at < 0 {
			continue
		}
		rest := tag[at+len(`class="`):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			continue
		}
		for _, c := range strings.Fields(rest[:end]) {
			if scrollsItself[c] {
				return true
			}
		}
	}
	return false
}
