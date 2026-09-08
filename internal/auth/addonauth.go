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
	// RecordAddonIdentityLink records a provider being connected to an account or
	// disconnected from one (M70, F320). `linked` says which.
	//
	// On the same interface as the mint rather than on one of its own, because it
	// is the same seam onto internal/audit and the same nil-records-nothing rule.
	// There is one implementer.
	RecordAddonIdentityLink(ctx context.Context, actor *Identity, ev AddonIdentityLink) error
}

// AddonIdentityLink is a provider being connected to an account, or disconnected.
//
// **No subject.** The subject is the provider's identifier for a person, and an
// audit record naming it would put an opaque external id into a log this product
// keeps for years — while answering none of the questions the record exists for,
// which are *which add-on, which provider, and when*. The issuer is here for the
// reason it is on the mint: an operator responding to a compromised provider needs
// to find every account that trusted it.
type AddonIdentityLink struct {
	// Addon is the add-on that owns the connection.
	Addon string
	// Issuer is the provider as it named itself. Not an identifier of a person.
	Issuer string
	// UserID is whose account the link is on, which is not always the actor's:
	// an operator severing a link is acting on somebody else's account.
	UserID uuid.UUID
	// Linked distinguishes the two actions. False is a disconnection.
	Linked bool
	// ByOperator says the actor reached this through the Add-on manager rather
	// than through their own account page. It is the difference between *I removed
	// my own credential* and *somebody removed mine*, which is the first question
	// asked when a person finds they can no longer sign in.
	ByOperator bool
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
		// **The tenancy has to be resolved, not left zero** (review finding 11a).
		// RecordTx takes the organization from the actor and this action is not
		// InstanceWide, so a zero OrgID put the row in the instance-wide log while
		// the very same act, on an account with no second factor, landed in the
		// organization's. One act, split across two audit surfaces by whether the
		// account happens to have TOTP configured.
		//
		// Nobody has signed in yet and this still does not claim they have: the
		// identity is loaded for its tenancy alone, and everything that would
		// assert a session — the session id — stays unset. A lookup that fails
		// leaves the record where it was, which is worse than tenanted and better
		// than absent.
		actor = &Identity{UserID: userID}
		if tenant, err := s.tenancyFor(ctx, userID); err != nil {
			if s.log != nil {
				s.log.Warn("could not resolve whose organization an add-on's pending "+
					"sign-in belongs to; the record is instance-wide",
					slog.String("addon", ev.Addon), slog.Any("error", err))
			}
		} else {
			actor.WorkspaceID, actor.OrgID = tenant.ID, tenant.OrganizationID
		}
	}
	// Detached, for the reason every other audit write added in this phase is
	// (review finding 11b): the act has already happened, and this context is the
	// guest invocation's — cancellable and bounded by the route deadline. A mint
	// that lands near that deadline would commit the session and lose its record,
	// which is exactly the failure F320 is a row about.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditRecordTimeout)
	defer cancel()
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
	// A standing credential was just written, and every other credential on this
	// account is audited (F320). Without this, an operator reading the log of a
	// compromised account sees the sessions an identity minted and cannot see when
	// the identity was connected — which is the act that made those sessions
	// possible.
	s.auditIdentityLink(ctx, actor, AddonIdentityLink{
		Addon: addon, Issuer: issuer, UserID: actor.UserID, Linked: true,
	})
	return nil
}

// auditIdentityLink records a connect or a disconnect, best-effort.
//
// Best-effort for [Service.auditAddonSession]'s reason: the act has happened by
// the time this runs, and failing it afterwards would leave a link that exists
// and a caller told it does not.
func (s *Service) auditIdentityLink(ctx context.Context, actor *Identity, ev AddonIdentityLink) {
	if s.sessionAuditor == nil {
		return
	}
	// Detached for [Service.auditAddonSession]'s reason (review finding 11b): the
	// link exists by the time this runs, and the caller's context is the guest
	// invocation's.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditRecordTimeout)
	defer cancel()
	if err := s.sessionAuditor.RecordAddonIdentityLink(ctx, actor, ev); err != nil && s.log != nil {
		s.log.Warn("could not record a change to an account's connected identities; "+
			"the change itself happened",
			slog.String("addon", ev.Addon),
			slog.Bool("linked", ev.Linked), slog.Any("error", err))
	}
}

// ConnectedIdentity is one provider an account has connected, for a page that
// lists them.
//
// No subject, for [AddonIdentityLink]'s reason. Email and Name are set only on
// the operator's listing, where the question is *whose account is this*.
type ConnectedIdentity struct {
	ID         uuid.UUID
	Addon      string
	Issuer     string
	CreatedAt  time.Time
	LastUsedAt *time.Time

	UserID uuid.UUID
	Email  string
	Name   string
}

// ConnectedIdentities is every provider one account has connected.
//
// **M70, and it is half of what F315 was open for.** M65 built the table and
// deliberately shipped no way to see a row; a link signs somebody in with no
// password and no second factor of this product's, so *what is connected to my
// account* is a question the account's owner is entitled to ask.
//
// The actor's own account only. An operator asking about somebody else's uses
// [Service.IdentitiesForAddon], which costs a different permission and answers a
// different question.
func (s *Service) ConnectedIdentities(ctx context.Context, actor *Identity) ([]ConnectedIdentity, error) {
	if err := requireSessionActor(actor, "listing connected identity providers"); err != nil {
		return nil, err
	}
	if actor.UserID == uuid.Nil {
		return nil, domain.ErrUnauthorized
	}
	rows, err := s.q.ListAddonIdentityLinksForUser(ctx, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("list connected identities: %w", err)
	}
	out := make([]ConnectedIdentity, 0, len(rows))
	for _, r := range rows {
		out = append(out, ConnectedIdentity{
			ID: r.ID, Addon: r.Addon, Issuer: r.Issuer,
			CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt,
			UserID: actor.UserID,
		})
	}
	return out, nil
}

// DisconnectIdentity severs one of the actor's own connected providers.
//
// The other half of F315, from the side of the person whose account it is. The
// statement carries the user id, so an id belonging to somebody else is
// [domain.ErrNotFound] rather than a removal.
//
// **A disconnection is not a sign-out.** Sessions this link already minted stay
// valid until they lapse or are revoked, and that is deliberate rather than
// overlooked: revoking them is the account's session control, which exists and is
// a separate act with its own record. What this removes is the standing way back
// in.
func (s *Service) DisconnectIdentity(ctx context.Context, actor *Identity, id uuid.UUID) error {
	if err := requireSessionActor(actor, "disconnecting an identity provider"); err != nil {
		return err
	}
	if actor.UserID == uuid.Nil {
		return domain.ErrUnauthorized
	}
	gone, err := s.q.DeleteAddonIdentityLink(ctx, dbgen.DeleteAddonIdentityLinkParams{
		ID: id, UserID: actor.UserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("disconnect identity: %w", err)
	}
	s.auditIdentityLink(ctx, actor, AddonIdentityLink{
		Addon: gone.Addon, Issuer: gone.Issuer, UserID: gone.UserID, Linked: false,
	})
	return nil
}

// IdentitiesForAddon is every account one add-on has connected, for the operator.
//
// The manager's half of F315, and the reason it is a separate function rather
// than a parameter on [Service.ConnectedIdentities]: it answers about accounts
// that are not the caller's, so it carries the email, and the permission it costs
// is the instance's rather than the account's. The caller checks that permission —
// this package has no view of the instance grant vocabulary, which is why
// [Service.DisconnectIdentityFor] takes the same shape.
func (s *Service) IdentitiesForAddon(ctx context.Context, addon string) ([]ConnectedIdentity, error) {
	rows, err := s.q.ListAddonIdentityLinksForAddon(ctx, addon)
	if err != nil {
		return nil, fmt.Errorf("list identities for add-on %q: %w", addon, err)
	}
	out := make([]ConnectedIdentity, 0, len(rows))
	for _, r := range rows {
		out = append(out, ConnectedIdentity{
			ID: r.ID, Addon: addon, Issuer: r.Issuer,
			CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt,
			UserID: r.UserID, Email: r.Email, Name: r.Name,
		})
	}
	return out, nil
}

// DisconnectIdentityFor severs a link on somebody else's account, for an operator
// responding to a compromised provider.
//
// **The owner is resolved from the row rather than taken from the caller**, which
// is what lets one statement serve both surfaces: the delete's predicate is
// (id, user_id) either way, and here the user_id comes from the row this function
// just read rather than from an argument that could name the wrong account.
//
// The permission is the caller's to check, for [Service.IdentitiesForAddon]'s
// reason, and `by_operator` is what tells the two records apart afterwards.
func (s *Service) DisconnectIdentityFor(
	ctx context.Context, actor *Identity, addon string, id uuid.UUID,
) error {
	rows, err := s.q.ListAddonIdentityLinksForAddon(ctx, addon)
	if err != nil {
		return fmt.Errorf("list identities for add-on %q: %w", addon, err)
	}
	owner := uuid.Nil
	var issuer string
	for _, r := range rows {
		if r.ID == id {
			owner, issuer = r.UserID, r.Issuer
			break
		}
	}
	if owner == uuid.Nil {
		return domain.ErrNotFound
	}
	if _, err := s.q.DeleteAddonIdentityLink(ctx, dbgen.DeleteAddonIdentityLinkParams{
		ID: id, UserID: owner,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("disconnect identity: %w", err)
	}
	s.auditIdentityLink(ctx, actor, AddonIdentityLink{
		Addon: addon, Issuer: issuer, UserID: owner, Linked: false, ByOperator: true,
	})
	return nil
}

// auditRecordTimeout bounds an audit write this file detaches onto its own
// context. The same five seconds internal/addon uses for the same reason and
// under the same name there — a detached write needs a bound, or a database that
// has stopped answering leaves a goroutine per act.
const auditRecordTimeout = 5 * time.Second

// tenancyFor is which organization an account acts in, for an audit record
// written before anybody has signed in.
//
// Its one caller is [Service.auditAddonSession]'s pending-second-factor path,
// which has a user id and nothing else. Deliberately the same resolution
// [Service.IdentityForEmail] uses — the workspace the person would land in — so
// the record lands where the same act lands for an account with no second
// factor.
//
// An account belonging to no organization answers ErrNoWorkspace, and the caller
// leaves the record instance-wide: that is where an act by such an account
// belongs, rather than a wrong organization.
func (s *Service) tenancyFor(ctx context.Context, userID uuid.UUID) (dbgen.Workspace, error) {
	return s.resolveWorkspace(ctx, userID, nil, nil)
}
