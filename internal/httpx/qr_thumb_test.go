package httpx

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/qr"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// TestTheQRThumbnailStatesItsOwnHeight is the half of M48's heading-row claim
// that internal/ui cannot check.
//
// `TestTheEditControlIsReachableWithoutScrolling` there reads the class off a
// *fixture* and asserts the rule the owner set on 2026-08-07: an `<svg>` may be
// drawn in front of the destination box when its height is in the markup. That
// is the right place for the rule and the wrong place for the wiring, because
// the fixture is a string in a test file and the picture a reader is served
// comes from here. Both ends name `ui.QRThumbClass`, and this is the assertion
// that the rendering end still does.
//
// It is deliberately not a check that the class is `h-24`. Which box the
// heading row can afford is internal/ui's to state and to re-measure; what this
// owes is that whatever internal/ui states arrives on the element.
func TestTheQRThumbnailStatesItsOwnHeight(t *testing.T) {
	got := string(qrThumb("http://links.test/demo?src=qr", qr.Style{}, nil))

	if want := `class="` + ui.QRThumbClass + `"`; !strings.Contains(got, want) {
		t.Errorf("the thumbnail is drawn without %s.\n\nIts height is then whatever "+
			"version the link's URL encodes to, and it is rendered in front of the "+
			"destination box the link page measures. internal/ui's guard reads a "+
			"fixture and would not notice; this is what does.\n\n%s", want, head(got))
	}

	// Before `width`, because an attribute cannot arrive after the tag closes and
	// a class written into the body would be a class on something else.
	if at := strings.Index(got, ">"); at < 0 || !strings.Contains(got[:at], "class=") {
		t.Errorf("the class is not on the root element:\n\n%s", head(got))
	}

	// The scale is the no-stylesheet fallback and is meant to land near the class
	// rather than at the full code's size, so a page that never got app.css does
	// not put a 300px picture above the control either. Derived from the drawing's
	// own viewBox rather than typed as a number, because how many modules a URL
	// encodes to is the thing this cannot state in advance.
	span := viewBoxSpan(t, got)
	if want := `width="` + strconv.Itoa(span*qrThumbScale) + `"`; !strings.Contains(got, want) {
		t.Errorf("the thumbnail is not drawn at the thumbnail scale: %d modules "+
			"across want %s. A stylesheet that failed to load leaves this size on "+
			"the page, and the full code is three times it.\n\n%s",
			span, want, head(got))
	}
}

// TestTheQRThumbnailFailsSoft is the other half of the same function: a picture
// that cannot be drawn is not an error the reader can act on.
func TestTheQRThumbnailFailsSoft(t *testing.T) {
	if got := qrThumb(strings.Repeat("x", qr.MaxContent+1), qr.Style{}, nil); got != "" {
		t.Errorf("a thumbnail that could not be drawn rendered %q; the QR section "+
			"still draws its worded trigger, so the panel is reachable and there is "+
			"nothing to tell anybody", got)
	}
}

// head is the front of a document, for a message that does not print a QR code.
func head(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// viewBoxSpan is the drawing's width in modules, quiet zone included.
var viewBox = regexp.MustCompile(`viewBox="0 0 (\d+) \d+"`)

func viewBoxSpan(t *testing.T, svg string) int {
	t.Helper()
	m := viewBox.FindStringSubmatch(svg)
	if m == nil {
		t.Fatalf("the thumbnail has no viewBox, so its module count cannot be "+
			"read:\n\n%s", head(svg))
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}
