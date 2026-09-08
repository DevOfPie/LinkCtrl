package addon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
)

// This file is the add-on side of M65: the two records `session_mint` carries,
// the seam onto the service that decides, and the value the host carries back out
// of Route so that a *cookie* is written by internal/httpx and never by a module.
//
// # Where each half lives, and why
//
// The rules — does an account exist, is it active, is it locked, does it still owe
// a second factor, how long does the session live — are internal/auth's, in
// addonauth.go, beside the password path they have to be identical to. What is
// here is the *boundary*: decoding what a module said, refusing what the host
// knows better than the module does, and making sure the one thing that must not
// cross the sandbox never has a way to.
//
// # The token has no route to the guest, and that is structural
//
// [MintedSession] is what `session_mint` writes back, and it carries two fields:
// an expiry and whether a factor is still owed. The token lives on [Minted],
// which is a different type, is not marshalled by anything on the ABI path, and
// holds the token as a [config.Secret] so that logging the struct — from here,
// from internal/httpx, or from a caller a later milestone adds — prints a
// redaction rather than a credential. abi.CredentialBearing walks the record's
// field names for the same claim from the other end.

// SessionClaim is the record a module writes into `session_mint`: its assertion
// that somebody authenticated.
//
// Decoded strictly (see [decodeClaim]): a field this host does not know is a
// module written against a contract that is not this one, and guessing at what it
// meant on the authentication path is the wrong direction to guess in.
type SessionClaim struct {
	Subject       string   `json:"subject"`
	Issuer        string   `json:"issuer"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	DisplayName   string   `json:"display_name"`
	Groups        []string `json:"groups"`
}

// MintedSession is the record the host writes back, and the whole of what an
// add-on learns about the session it caused.
//
// Not the session: no token, no cookie, no row identifier and no account
// identifier. What a module can do with this is decide which page to send the
// person to, which is the only thing it needs it for — and if it wants to know
// who is now signed in, `session_context` on the *next* request is the read half
// and costs its own grant.
type MintedSession struct {
	ExpiresAt string `json:"expires_at"`
	// SecondFactorRequired is true when the host stopped at its own prompt. The
	// module's own `location` is then where the visitor lands *after* the prompt
	// rather than immediately — the host interposes, because the pending
	// credential is the host's and there is no shape in which a module holds one.
	SecondFactorRequired bool `json:"second_factor_required"`
}

// mintedSessionMaxBytes is the largest a [MintedSession] can encode to, and it is
// what `session_mint` requires of the guest's buffer **before it mints**.
//
// # Why a bound and not the record's own size
//
// The ABI publishes one out-parameter convention: a value too large for the
// buffer offered means *nothing was written*, and the caller retries with a
// buffer that size. On every other function the retry costs a second read. On
// this one the first call has already minted a session, so the retry re-enters,
// meets the one-mint guard, and is told its claim was invalid while the host is
// about to set the cookie — the guest and the host telling two different stories
// about whether somebody is signed in.
//
// The fix is to check the buffer before the mint, and the size of the record is
// not known until after it. So what is checked is the record's *maximum*: a
// guest that cannot hold the widest possible answer is told the number to retry
// at and nothing is minted, which makes the retry the first mint rather than a
// second one. A guest offering a zero-length buffer to ask for the size — legal
// under the convention — therefore costs nothing too.
//
// # The arithmetic
//
//	{"expires_at":"","second_factor_required":false}   48 bytes of frame
//	2026-08-21T12:34:56Z                               20 bytes of RFC 3339 in UTC
//
// `false` rather than `true` because it is the longer of the two. The timestamp
// is [time.RFC3339] on a UTC instant, which is twenty bytes for every year this
// product can express in a session expiry. Sixteen bytes of slack sit on top so
// that the number is not a tight fit against a format nobody re-derives, and
// TestAMintedSessionFitsItsPublishedBound is what holds the whole of it.
const mintedSessionMaxBytes = 48 + 20 + 16

// Minted is what the host carries out of [Host.Route] when a module's assertion
// produced something. It is not part of the ABI and no module sees it.
type Minted struct {
	// Token is the session cookie's value, or empty when a second factor is owed.
	// A Secret, so that no log line, no %v and no JSON encoder anywhere can turn
	// this struct back into a credential — the same wrapping an operator's
	// configured add-on settings get, and for a stronger reason.
	Token config.Secret
	// PendingToken is the second-factor challenge, or empty. Also a Secret: it is
	// a bearer credential for one operation, and one operation is a session.
	PendingToken config.Secret
	// ExpiresAt is whichever of the two above expires.
	ExpiresAt time.Time
	// SecondFactorRequired distinguishes the two without reading either Secret.
	SecondFactorRequired bool
}

// SessionMinter is what this package needs from internal/auth in order to answer
// `session_mint`.
//
// An interface rather than the concrete service, for the reason AddonRouter is one
// in internal/httpx: the tests in this package construct hosts without a database,
// and a host that could only be built beside a real auth service could not be
// tested at all. A nil minter is a host that cannot mint — which is what every
// unit test in this package is, and what an instance never is.
type SessionMinter interface {
	MintFromAddonAssertion(ctx context.Context, in auth.AddonAssertion) (*auth.AddonMint, error)
	// LinkAddonIdentity is `identity_link`'s half. The actor is the *host's* —
	// resolved from the request's own session — and it is a parameter rather than
	// something the implementation looks up, because that is what makes "a module
	// names a subject and the host names the account" a property of the signature.
	LinkAddonIdentity(ctx context.Context, actor *auth.Identity,
		addon, issuer, subject string) error
}

// PermissionSessionMint is the grant `session_mint` costs, and it is the reason a
// manifest declaring it is treated as `required` unless an operator says
// otherwise — see requiredByDefault.
const PermissionSessionMint = "session.mint"

// maxClaimField bounds one string inside a claim.
//
// The record as a whole is already bounded by maxStringIn, which protects the
// host's heap. This is a different bound and it protects the *database*: subject
// and issuer become columns and an index key, and a module that can write a
// 60 KB subject can make one index entry out of most of a page. 1 KiB is far more
// than any provider's `sub` or `iss`, and refusing is right rather than truncating
// — a truncated subject is a subject that could collide with somebody else's.
const maxClaimField = 1024

// decodeClaim reads what the module wrote, refusing anything it did not mean.
func decodeClaim(raw []byte) (SessionClaim, error) {
	var c SessionClaim
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Unknown fields refused, matching what the response record does. On this
	// function it is worth more than tidiness: a module built against a later ABI
	// that carries a field this host does not implement must not have that field
	// silently dropped from an *authentication* assertion.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return SessionClaim{}, fmt.Errorf("claim is not a valid record: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err == nil {
		return SessionClaim{}, errors.New("trailing content after the claim object")
	}
	if c.Subject == "" || c.Issuer == "" {
		return SessionClaim{}, errors.New("claim names no subject or no issuer")
	}
	for name, v := range map[string]string{
		"subject": c.Subject, "issuer": c.Issuer,
		"email": c.Email, "display_name": c.DisplayName,
	} {
		if len(v) > maxClaimField {
			return SessionClaim{}, fmt.Errorf("claim field %q is %d bytes and the bound is %d",
				name, len(v), maxClaimField)
		}
	}
	return c, nil
}
