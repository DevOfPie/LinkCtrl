package httpx

import (
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// The source parameter on the redirect path (M41).
//
// Two claims, and the second is the one that costs something if it breaks: the
// value that reaches the analytics comes from a closed vocabulary, so a visitor
// cannot append `?src=` and a random string a million times and grow
// `link_dimension_daily` by a million rows for one link. Its primary key
// includes the value, so an open vocabulary is an unbounded write amplification
// anybody on the internet can trigger.

func TestTheSourceParameterIsReadFromTheRawQuery(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"no query at all", "", ""},
		{"the parameter a QR code carries", "src=qr", domain.ClickSourceQR},
		{"beside other parameters", "utm_medium=print&src=qr&x=1", domain.ClickSourceQR},
		{"upper case", "src=QR", domain.ClickSourceQR},
		{"percent-encoded", "src=%71r", domain.ClickSourceQR},
		{"an ordinary link with a query", "utm_source=newsletter", ""},
		// A parameter whose name merely contains "src" must not be read as one.
		{"a similarly named parameter", "resrc=qr", ""},
		{"an unparseable query", "src=qr;%zz", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clickSource(tc.query); got != tc.want {
				t.Errorf("clickSource(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// TestAnUnknownSourceIsIgnoredRatherThanStored is the cardinality guard.
//
// Every value below is one somebody could put in a URL and point at a link. If
// any of them reached the click event, it would become a permanent row in the
// dimension rollup, and a script could mint as many as it liked.
func TestAnUnknownSourceIsIgnoredRatherThanStored(t *testing.T) {
	hostile := []string{
		"src=" + strings.Repeat("a", 64),
		"src=evil.example",
		"src=qr2",
		"src=" + strings.Repeat("x", 17),
		"src=",
		"src=%00",
		"src=qr%20",
	}
	for _, q := range hostile {
		t.Run(q, func(t *testing.T) {
			if got := clickSource(q); got != "" {
				t.Errorf("clickSource(%q) = %q; an unrecognised source reached the "+
					"analytics, and the dimension rollup keys on that value", q, got)
			}
		})
	}
}

// TestTheSourceParameterIsNotAWildcard states the vocabulary's size, so widening
// it is a deliberate edit to a test rather than a side effect of adding a
// constant. Every entry is a value that can appear in the referrers breakdown
// forever.
func TestTheSourceParameterIsNotAWildcard(t *testing.T) {
	known := 0
	for _, v := range []string{domain.ClickSourceQR} {
		if _, ok := domain.ClickSource(v); ok {
			known++
		}
	}
	if known != 1 {
		t.Fatalf("the click-source vocabulary holds %d recognised values; M41 "+
			"defines exactly one", known)
	}
}
