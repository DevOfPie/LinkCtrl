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

// Which of a link's QR codes was scanned (M50).
//
// **The identity is in a parameter of its own, and that is the whole of why
// this exists separately.** A link may carry several codes — a print run and a
// shop window against one destination — and telling them apart means the code
// saying which one it is. It cannot say so inside `src`: that vocabulary is
// closed above, and closed *because* its values become primary-key components in
// `link_dimension_daily`, so admitting a value a visitor chose would let one
// person grow that table by a row per string they invent. Widening `src` to
// carry an identity would open exactly the hole the allowlist was built to shut.
//
// So the identity rides beside it, and the closing happens on the other side
// instead: the value is resolved against the slugs *this link actually has*,
// read from the snapshot the redirect already holds, and anything else is
// recorded as no code at all. The set of storable values is therefore bounded by
// MaxQRCodesPerLink per link, which is a bound the workspace sets and a visitor
// cannot move.

// ClickCodeParam is the query parameter a named QR code carries.
//
// Reserved, like ClickSourceParam, and read on every redirect that carries a
// recognised source. Short because it is printed inside a picture: every
// character in the payload is another module in the matrix, and a longer
// parameter name makes every code that carries it physically bigger.
//
// Not stripped before the query reaches the destination, for the reason
// ClickSourceParam is not: it is a label rather than a credential, and a
// destination whose own analytics can also see which printed code sent somebody
// is better informed rather than compromised.
const ClickCodeParam = "qrc"

// MaxQRCodeSlugLength bounds a slug on the way in and on the way out.
//
// Checked before the snapshot is consulted, so a request carrying a megabyte of
// `qrc` is refused by a length test rather than by a scan over the slugs.
const MaxQRCodeSlugLength = 16

// ClickSourceCode is the value stored for a scan of a named code.
//
// **`qr:<slug>`, into the referrer dimension the bare `qr` already lives in.**
// No new column, no new rollup pass, and no change to RollupDimensionDaily —
// per-code counts are a filter over values the existing dimension already
// carries, read by exactly the query every other breakdown is read by. That is
// what keeps this milestone on the near side of the line campaign analytics was
// deferred behind: a new pass over `click_events` grouped by a mostly-null
// column is the cost that deferred it, and this adds none.
//
// The colon is what makes the namespace safe. `referrer_host` otherwise holds
// hostnames and the `direct` sentinel, and a colon cannot appear in a hostname,
// so `qr:` prefixes a set of values nothing else can collide with.
//
// The default code — the one whose payload carries no ClickCodeParam at all —
// is stored as the bare ClickSourceQR, unchanged from M41. Every code this
// product printed before M50 carries that payload, so every one of them goes on
// being counted where it has always been counted.
func ClickSourceCode(slug string) string {
	if slug == "" {
		return ClickSourceQR
	}
	return ClickSourceQR + ":" + slug
}

// QRCodeSlugOf returns the slug inside a stored source value, and whether the
// value named a code at all.
//
// The inverse of ClickSourceCode, and the reader's half of it: the analytics
// store the slug and the dashboard shows the label, so something has to turn one
// back into the other. The bare `qr` returns ("", false) — it is a scan of the
// default code, which is a code, but not one named by a slug.
func QRCodeSlugOf(value string) (string, bool) {
	const prefix = ClickSourceQR + ":"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	return value[len(prefix):], true
}
