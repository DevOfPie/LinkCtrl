package ui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
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
// **And since F184, every `<svg>` that states a width past the viewport.** That
// one is not a tag, it is a tag with an attribute on it: an `<svg>` sized by
// class reflows and an `<svg>` carrying `width="488"` does not, and the
// difference is readable from the markup. It is here because it stopped being
// hypothetical — `/account/mfa` reached 174px past a 360px viewport, measured in
// Chromium on 2026-08-09, and was the one page in the dashboard that did. The
// answer this one accepts is a second one: `max-w-full` on the element itself,
// which shrinks it, as well as an ancestor that scrolls. A QR code inside a
// scrolling box is a QR code somebody has to drag into view before they can
// photograph it, so the shrink is the right answer for it and the scan must not
// insist on the wrong one.
//
// **What it does not enforce, stated rather than implied.** M46's bullet says
// "any element that can exceed the viewport", and that set cannot be enumerated
// from markup: a flex row of six controls and an unbroken URL in a table cell
// both can, and neither is distinguishable here from markup that is fine. The
// milestone's own risk section is where this narrowing is licensed — narrow the
// bullet to what the scan checks and say what is left over, never the other way
// round. What is left over is the header bar, which has its own assertion in
// TestTheHeaderCannotPushThePageSideways, and everything that reflows on its
// own, which is text.
//
// *(A fixed-width SVG was in that list, named by the amendment that narrowed
// M46's bullet at its reopening and two milestones before M53 added one. The
// owner's answer to F184 was both halves — constrain the element and make the
// scan able to see it — so it has moved up into what is enforced.)*
//
// **The width the scan reads is the one in the markup, and for the QR codes that
// markup comes out of internal/qr rather than a template.** The fixtures carry
// what that package emits, class attribute and all, exactly as the thumbnail
// fixture carries ui.QRThumbClass — and they name `qr.FluidClass` rather than
// spelling it, so a fixture cannot drift from the emitter and leave this scan
// claiming something about a page it is no longer rendering. The import is a
// test file's: internal/ui's shipped code depends on nothing outside the
// standard library, internal/qr does not import internal/ui, and the
// stdlib-only rule is about Node, a CDN and the CSP rather than about the Go
// import graph. internal/qr asserts its own half in
// TestAnInlinedCodeFitsTheBoxItIsDrawnInto.
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

// phoneViewport is the width the whole scan is written against: the 360px M46
// measured at, and the 360px F184 was measured at three milestones later.
const phoneViewport = 360

// statedWidth reads a `width="N"` presentation attribute in pixels, or 0 for a
// tag that states none.
//
// Pixels only, deliberately: `width="100%"` is an element that reflows and
// `width="12em"` is not something this codebase writes. Anything that is not a
// bare integer is left to the ancestor rules, which is where an element nobody
// has measured belongs.
var statedWidth = regexp.MustCompile(`\bwidth="(\d+)"`)

// boundsItself matches the utilities that stop a fixed-width element reaching
// past the box it is drawn into. Whole class names, on scrollsItself's reasoning.
var boundsItself = map[string]bool{"max-w-full": true}

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
			// Once per tab for link_detail (renderingsOf), because a page that
			// draws one section at a time shows a single render only one
			// section's markup — and this scan's claim is about all of them.
			for suffix, d := range renderingsOf(page, d) {
				rec := httptest.NewRecorder()
				if err := r.Render(rec, http.StatusOK, page, d); err != nil {
					t.Fatalf("render %s%s: %v", page, suffix, err)
				}
				page := page + suffix // the label failures point at

				var stack []string // opening tags of the tracked ancestors
				for _, m := range anyTag.FindAllStringSubmatch(rec.Body.String(), -1) {
					closing, name, attrs := m[1] == "/", strings.ToLower(m[2]), m[3]

					// F184. Before the container filter, because an <svg> is not one
					// and never becomes an ancestor this scan tracks — the package's
					// icons self-close their children in a dozen shapes, which is the
					// reason `containers` is a whitelist in the first place.
					if name == "svg" && !closing {
						if px := widthOf(attrs); px > phoneViewport &&
							!bounded(attrs) && !scrolls(stack) {
							t.Errorf("%s renders an <svg> %dpx wide that nothing bounds:\n  %s\n\n"+
								"At %dpx it reaches past the viewport and takes the whole "+
								"document sideways with it, which is what /account/mfa did "+
								`by 174px. Put max-w-full h-auto on the element — a code `+
								"scales, its width attribute is only an intrinsic size — or, "+
								`if it must keep that size, wrap it in `+
								`<div class="overflow-x-auto">.`,
								page, px, strings.TrimSpace(m[0]), phoneViewport)
						}
					}

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
			}
		})
	}
}

// scrolls reports whether anything in the chain gives the innermost element its
// own horizontal scroll.
func scrolls(chain []string) bool {
	for _, tag := range chain {
		for _, c := range classesOf(tag) {
			if scrollsItself[c] {
				return true
			}
		}
	}
	return false
}

// bounded reports whether an element's own classes stop it reaching past its
// container (F184).
func bounded(attrs string) bool {
	for _, c := range classesOf(attrs) {
		if boundsItself[c] {
			return true
		}
	}
	return false
}

// widthOf is the pixel width a tag states on itself, or 0.
func widthOf(attrs string) int {
	m := statedWidth.FindStringSubmatch(attrs)
	if m == nil {
		return 0
	}
	px, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return px
}

// classesOf is the class list written on a tag, or on the attributes of one.
func classesOf(tag string) []string {
	at := strings.Index(tag, `class="`)
	if at < 0 {
		return nil
	}
	rest := tag[at+len(`class="`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return nil
	}
	return strings.Fields(rest[:end])
}
