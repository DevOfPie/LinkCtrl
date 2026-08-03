package link

import (
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
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
			if got := QRContent(tc.shortURL); got != tc.want {
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
