package domain

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"unicode"
)

// A link's QR codes (M50).
//
// **One link, several codes, one destination.** A workspace running a print
// campaign and a screen campaign against the same link gets a code for each and
// can tell which one people scanned. What it does *not* get is a second link
// wearing a code's clothes: a code has no destination of its own, no expiry of
// its own and no gate of its own, because each of those already belongs to the
// link and giving one to a code would put two answers on one redirect.
//
// **The default code is the one with the empty slug.** Every code this product
// drew before M50 carries a payload with no code parameter in it, so the empty
// slug is what those already-printed codes resolve to — not a migration
// artefact but the identity of the code they are pictures of. It is why 03700
// backfills nothing, why M41's bare `qr` value in the referrers breakdown still
// means what it meant, and why the single-code endpoints can go on answering for
// a link without changing their meaning.

// MaxQRCodesPerLink bounds how many codes one link may carry.
//
// The idiom is MaxCampaignsPerWorkspace's: the list is loaded in one query and
// drawn in one panel, so this is the number that keeps it unpaginated. It is
// also the bound on how many distinct values a link's scans can write into
// `link_dimension_daily` — the analytics page draws a row per code, and a link
// with a thousand codes is a link whose analytics page cannot be drawn.
//
// Twenty rather than the hundreds campaigns get, because a code is a physical
// artefact. Each one is a picture somebody printed, mounted or published, and a
// workspace with twenty live print runs against a single destination has a
// naming problem rather than a capacity one.
const MaxQRCodesPerLink = 20

// MaxQRCodeLabelLength bounds a label, in runes.
//
// Short, because the label's whole job is telling one row of a list from
// another. Something longer than this is a description, and a code has nowhere
// to put one.
const MaxQRCodeLabelLength = 60

// qrSlugBytes and QRCodeSlugLength are the generated slug's size.
//
// Five bytes because base32 encodes exactly five as eight characters with no
// padding — the same arithmetic auth.newAPIKeyToken uses for a public id, and
// for the same reason: 40 bits is a handle rather than a secret, and the unique
// index catches the rare collision.
//
// Eight characters is also as much as a printed payload can afford. Every
// character in the content is more modules in the matrix, so a longer slug is a
// physically larger code for every workspace that names one.
const (
	qrSlugBytes = 5
	// QRCodeSlugLength is how many characters NewQRCodeSlug returns.
	QRCodeSlugLength = 8
)

// Lowercase base32, unpadded: no mixed case to transcribe, nothing that needs
// escaping in a query string, and nothing that changes meaning when a code is
// read aloud off a poster.
var qrSlugEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewQRCodeSlug returns a slug for a new code.
//
// crypto/rand rather than a counter, because a slug is printed and a predictable
// one invites somebody to guess a neighbouring code's name and attribute traffic
// to it. It is not a secret — anybody holding the printed code can read it — so
// the entropy is sized as a handle, not as a credential.
//
// crypto/rand.Read is documented never to fail since Go 1.24; it panics instead,
// which is the same contract the rest of this package relies on.
func NewQRCodeSlug() string {
	buf := make([]byte, qrSlugBytes)
	_, _ = rand.Read(buf)
	return strings.ToLower(qrSlugEncoding.EncodeToString(buf))
}

// ValidQRCodeSlug says whether a string could be one of this product's slugs.
//
// **A shape test and never a membership test.** Whether a slug names a code of
// *this link* is answered by the link's own slug list, on the redirect path
// against the snapshot and in the service against the stored rows. This function
// exists so the redirect can refuse a hostile value by length and alphabet
// before it scans anything, and so a stored value can be read back with the same
// rules it was written under.
//
// The empty string is not valid here. It is the default code's slug and it is
// never carried in a payload, so a request presenting it is a request presenting
// a parameter with nothing in it.
func ValidQRCodeSlug(s string) bool {
	if s == "" || len(s) > MaxQRCodeSlugLength {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// QRCodeLabelErrors validates a label, returning the field errors it earns.
//
// Empty is allowed and means "unnamed": the default code starts that way, and a
// surface that insisted on a name before a workspace had two codes would be
// asking a question with no answer yet. What is refused is a label that is only
// whitespace, which reads as a name in the database and as nothing on the page.
func QRCodeLabelErrors(label string) ValidationErrors {
	var errs ValidationErrors
	if label != "" && strings.TrimSpace(label) == "" {
		errs = append(errs, FieldError{
			Field: "label", Code: "invalid",
			Message: "a label made only of spaces reads as a name in the list and shows as nothing",
		})
	}
	if len([]rune(label)) > MaxQRCodeLabelLength {
		errs = append(errs, FieldError{
			Field: "label", Code: "too_long",
			Message: "a label is at most " + itoa(MaxQRCodeLabelLength) +
				" characters; it is what tells one code from another in a list, not a description",
		})
	}
	return errs
}
