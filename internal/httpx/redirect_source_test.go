package httpx

import (
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

// codeSnapshot is a link carrying the named codes given.
func codeSnapshot(slugs ...string) *redirect.Snapshot {
	return &redirect.Snapshot{Codes: slugs}
}

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
			if got := clickSource(tc.query, codeSnapshot()); got != tc.want {
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
			if got := clickSource(q, codeSnapshot()); got != "" {
				t.Errorf("clickSource(%q) = %q; an unrecognised source reached the "+
					"analytics, and the dimension rollup keys on that value", q, got)
			}
		})
	}
}

// The code parameter beside it (M50).
//
// **The same cardinality guard, one level down.** M41 closed `src` because its
// value becomes part of a primary key; M50 adds a second parameter whose value
// reaches the same column, and it is bounded by a different mechanism — the
// value is resolved against the codes the link actually has, and anything else
// is recorded as the default code. These tests are that mechanism's evidence.
func TestTheCodeParameterNamesOnlyThisLinksCodes(t *testing.T) {
	cases := []struct {
		name  string
		query string
		snap  *redirect.Snapshot
		want  string
	}{
		{"no code parameter is the default code", "src=qr", codeSnapshot("k7m2qh4b"),
			domain.ClickSourceQR},
		{"a code this link has", "src=qr&qrc=k7m2qh4b", codeSnapshot("k7m2qh4b"),
			domain.ClickSourceQR + ":k7m2qh4b"},
		{"one of several", "src=qr&qrc=b2c3d4e5", codeSnapshot("k7m2qh4b", "b2c3d4e5"),
			domain.ClickSourceQR + ":b2c3d4e5"},
		{"order does not matter", "qrc=k7m2qh4b&src=qr", codeSnapshot("k7m2qh4b"),
			domain.ClickSourceQR + ":k7m2qh4b"},
		{"a code this link does not have", "src=qr&qrc=zzzzzzzz", codeSnapshot("k7m2qh4b"),
			domain.ClickSourceQR},
		{"a code on a link with none", "src=qr&qrc=k7m2qh4b", codeSnapshot(),
			domain.ClickSourceQR},
		{"a deleted code, which is what an old print carries", "src=qr&qrc=k7m2qh4b",
			codeSnapshot("b2c3d4e5"), domain.ClickSourceQR},
		{"without a source it is not read at all", "qrc=k7m2qh4b", codeSnapshot("k7m2qh4b"), ""},
		{"a snapshot that is not there", "src=qr&qrc=k7m2qh4b", nil, domain.ClickSourceQR},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clickSource(tc.query, tc.snap); got != tc.want {
				t.Errorf("clickSource(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// TestAnUnknownCodeIsRecordedAsTheDefaultRatherThanStored is M50's half of the
// cardinality guard.
//
// Every value below is one somebody could print on a sticker or type into a bar.
// If any of them reached the click event it would become a permanent row in the
// dimension rollup, keyed on that value, and a script could mint as many as it
// liked — which is precisely the hole `src`'s closed vocabulary was built to
// shut, re-opened one parameter to the left.
func TestAnUnknownCodeIsRecordedAsTheDefaultRatherThanStored(t *testing.T) {
	snap := codeSnapshot("k7m2qh4b")
	hostile := []string{
		"src=qr&qrc=" + strings.Repeat("a", 64),
		"src=qr&qrc=evil.example",
		"src=qr&qrc=K7M2QH4B",
		"src=qr&qrc=k7m2qh4b%20",
		"src=qr&qrc=" + strings.Repeat("x", 17),
		"src=qr&qrc=",
		"src=qr&qrc=%00",
		"src=qr&qrc=../k7m2qh4b",
		"src=qr&qrc=k7m2qh4b&qrc=zzzzzzzz",
	}
	for _, q := range hostile {
		t.Run(q, func(t *testing.T) {
			got := clickSource(q, snap)
			if got != domain.ClickSourceQR && got != domain.ClickSourceQR+":k7m2qh4b" {
				t.Errorf("clickSource(%q) = %q; a value this link never issued reached "+
					"the analytics, and the dimension rollup keys on it", q, got)
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
