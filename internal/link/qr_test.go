package link

import (
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/alias"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// TestTheCodeCarriesTheSourceParameter is the whole attribution mechanism in one
// assertion (M41).
//
// A QR code is scanned by a camera, which sends no `Referer`. If the parameter
// is not inside the picture there is nowhere else it can come from, and every
// scan lands as `direct` — indistinguishable from somebody typing the URL. So
// what this asserts is not string formatting: it is that a scan is countable at
// all.
func TestTheCodeCarriesTheSourceParameter(t *testing.T) {
	cases := []struct {
		name     string
		shortURL string
		want     string
	}{
		{
			name:     "an ordinary short URL",
			shortURL: "https://links.example/summer",
			want:     "https://links.example/summer?src=qr",
		},
		{
			// A short URL never carries a query today, but the separator has to
			// be right if one ever does: `?` twice is a URL whose second
			// parameter is part of the first one's value, and the scan would be
			// attributed to nothing.
			name:     "one that already has a query",
			shortURL: "https://links.example/summer?utm_source=poster",
			want:     "https://links.example/summer?utm_source=poster&src=qr",
		},
		{
			// Nothing to encode. Returning "?src=qr" would produce a QR code
			// pointing at a relative path, which scans and goes nowhere.
			name:     "no URL at all",
			shortURL: "",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QRContent(tc.shortURL, ""); got != tc.want {
				t.Errorf("QRContent(%q) = %q, want %q", tc.shortURL, got, tc.want)
			}
		})
	}
}

// TestTheSourceValueIsOneTheRedirectPathAccepts pins the two ends of the
// mechanism together. The code writes a value and the redirect path resolves it
// against a closed vocabulary; if the two ever disagree, scans are silently
// unattributed and nothing else fails.
func TestTheSourceValueIsOneTheRedirectPathAccepts(t *testing.T) {
	if _, ok := domain.ClickSource(domain.ClickSourceQR); !ok {
		t.Fatalf("the value QR codes encode (%q) is not one the redirect path "+
			"recognises; every scan would be attributed as direct",
			domain.ClickSourceQR)
	}
}

// TestTheShortestContentIsWhatInternalQRAssumes is the other end of
// internal/qr's `minProductContent` (M50.6).
//
// **The occlusion cap holds for the symbol versions this product's content
// lengths produce, and the floor of that range is an assumption internal/qr
// cannot check.** It knows nothing about aliases or hostnames — the layering is
// deliberate and predates this milestone — so the shortest URL it assumes is
// written there as a constant. This is where that constant is measured against
// the two bounds it was derived from, so a change to either one fails here
// rather than silently widening a range a cap was checked over.
//
// Versions 1 and 2 at level H hold 7 and 14 bytes. Anything at or above this
// floor is version 3 or larger, which is where the cap's arithmetic starts
// working.
func TestTheShortestContentIsWhatInternalQRAssumes(t *testing.T) {
	// The shortest registrable hostname: two labels, the last alphabetic —
	// internal/domain's ValidateHostname refuses one label and a numeric TLD.
	const shortestHost = "a.b"
	shortestAlias := strings.Repeat("a", alias.MinLength)

	got := QRContent("https://"+shortestHost+"/"+shortestAlias, "")
	if n := len(got); n != qr.MinProductContent {
		t.Errorf("the shortest content this product can build is %d bytes (%q) and "+
			"internal/qr's cap is checked from %d. The two have to agree: the range "+
			"of symbol versions the occlusion cap was asserted over starts at "+
			"whichever version that floor encodes to",
			n, got, qr.MinProductContent)
	}

	// A named code is longer, never shorter, so the floor above is the floor for
	// every code this product draws rather than only for the default one.
	named := QRContent("https://"+shortestHost+"/"+shortestAlias, "abcdefgh")
	if len(named) <= len(got) {
		t.Errorf("a named code's payload is %d bytes and the default's is %d; the "+
			"floor is then the wrong one", len(named), len(got))
	}
}
