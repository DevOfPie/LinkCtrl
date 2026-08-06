package domain

import "strings"

// Where a click came from, when the visitor's browser cannot say (M41).
//
// A QR code is scanned by a camera, not clicked in a page, so the request
// carries no `Referer` header and the click lands in the analytics as `direct` —
// indistinguishable from somebody typing the short URL by hand. **The source
// query parameter is how the code tells this server what it is.** Every QR code
// this product draws encodes `<short url>?src=qr`, so the fact travels in the
// picture rather than in a header the scanner will not send.
//
// **It adds no analytics schema**, which m41.md required. The value replaces the
// referrer host on the click event and is rolled up, capped and read by exactly
// the query every other breakdown uses, appearing in the Referrers panel beside
// `direct` — which is itself a non-hostname sentinel that rollup already writes
// into that column. See decisions.md, D76.
//
// **The vocabulary is closed, and that is the load-bearing part.** The value is
// written into `link_dimension_daily.value`, whose primary key includes it, so
// accepting anything a visitor typed would let one person append `?src=` and a
// random string a million times and grow that table by a million rows for one
// link. An allowlist makes the parameter free to forward, free to bookmark and
// impossible to use that way; an unrecognised value is ignored and the click is
// attributed exactly as it would have been without the parameter.

// ClickSourceParam is the query parameter a QR code carries. Reserved: it is
// read by this server on every redirect, and the values it accepts are below.
//
// Unlike the signature parameters (M35) it is **not** stripped before the query
// reaches the destination. A signature is a credential and leaking one hands the
// destination's operator a replayable URL; a source tag is a label, and a
// destination whose own analytics also see that the visit came from a QR code is
// better informed rather than compromised.
const ClickSourceParam = "src"

// ClickSourceQR is the only value this milestone defines.
const ClickSourceQR = "qr"

// clickSources is the closed vocabulary. Adding an entry is a deliberate act:
// each one is a value that can appear in the Referrers breakdown forever.
var clickSources = map[string]struct{}{
	ClickSourceQR: {},
}

// ClickSource resolves a raw `src` value against the vocabulary. The second
// return is false for anything unrecognised, which is every value this product
// did not put in a QR code itself.
func ClickSource(raw string) (string, bool) {
	if raw == "" || len(raw) > 16 {
		return "", false
	}
	v := strings.ToLower(raw)
	if _, ok := clickSources[v]; !ok {
		return "", false
	}
	return v, true
}
