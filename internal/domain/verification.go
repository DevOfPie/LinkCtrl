package domain

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// DNS verification of a custom hostname (M40).
//
// The whole security story of custom domains is one question: does the person
// who registered this hostname control it? DNS is the only party that can
// answer, so the answer is a record they publish in their own zone and this
// instance reads back. Everything else in the milestone — the router, the cache,
// the grace window — is machinery around that one fact.

// ChallengeLabel is the subdomain the TXT record is published under.
//
// A dedicated label rather than the apex, for two reasons that both matter.
// The apex TXT record is shared property — SPF, DMARC and every SaaS
// verification anybody has ever done live there — so writing to it risks
// breaking something unrelated, and reading it means sifting a list. And an
// underscore label cannot collide with a hostname, because a hostname may not
// contain one; ValidateHostname refuses it, so nothing registered here can ever
// be the challenge name for something else.
const ChallengeLabel = "_linkctrl-challenge"

// ChallengeRecordName is the fully-qualified name to publish the token under.
func ChallengeRecordName(hostname string) string {
	return ChallengeLabel + "." + strings.ToLower(strings.TrimSuffix(hostname, "."))
}

// NewVerificationToken mints a challenge value.
//
// Sixteen bytes of crypto/rand, hex-encoded. It has to be unguessable: a token
// somebody could predict would let them publish the record for a hostname
// *before* registering it, and the record is the whole proof. Hex rather than
// base64 because it goes into a DNS TXT record a person copies by hand, and a
// character set with no case distinction and no punctuation is the one that
// survives that.
func NewVerificationToken() string {
	var b [16]byte
	// crypto/rand.Read is documented never to fail since Go 1.24 — it panics
	// internally rather than returning an error a caller could ignore — so the
	// error is genuinely unreachable and discarding it is the honest reading.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ChallengeSatisfied reports whether any of the TXT strings returned for the
// challenge name carries this token.
//
// Any, not all, and not "exactly one". A zone may carry several TXT records
// under one name — a second LinkCtrl instance, a stale value from a previous
// registration, a record the resolver concatenated differently — and requiring
// the set to be exactly our token would fail a verification the owner has
// genuinely completed.
//
// Whitespace is trimmed because resolvers and zone editors disagree about it,
// and the comparison is case-sensitive because the token is hex from our own
// generator rather than something a human chose.
func ChallengeSatisfied(token string, records []string) bool {
	if token == "" {
		return false
	}
	for _, r := range records {
		if strings.TrimSpace(r) == token {
			return true
		}
	}
	return false
}
