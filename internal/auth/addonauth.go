package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// This file is M65's host half: the rules a session minted on an add-on's word
// has to pass, and the mapping that makes an add-on's assertion resolvable at
// all.
//
// # The split, stated once
//
// The add-on asserts and the host mints. An add-on holding `session.mint` can say
// *this external subject authenticated*; it cannot say who that is on this
// instance, whether they may sign in, how long the session lives, or whether they
// still owe a second factor. Every one of those is answered here, by the same
// code and against the same columns the password path uses — which is the whole
// mitigation for the risk m65.md opens with, that a bug in this milestone is a
// CVE rather than an outage.
//
// # Why this is a third caller of mintSession and not a fourth path
//
// mintSession's own comment says there are two callers and nothing else in this
// package calls CreateSession. That sentence is the mechanism M53 built: one place
// a session token comes into existence, so a second factor is worth something.
// This is the third caller and it reaches the same function rather than a copy of
// it, so the workspace resolution, the token generation, the TTL and the identity
// assembly are one implementation. What this file adds is what happens *before*
// that call, and every one of those is a refusal.

// Errors this path answers with. Each is distinguishable because the add-on's own
// behaviour differs — a module that learns the subject is unlinked can send the
// person to a linking page, and one that learns the account is locked cannot — and
// none of them carries a reason across the boundary: the ABI's answer is a status
// (hostabi.go), and the sentence is for this instance's log.
var (
	// ErrAssertionIncomplete is a claim missing the two fields that identify
	// anybody: the issuer and the subject.
	ErrAssertionIncomplete = errors.New("auth: the assertion names no subject or no issuer")
	// ErrSubjectNotLinked is a well-formed assertion for an external identity no
	// account has connected. **It is the ordinary refusal**, not an error state:
	// linking is explicit, so an unlinked subject is what every first visit looks
	// like.
	ErrSubjectNotLinked = errors.New("auth: no account has connected this external identity")
	// ErrAlreadySignedIn is an assertion made on a request that already carries a
	// session. A mint is how somebody signs in; changing who a browser is signed
	// in as, on the word of a module, is the login-CSRF shape and there is no
	// legitimate flow in this product that needs it.
	ErrAlreadySignedIn = errors.New("auth: this request is already signed in")
	// ErrSubjectLinkedElsewhere is a linking attempt for a subject some other
	// account already holds. Refused rather than moved: a link is a credential,
	// and re-pointing one is the takeover the linking table exists to prevent.
	ErrSubjectLinkedElsewhere = errors.New("auth: that external identity is connected to another account")
)

// AddonAssertion is what an add-on said, plus what the *host* knows about the
// request it said it on.
//
// The split between those two halves is load-bearing and is why this is not
// simply the SessionClaim record: Addon, IP, UserAgent and AlreadySignedIn are
// facts the host holds about the request, and a claim that could carry them would
// be a claim a module could lie in. SatisfiesSecondFactor is the operator's, read
// from the environment — the `mfa_satisfied` entry that `config.AddonOverrides`
// reads, one of the two reserved names in `config.AddonOverrideNames` — and never
// from the manifest, because it is a statement about a provider's own
// authentication strength that only the person who configured that provider can
// make. This package does not import internal/config: the value arrives already
// resolved, as a bool, from internal/addon.
type AddonAssertion struct {
	Addon   string
	Issuer  string
	Subject string
	// Email and DisplayName are the provider's, and this path **reads neither**.
	// They are carried so the audit and the log can say what was asserted; no
	// lookup, no comparison and no write in this product uses them, which is the
	// enforced form of m65.md's "matching by email string alone is refused by
	// design".
	Email         string
	DisplayName   string
	EmailVerified bool
	Groups        []string

	AlreadySignedIn       bool
	SatisfiesSecondFactor bool

	IP        netip.Addr
	UserAgent string
}

// AddonMint is what the host decided, as the caller that has to write a cookie
// sees it.
//
// Exactly one of Login and Pending is set. That is the same shape LoginResult
// uses and for the same reason M53 gave: "signed in" and "half signed in" are two
// values rather than one value with a flag, so a caller that ignores the
// distinction writes nothing rather than writing a session cookie for somebody who
// still owes a factor.
type AddonMint struct {
	// Login is the finished sign-in, or nil when a second factor is owed.
	Login *LoginResult
	// Pending is the second-factor challenge, or nil when there is none.
	Pending *PendingSecondFactor
	// ExpiresAt is when whichever of the two above stops being valid. It is the
	// one field that crosses back to the add-on, in the MintedSession record.
	ExpiresAt time.Time
}

// SecondFactorRequired reports whether the person still owes a factor.
func (m *AddonMint) SecondFactorRequired() bool { return m != nil && m.Pending != nil }

// AddonSessionMint is what the audit seam is told about a session minted on an
// add-on's word.
//
// It carries the provenance m65.md asks for and **deliberately nothing about the
// external identity**: no subject, no address, no display name. The reason is
// M52's erasure sweep, which scrubs `audit_logs.metadata` by the keys it knows
// (`email`, and the `from` array), and whose coverage was counted site by site
// when F177 closed. A writer that put a person's provider identifier into a jsonb
// column the sweep does not read would be that count going wrong again, in the
// milestone after the one that finished getting it right. What an operator needs
// from this record — that a session existed, whose, and that an add-on rather than
// a password produced it — is here.
type AddonSessionMint struct {
	// Addon is the add-on's name, and MintedBy is the label the record stores.
	Addon    string
	MintedBy string
	// Issuer is the provider as it named itself. Not an identifier of a person.
	Issuer string
	// SecondFactorRequired says the mint stopped at the prompt rather than
	// producing a session, which is a different event to read afterwards.
	SecondFactorRequired bool
}

// MintedByLabel is how an add-on is named as the minter of a session, and it is
// the string the `minted_by` metadata key carries on a
// `session.minted_by_addon` record.
//
// A prefix rather than a bare name, so the column can grow a second kind of
// minter without the values it already holds becoming ambiguous — and so a reader
// can tell "an add-on called oidc" from any other authority that might one day
// vouch for somebody.
func MintedByLabel(addon string) string { return "addon:" + addon }

// SessionAuditor records a session minted on an add-on's assertion.
//
// The seam onto internal/audit, in the shape APIKeyAuditor and MFAAuditor already
// established: internal/audit imports this package to resolve an actor into the
// label it stores, so this package cannot import that one. Nil records nothing.
type SessionAuditor interface {
	RecordAddonSessionMint(ctx context.Context, actor *Identity, ev AddonSessionMint) error
}

// SetSessionAuditor wires the recorder.
//
// A setter rather than a ServiceConfig field because cmd/linkctrl builds the auth
// service before the audit service — the key service needs auth, and audit needs a
// pool that is opened alongside — and the alternative was reordering three
// constructions to save one method. It is called once at startup, before anything
// is listening.
func (s *Service) SetSessionAuditor(a SessionAuditor) { s.sessionAuditor = a }

// SetLogger gives this service somewhere to put what it cannot return.
//
// One path needs it and it is this file's: a link's `last_used_at` failing to
// move must not fail a sign-in that has already happened, so the failure has
// nowhere to go but a log.
func (s *Service) SetLogger(log *slog.Logger) { s.log = log }

// MintFromAddonAssertion is the third caller of mintSession, and the host half of
// M65's boundary.
//
// The order of the refusals below is deliberate and is the order the password path
// uses, for the reason that path documents at length: what must not differ between
// a registered identity and an unregistered one is what the caller can *observe*.
// This path is cheaper to reason about than Login's, because there is no secret to
// verify and therefore no work to equalise — every refusal here is one indexed
// read, and none of them is reached by a stranger: an assertion only exists at all
// because an add-on holding `session.mint` made one.
func (s *Service) MintFromAddonAssertion(ctx context.Context, in AddonAssertion) (*AddonMint, error) {
	if in.Addon == "" || in.Issuer == "" || in.Subject == "" {
		return nil, ErrAssertionIncomplete
	}
	if in.AlreadySignedIn {
		// Before the lookup, so a signed-in browser cannot be used to ask whether a
		// subject is linked.
		return nil, ErrAlreadySignedIn
	}

	link, err := s.q.ResolveAddonIdentityLink(ctx, dbgen.ResolveAddonIdentityLinkParams{
		Addon:   in.Addon,
		Issuer:  in.Issuer,
		Subject: in.Subject,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSubjectNotLinked
		}
		return nil, fmt.Errorf("resolve add-on identity link: %w", err)
	}

	// The same two gates the password path applies, in the same order, against the
	// same columns. **Lockout applies here even though no password was guessed**,
	// and that is the point rather than an oversight: a lockout is a statement
	// about the account, and an external provider is not a way around one. It is
	// also the only way an operator can stop a compromised provider signing
	// somebody in while they work out what to do about it.
	if link.LockedUntil != nil && link.LockedUntil.After(time.Now()) {
		return nil, ErrAccountLocked
	}
	if link.Status != "active" {
		return nil, ErrAccountInactive
	}

	// The second factor, composed rather than bypassed (m65.md).
	//
	// The default is the safe reading: an account with TOTP enrolled meets its
	// factor *after* an assertion, exactly as it does after a right password. The
	// operator may say a provider already satisfied it — that is what
	// SatisfiesSecondFactor carries — and saying so is a deliberate act with a
	// documented consequence, which is why it is an environment variable an
	// operator sets and not a field an add-on's manifest declares.
	//
	// Note the shape: this branch is reached *before* RecordSuccessfulLogin, so an
	// assertion does not clear the account's lockout counter until the factor is
	// met. That is M53's guard, and it holds here for the same reason — clearing
	// the counter at the assertion would hand somebody who controls the provider a
	// fresh lockout budget on every attempt at six digits.
	if link.MfaEnabledAt != nil && !in.SatisfiesSecondFactor {
		// The provenance goes into the pending row rather than only into the record
		// below. The record below is about *this* event — an add-on vouched and a
		// factor is still owed — and the session m65.md wants provenance for does not
		// exist yet; CompleteSecondFactor is where it does, and it can only name the
		// add-on if the pending row carried it there (04600).
		res, err := s.pendingSecondFactor(ctx, link.UserID, in.IP, in.UserAgent,
			&addonProvenance{Addon: in.Addon, Issuer: in.Issuer})
		if err != nil {
			return nil, err
		}
		s.touchLink(ctx, link.ID)
		s.auditMint(ctx, nil, link.UserID, in, true)
		return &AddonMint{Pending: res.Pending, ExpiresAt: res.Pending.Expires}, nil
	}

	if err := s.q.RecordSuccessfulLogin(ctx, link.UserID); err != nil {
		return nil, fmt.Errorf("record login: %w", err)
	}
	res, err := s.mintSession(ctx, link.UserID, link.Email, link.Name, in.IP, in.UserAgent)
	if err != nil {
		return nil, err
	}
	s.touchLink(ctx, link.ID)
	s.auditMint(ctx, res.Identity, link.UserID, in, false)
	return &AddonMint{Login: res, ExpiresAt: res.Expires}, nil
}

// touchLink records that this link was used, and never fails a sign-in for it.
func (s *Service) touchLink(ctx context.Context, id uuid.UUID) {
	if err := s.q.TouchAddonIdentityLink(ctx, id); err != nil && s.log != nil {
		s.log.Warn("could not record when an add-on identity link was last used",
			slog.Any("error", err))
	}
}

// auditMint writes the provenance record, after the fact and outside any
// transaction — which is what Record's own documentation prescribes everywhere
// except the account deletion that cannot afford it: losing the record is worse
// than losing nothing, and losing the session is worse than losing the record.
//
// The actor is the identity the mint produced when there is one. A mint that
// stopped at the second-factor prompt has no identity yet, so the record carries
// the user id as its target and nobody as its actor — which is honest: at that
// moment nobody has signed in.
func (s *Service) auditMint(ctx context.Context, actor *Identity, userID uuid.UUID,
	in AddonAssertion, pending bool) {
	s.auditAddonSession(ctx, actor, userID, AddonSessionMint{
		Addon:                in.Addon,
		MintedBy:             MintedByLabel(in.Addon),
		Issuer:               in.Issuer,
		SecondFactorRequired: pending,
	})
}

// auditAddonSession is the write itself, split out from [Service.auditMint]
// because it has a second caller that has no AddonAssertion to hand.
//
// That caller is [MFAService.CompleteSecondFactor], which mints the session an
// add-on's assertion asked for minutes after the assertion is gone — everything
// it knows about the provenance comes off the pending row. Two callers of one
// writer rather than two writers, so the record's shape cannot start differing
// between the account that has a second factor and the account that does not.
func (s *Service) auditAddonSession(ctx context.Context, actor *Identity,
	userID uuid.UUID, ev AddonSessionMint) {
	if s.sessionAuditor == nil {
		return
	}
	if actor == nil {
		// Enough for the tenancy columns to be right and for the record to name
		// whose account it is about, without claiming a session that does not exist.
		actor = &Identity{UserID: userID}
	}
	if err := s.sessionAuditor.RecordAddonSessionMint(ctx, actor, ev); err != nil && s.log != nil {
		s.log.Warn("could not record that an add-on minted a session",
			slog.String("addon", ev.Addon), slog.Any("error", err))
	}
}

// addonProvenance is which add-on vouched, carried through a second-factor prompt.
//
// A type rather than two strings because it is passed through a function whose
// other four parameters are also strings and addresses, and because a nil one is
// the ordinary case — the password form, where nobody vouched — which reads as
// what it means at every call site.
type addonProvenance struct{ Addon, Issuer string }

// LinkAddonIdentity connects an external identity to the account of the person
// who is signed in.
//
// **The actor is the whole of the authorization**, and it is an *Identity rather
// than a user id for exactly that reason: this is the deliberate linking flow
// m65.md requires, so what it takes is proof that somebody is signed in, and the
// account it writes is theirs. Nothing an add-on asserts reaches here. A module
// that could call this could link itself to any account and then be believed about
// it, which is the takeover the linking table exists to make impossible.
func (s *Service) LinkAddonIdentity(ctx context.Context, actor *Identity,
	addon, issuer, subject string) error {
	// requireSessionActor's rule (D87), applied where it belongs: connecting a
	// provider decides how a *person* signs in, and a key is not the person.
	if err := requireSessionActor(actor, "connecting an identity provider"); err != nil {
		return err
	}
	if actor.UserID == uuid.Nil {
		return domain.ErrUnauthorized
	}
	if addon == "" || issuer == "" || subject == "" {
		return ErrAssertionIncomplete
	}
	_, err := s.q.CreateAddonIdentityLink(ctx, dbgen.CreateAddonIdentityLinkParams{
		ID:      uuid.Must(uuid.NewV7()),
		UserID:  actor.UserID,
		Addon:   addon,
		Issuer:  issuer,
		Subject: subject,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The unique key held. Either this account already has this link, or
			// another account does — and the two are told apart by reading, rather
			// than by re-pointing the row and finding out afterwards.
			existing, lerr := s.q.ResolveAddonIdentityLink(ctx, dbgen.ResolveAddonIdentityLinkParams{
				Addon: addon, Issuer: issuer, Subject: subject,
			})
			if lerr == nil && existing.UserID == actor.UserID {
				// Already connected, to this same account. Idempotent rather than an
				// error: a person who clicks connect twice, or whose browser retried a
				// callback, has asked for a state that already holds.
				return nil
			}
			return ErrSubjectLinkedElsewhere
		}
		return fmt.Errorf("link add-on identity: %w", err)
	}
	return nil
}

// **Reading and removing links are deliberately not here.** m65.md asks for the
// table, for the flow that writes it, and for what an assertion against it does;
// it asks for no management surface, and M68 is the milestone that builds one.
// Two exported functions nothing calls would be API on the most sensitive boundary
// in this product, kept alive by a test rather than by a caller. The gap that
// leaves — somebody who connects a provider cannot disconnect it — is real and is
// a deferred row rather than an omission.
