// Package gate holds the four things a link can put in front of its
// destination: a password, a signature, a one-time budget and a click ceiling.
//
// It exists as a package rather than as more methods on the redirect handler
// because M36 reuses the budget counter, and because the shape of each gate is a
// decision worth reading in one place rather than inferred from where it is
// called. Everything here is consulted **only** for a link whose cached snapshot
// says it is gated; a link with no gates never reaches this package at all,
// which is what keeps the 20ms budget the property of the ungated path it has
// always been.
package gate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// The signed-URL format, and it is documented here because docs/SECURITY.md and
// the OpenAPI description both point at this comment rather than restating it.
//
//	https://<link host>/<alias>?exp=<unix seconds>&sig=<signature>
//
// `sig` is the base64url encoding, unpadded, of HMAC-SHA256 over
//
//	"lc1\n" + <domain uuid> + "\n" + <canonical alias> + "\n" + <exp>
//
// keyed by the workspace's signing secret.
//
// Four things are in the message and each closes something.
//
// The **version tag** means a later format can be introduced without the old
// one being reinterpreted under the new rules — a signature is a capability, and
// a capability whose meaning can change is not one.
//
// The **domain id** binds the signature to the hostname it was minted for.
// Without it, a workspace serving the same alias on two domains — which is what
// M40's custom domains are for — would find a signature issued for one working
// on the other.
//
// The **canonical alias** is the same spelling the resolver looked the link up
// under, so a signature cannot be made to verify by re-casing or re-encoding the
// path.
//
// The **expiry** is inside the MAC rather than beside it. A signature whose
// expiry could be edited by whoever holds the URL expires when they say it does,
// which is to say never.
const (
	// SigParam and ExpParam are the two query parameters a signed URL carries.
	// Both are stripped before the query is forwarded to the destination: they
	// are addressed to this server, and leaking a workspace's signature to
	// whoever runs the destination would hand them a URL they can replay until
	// it expires.
	SigParam = "sig"
	ExpParam = "exp"

	// sigVersion prefixes the signed message. Changing it invalidates every
	// signature in existence, which is the point of having it.
	sigVersion = "lc1"

	// SecretLength is how many random bytes a workspace secret carries. 32, the
	// output size of the hash it keys: more would be folded by HMAC's own
	// padding, less would be the weakest part of the construction.
	SecretLength = 32
)

// Signature errors. Distinguished so the handler can log the cause while
// answering the same thing to the client either way — a caller that learns
// *which* way its signature was wrong learns something about the secret.
var (
	ErrNoSignature  = errors.New("gate: request carries no signature")
	ErrBadSignature = errors.New("gate: signature does not verify")
	ErrExpired      = errors.New("gate: signature has expired")
	ErrNoSecret     = errors.New("gate: workspace has no signing secret")
)

// Sign returns the query parameters a signed URL for this alias must carry.
//
// The caller assembles the URL, because what the public origin is depends on
// configuration this package does not read.
func Sign(secret []byte, domainID uuid.UUID, alias string, expires time.Time) (url.Values, error) {
	if len(secret) == 0 {
		return nil, ErrNoSecret
	}
	exp := strconv.FormatInt(expires.UTC().Unix(), 10)
	v := url.Values{}
	v.Set(ExpParam, exp)
	v.Set(SigParam, base64.RawURLEncoding.EncodeToString(mac(secret, domainID, alias, exp)))
	return v, nil
}

// Verify checks the signature on an incoming request's query.
//
// Nothing here reads the database, allocates beyond the MAC, or depends on
// Redis: the secret arrives from the caller's in-process keyring and the rest is
// a hash over four short strings. That is what makes this affordable on the hot
// path for the links that ask for it.
func Verify(secret []byte, domainID uuid.UUID, alias string, q url.Values, now time.Time) error {
	if len(secret) == 0 {
		return ErrNoSecret
	}
	got := q.Get(SigParam)
	exp := q.Get(ExpParam)
	if got == "" || exp == "" {
		return ErrNoSignature
	}
	sum, err := base64.RawURLEncoding.DecodeString(got)
	if err != nil {
		return ErrBadSignature
	}
	// The MAC is checked before the expiry, and in constant time. Reading the
	// expiry first would answer "expired" faster than "wrong", which is a signal
	// about a signature the caller did not produce.
	if !hmac.Equal(sum, mac(secret, domainID, alias, exp)) {
		return ErrBadSignature
	}
	at, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		// Unreachable through a signature this code minted, because the MAC
		// covers these bytes and already matched. Refused rather than trusted:
		// a value that got here is a value nobody in this package wrote.
		return ErrBadSignature
	}
	if !now.Before(time.Unix(at, 0)) {
		return ErrExpired
	}
	return nil
}

// StripSignature removes the signature parameters from a raw query string.
//
// Applied before the query is forwarded to the destination (M8's forward_query),
// so a signed URL that also forwards its query does not hand the destination a
// replayable capability. Returns the query unchanged when it carries neither
// parameter, which is every request to every ungated link.
func StripSignature(raw string) string {
	if raw == "" {
		return raw
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		// Unparseable queries are forwarded verbatim elsewhere in this codebase
		// (see httpx.appendQuery), and rewriting one here would be the lossy
		// re-encoding that path exists to avoid. A signature inside a query the
		// parser cannot read is a query no signed URL this code minted produced.
		return raw
	}
	if !q.Has(SigParam) && !q.Has(ExpParam) {
		return raw
	}
	q.Del(SigParam)
	q.Del(ExpParam)
	return q.Encode()
}

func mac(secret []byte, domainID uuid.UUID, alias, exp string) []byte {
	h := hmac.New(sha256.New, secret)
	// Written field by field with a separator that cannot appear in any of them,
	// rather than concatenated: "ab"+"c" and "a"+"bc" must not sign the same
	// bytes, or an alias could be chosen to impersonate another one's signature.
	h.Write([]byte(sigVersion))
	h.Write([]byte{'\n'})
	h.Write([]byte(domainID.String()))
	h.Write([]byte{'\n'})
	h.Write([]byte(alias))
	h.Write([]byte{'\n'})
	h.Write([]byte(exp))
	return h.Sum(nil)
}
