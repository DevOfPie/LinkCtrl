package auth

// A second factor (M53): enrolment, the login challenge, disabling, and the
// recovery codes that make all three survivable.
//
// # Why this is in internal/auth and not beside it
//
// internal/recovery and internal/account are each their own package, and the
// reason both give is that internal/audit imports internal/auth, so a service
// that has to write an audit record cannot live here. That reason does not apply
// to a second factor: the interposition is *inside* auth.Service.Login, between
// the password and the session mint, and moving it out would mean exporting the
// mint. So this follows the other precedent in this package instead — the one
// APIKeyService already sets, where auth declares the narrow interface it needs
// (APIKeyAuditor) and internal/audit implements it on the other side of the seam.
// MFAAuditor and MFANotifier below are that seam, for the same reason and in the
// same shape.
//
// # The three credentials, and what each is for
//
// **The TOTP secret** is the factor. Encrypted at rest under MFA_SECRET_KEY,
// because verifying a time-based code means recomputing it — see mfasecret.go for
// why that key is its own and not the API-key pepper.
//
// **Ten recovery codes** are the answer to a lost phone. Single-use, hashed with
// the same HashOpaqueToken the rest of the product uses, shown once. m53.md makes
// them the hard dependency on M51 — *recovery lands first or this does not land* —
// because a second factor multiplies a lockout that had only just stopped being
// permanent.
//
// **A pending login** is the few minutes between a right password and a session.
// It is the newest short-lived credential in a product that has deliberately had
// few, and it sits where getting a state machine wrong turns a login page into an
// authentication bypass, so every transition it has is a SQL predicate rather than
// a branch: single use is decided by `ConsumeMFAPendingLogin`, replay is decided
// by `AcceptMFAStep`, and both refuse by affecting no rows.
//
// # What this milestone deliberately does not build
//
// No WebAuthn, no passkeys, no SMS, no push — each is a separate credential model
// with its own recovery story, and SMS is worse than what it replaces. No
// organization-level *require MFA for all members* policy, which needs a
// permission, an enforcement point on every session resolution and an answer for
// members who cannot enrol: a policy feature wearing an authentication milestone's
// clothes. Both absences are decisions and m53.md is where they are argued.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// MFAPendingTTL is how long the step between a right password and a session
// stays usable.
//
// **Five minutes, and m53.md asks for it to be a number a test asserts.** The
// window is a person reading six digits off a phone that is already in their
// hand, plus the time it takes to find the phone. Longer is a credential lying
// about for no reason; much shorter fails somebody whose phone locked itself while
// they were typing their password.
const MFAPendingTTL = 5 * time.Minute

// MFARecoveryCodeCount is how many codes an enrolment issues. Ten, which m53.md
// names, and which is the number every product that does this settled on: enough
// that spending one is not an event, few enough to write on one line of paper.
const MFARecoveryCodeCount = 10

// recoveryCodeChars is the alphabet a recovery code is drawn from.
//
// Crockford's base32 in lower case: the digits and the letters, less `i`, `l`,
// `o` and `u`. The first three are the pairs somebody transcribing from paper gets
// wrong, and `u` is left out so the alphabet cannot spell anything a person has to
// read aloud to support.
//
// Exactly thirty-two symbols, which matters for more than tidiness: 256 divides by
// 32, so masking a random byte to five bits picks uniformly and needs no rejection
// loop. An alphabet of any other size would need one, and a rejection loop written
// carelessly is how a code generator ends up biased.
const recoveryCodeChars = "0123456789abcdefghjkmnpqrstvwxyz"

// recoveryCodeLength is the symbol count in one code, before the separator.
//
// Ten symbols over a 32-symbol alphabet is fifty bits. The codes are presented at
// a prompt whose failures count against the account's own lockout, so the guessing
// bound is the lockout rather than the entropy — fifty bits is what makes that
// true rather than what does the work.
const recoveryCodeLength = 10

// recoveryCodeGroup is where the hyphen goes. Halves, because a ten-character
// string with no break is one somebody loses their place in.
const recoveryCodeGroup = 5

// Errors this package returns for the second factor, and that a caller
// distinguishes.
var (
	// ErrMFAUnavailable is any second-factor operation on an instance that has
	// not configured MFA_SECRET_KEY. Wraps ErrMFAKeyMissing so a caller may test
	// for either.
	ErrMFAUnavailable = fmt.Errorf("auth: the second factor is not available on this instance: %w",
		ErrMFAKeyMissing)

	// ErrMFAAlreadyEnabled is enrolling an account that already has a factor.
	// Disabling and enrolling again is the route to a new secret, deliberately:
	// replacing one in place would mean an account briefly having two, and the
	// recovery codes belonging to neither.
	ErrMFAAlreadyEnabled = errors.New("auth: this account already has a second factor")

	// ErrMFANotEnabled is disabling or regenerating on an account with no factor.
	ErrMFANotEnabled = errors.New("auth: this account has no second factor")

	// ErrMFACodeInvalid is every rejected second factor: a wrong code, a code
	// from a step already spent, a recovery code that does not match or has been
	// used, and a secret this instance's key cannot read.
	//
	// **One error for all of them, and the last one is why it is worth saying.**
	// A secret that will not decrypt is an operator's mistake and not the
	// person's, and it is still answered here rather than surfaced — telling
	// whoever is at the prompt that the *server* cannot read the secret hands a
	// stranger a way to probe which accounts are enrolled and what state the
	// instance's configuration is in. The operator's copy of that fact is the log
	// line, which names it plainly.
	ErrMFACodeInvalid = errors.New("auth: that code is not valid")

	// ErrMFAChallengeInvalid is a pending login that cannot be completed: no such
	// token, lapsed, already spent, or an account whose state changed while the
	// prompt was open.
	//
	// Collapsed for the reason recovery.ErrNotResettable is, and answered by
	// sending the person back to the sign-in form. A caller enumerating cannot
	// tell the four apart.
	ErrMFAChallengeInvalid = errors.New("auth: this sign-in can no longer be completed")
)

// PendingSecondFactor is the challenge a caller hands the browser instead of a
// session.
//
// The token is bearer-shaped and is the only thing that identifies the sign-in in
// flight; the account is deliberately not named in it, so a caller cannot render
// "signing in as …" from a value somebody else's browser could be holding.
type PendingSecondFactor struct {
	Token   string
	Expires time.Time
}

// MFAChangeKind is what happened to an account's second factor.
type MFAChangeKind string

const (
	MFAEnabled                  MFAChangeKind = "enabled"
	MFADisabled                 MFAChangeKind = "disabled"
	MFARecoveryCodeUsed         MFAChangeKind = "recovery_code_used"
	MFARecoveryCodesRegenerated MFAChangeKind = "recovery_codes_regenerated"
)

// MFAChange is one such event, as the audit and notification seams see it.
//
// It carries no secret and no code — not the TOTP secret, not a recovery code,
// not a hash of one. What a reader of either surface gets is *what changed* and
// *how many codes are left*, which is the whole of what either is read for.
type MFAChange struct {
	Kind   MFAChangeKind
	UserID uuid.UUID
	Email  string
	// RecoveryCodesRemaining is the unspent count after the change. Meaningful
	// for every kind: ten after an enrolment or a regeneration, zero after a
	// disable, and the number that makes "you have two left" worth sending after
	// a recovery code is spent.
	RecoveryCodesRemaining int64
}

// MFAAuditor records second-factor changes.
//
// The seam onto internal/audit, in the shape APIKeyAuditor already established:
// internal/audit imports this package to resolve an actor into the label it
// stores, so this package cannot import that one. Nil records nothing.
type MFAAuditor interface {
	RecordMFAChange(ctx context.Context, actor *Identity, ev MFAChange) error
}

// MFANotifier tells the account holder.
//
// A separate seam from the auditor because the audiences are different: an audit
// record is read by whoever administers the instance, and this reaches the person
// whose credential changed. m53.md asks for it by name on the path that matters
// most — *a recovery code being spent is the signal that either the phone is gone
// or somebody else has it* — and the others are here because a second factor
// appearing on, or vanishing from, your own account is the same kind of news.
//
// The recipient is the subject rather than a parameter: every one of these events
// is about one account and is told to that account.
type MFANotifier interface {
	NotifyMFAChange(ctx context.Context, ev MFAChange) error
}

// MFAConfig is what an MFAService needs.
type MFAConfig struct {
	// Auth verifies the account's own password and mints the session a completed
	// second factor earns. Required: there is no second factor without a first
	// one, and no second place a session is created.
	Auth *Service
	// Cipher reads and writes the secret at rest. **Nil is an instance with no
	// MFA_SECRET_KEY**, and it is the one dependency here whose absence is a
	// refusal rather than a degradation — an enrolled account signs in with a
	// recovery code, which needs no key, and everything else refuses.
	Cipher *MFACipher
	// Issuer is what an authenticator app files the entry under. The instance's
	// own host, so somebody with three of these on their phone can tell them
	// apart.
	Issuer string
	// Audit records the change. Nil records nothing.
	Audit MFAAuditor
	// Notify tells the account holder. Nil tells nobody.
	Notify MFANotifier
	Log    *slog.Logger
}

// MFAService owns the second factor.
type MFAService struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
	cfg  MFAConfig
	log  *slog.Logger
}

func NewMFAService(pool *pgxpool.Pool, cfg MFAConfig) (*MFAService, error) {
	if cfg.Auth == nil {
		return nil, errors.New("auth: mfa service has no auth service")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &MFAService{pool: pool, q: dbgen.New(pool), cfg: cfg, log: cfg.Log}, nil
}

// Available reports whether this instance can enrol anybody.
//
// Read by both surfaces before they draw the enrolment offer, so nobody starts
// something the instance was never going to finish — the shape ForgotPage uses for
// a mail-free instance. Every operation below refuses again on its own, because a
// surface remembering to ask is not the invariant.
func (m *MFAService) Available() bool { return m != nil && m.cfg.Cipher != nil }

// --- what the account page shows ----------------------------------------------

// MFAStatus is the second factor as the account page describes it.
type MFAStatus struct {
	// Available is the instance's answer: is there a key to encrypt a secret
	// with. False draws an explanation instead of an offer.
	Available bool
	Enabled   bool
	EnabledAt *time.Time
	// RecoveryCodesRemaining is how many unspent codes are left. Rendered as a
	// number because it is one somebody acts on: three left is a prompt to
	// regenerate, and zero with a lost phone is a conversation with the operator.
	RecoveryCodesRemaining int64
}

// Status reads one account's second factor.
func (m *MFAService) Status(ctx context.Context, actor *Identity) (MFAStatus, error) {
	out := MFAStatus{Available: m.Available()}
	if actor == nil {
		return out, ErrInvalidCredentials
	}
	row, err := m.q.GetUserMFA(ctx, actor.UserID)
	if err != nil {
		return out, fmt.Errorf("look up second factor: %w", err)
	}
	out.Enabled = row.MfaEnabledAt != nil
	out.EnabledAt = row.MfaEnabledAt
	if out.Enabled {
		n, err := m.q.CountUnusedMFARecoveryCodes(ctx, actor.UserID)
		if err != nil {
			return out, fmt.Errorf("count recovery codes: %w", err)
		}
		out.RecoveryCodesRemaining = n
	}
	return out, nil
}

// --- enrolment ------------------------------------------------------------------

// MFAEnrolment is an offer: a fresh secret and the URI that carries it.
//
// **Nothing is stored yet**, which is m53.md's *half-enrolled is not a state this
// product has* expressed as an absence. The secret exists in this value and in the
// form the person is looking at, and it reaches `users.mfa_secret` only in the
// statement that also sets `mfa_enabled_at`, and only after a code computed from
// it has verified. An enrolment that is started and abandoned leaves the account
// byte-for-byte as it was, and TestAnAbandonedEnrolmentLeavesTheAccountAlone is
// what holds that.
//
// The cost of not storing it is that the offer travels back through the form, so
// the confirm step is trusting the browser to return the secret it was given.
// Origin-checked CSRF is what stops a third party posting a secret of their own —
// the same protection every other state-changing form in this product rests on —
// and the alternative, parking a candidate secret on the account row, is precisely
// the half-enrolled state the milestone forbids.
type MFAEnrolment struct {
	// Secret is base32, as an authenticator app expects it and as it is shown in
	// text beside the QR code — because a person enrolling from the same device
	// cannot scan their own screen.
	Secret string
	// URI is the `otpauth://` string the QR code encodes. Rendered through
	// internal/qr by the surface; this package does not know what a QR code is.
	URI string
}

// BeginEnrolment offers a secret.
//
// Session actors only. A key is not a person and has no second factor to enrol,
// and D87's limb — the session is the authority for operations whose subject is
// the person — covers this one exactly.
func (m *MFAService) BeginEnrolment(ctx context.Context, actor *Identity) (*MFAEnrolment, error) {
	if err := requireSessionActor(actor, "enrolling a second factor"); err != nil {
		return nil, err
	}
	if !m.Available() {
		return nil, ErrMFAUnavailable
	}

	row, err := m.q.GetUserMFA(ctx, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("look up second factor: %w", err)
	}
	if row.MfaEnabledAt != nil {
		return nil, ErrMFAAlreadyEnabled
	}

	secret, err := NewTOTPSecret()
	if err != nil {
		return nil, err
	}
	return &MFAEnrolment{
		Secret: secret,
		URI:    TOTPURI(m.issuer(), row.Email, secret),
	}, nil
}

// MFAEnrolled is what a completed enrolment hands back, once.
type MFAEnrolled struct {
	// RecoveryCodes are shown on this response and never again. Nothing stores
	// them in a readable form, so a person who does not write them down has the
	// regenerate button and nothing else.
	RecoveryCodes []string
}

// ConfirmEnrolment turns an offered secret into the account's second factor.
//
// The order is the whole of it: verify a code computed from the offered secret,
// then write the secret and the timestamp in one statement, then issue the
// recovery codes — all inside one transaction, so an account never has a factor
// without codes or codes without a factor.
func (m *MFAService) ConfirmEnrolment(
	ctx context.Context, actor *Identity, secret, code string,
) (*MFAEnrolled, error) {
	if err := requireSessionActor(actor, "enrolling a second factor"); err != nil {
		return nil, err
	}
	if !m.Available() {
		return nil, ErrMFAUnavailable
	}

	// The presented code is checked against the secret in hand before anything is
	// read or written. It proves the person has the secret in an authenticator,
	// which is the only thing enrolment is for.
	step, ok := TOTPVerify(secret, code, time.Now())
	if !ok {
		return nil, ErrMFACodeInvalid
	}

	sealed, err := m.cfg.Cipher.Seal(secret)
	if err != nil {
		return nil, err
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := m.q.WithTx(tx)

	row, err := q.LockUserMFA(ctx, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("lock account: %w", err)
	}
	if row.MfaEnabledAt != nil {
		return nil, ErrMFAAlreadyEnabled
	}

	// The enrolling step is stamped as the replay floor, so the code just used to
	// enrol cannot also be the code that signs somebody in — the two prompts are
	// seconds apart and would otherwise share a window.
	n, err := q.EnableUserMFA(ctx, dbgen.EnableUserMFAParams{
		UserID: actor.UserID, Secret: &sealed, FirstStep: &step,
	})
	if err != nil {
		return nil, fmt.Errorf("enable second factor: %w", err)
	}
	if n == 0 {
		// Unreachable while the row above was locked and unenrolled, and checked
		// anyway: zero rows means something else enrolled inside this
		// transaction's window, and rolling back is the only correct answer.
		return nil, ErrMFAAlreadyEnabled
	}

	codes, err := issueRecoveryCodes(ctx, q, actor.UserID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	m.announce(ctx, actor, MFAChange{
		Kind: MFAEnabled, UserID: actor.UserID, Email: row.Email,
		RecoveryCodesRemaining: int64(len(codes)),
	})
	return &MFAEnrolled{RecoveryCodes: codes}, nil
}

// --- recovery codes ---------------------------------------------------------------

// RegenerateRecoveryCodes voids the previous set and issues a new one.
//
// Every code, spent ones included, because the previous set is void in full and a
// count of leftovers from a void set would be a lie. The account keeps the same
// TOTP secret: this is the answer to *I have lost the paper*, not to *I have lost
// the phone*.
func (m *MFAService) RegenerateRecoveryCodes(
	ctx context.Context, actor *Identity,
) ([]string, error) {
	if err := requireSessionActor(actor, "regenerating recovery codes"); err != nil {
		return nil, err
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := m.q.WithTx(tx)

	row, err := q.LockUserMFA(ctx, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("lock account: %w", err)
	}
	if row.MfaEnabledAt == nil {
		return nil, ErrMFANotEnabled
	}
	if _, err := q.DeleteMFARecoveryCodes(ctx, actor.UserID); err != nil {
		return nil, fmt.Errorf("clear recovery codes: %w", err)
	}
	codes, err := issueRecoveryCodes(ctx, q, actor.UserID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	m.announce(ctx, actor, MFAChange{
		Kind: MFARecoveryCodesRegenerated, UserID: actor.UserID, Email: row.Email,
		RecoveryCodesRemaining: int64(len(codes)),
	})
	return codes, nil
}

// issueRecoveryCodes mints a set and stores their hashes.
//
// Inside the caller's transaction, always. Both callers are writing something else
// in the same breath — an enrolment or a deletion of the previous set — and a set
// that half-exists is a person holding codes that do not work.
func issueRecoveryCodes(
	ctx context.Context, q *dbgen.Queries, userID uuid.UUID,
) ([]string, error) {
	codes := make([]string, 0, MFARecoveryCodeCount)
	for i := 0; i < MFARecoveryCodeCount; i++ {
		code, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		if err := q.InsertMFARecoveryCode(ctx, dbgen.InsertMFARecoveryCodeParams{
			ID:       uuid.Must(uuid.NewV7()),
			UserID:   userID,
			CodeHash: HashOpaqueToken(normalizeRecoveryCode(code)),
		}); err != nil {
			return nil, fmt.Errorf("store recovery code: %w", err)
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// newRecoveryCode returns one code, hyphenated.
//
// Five bits per symbol out of crypto/rand, masked rather than reduced: the
// alphabet is exactly thirty-two symbols, so `b & 31` is uniform and needs no
// rejection loop. A modulo against an alphabet of any other size would bias the
// early symbols, which is the classic way a generator like this is quietly wrong.
func newRecoveryCode() (string, error) {
	buf := make([]byte, recoveryCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read recovery code: %w", err)
	}
	var b strings.Builder
	for i, v := range buf {
		if i > 0 && i%recoveryCodeGroup == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(recoveryCodeChars[v&31])
	}
	return b.String(), nil
}

// normalizeRecoveryCode is what is hashed, on the way in and on the way back.
//
// Case, spaces and hyphens are removed, so a code typed from paper matches
// however the person grouped it. It is applied to the generated code before
// storage too — one function on both sides, rather than two that agree.
func normalizeRecoveryCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(code) {
		if r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// --- disabling ----------------------------------------------------------------------

// Disable takes the second factor away.
//
// **The password and a current code, or the password and a recovery code.** Both
// halves, because either alone is a downgrade somebody else can perform: a stolen
// session would otherwise remove the factor it was supposed to be stopped by, and
// a phone found unlocked on a train would do the same. m53.md asks for exactly
// this pairing.
//
// Session actors only, and this is the D87 limb m53.md names by test: an API key
// is not a person, and disabling somebody's second factor is an operation whose
// subject is the person.
func (m *MFAService) Disable(ctx context.Context, actor *Identity, password, code string) error {
	if err := requireSessionActor(actor, "disabling the second factor"); err != nil {
		return err
	}
	if err := m.cfg.Auth.VerifyPassword(ctx, actor.UserID, password); err != nil {
		return err
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := m.q.WithTx(tx)

	row, err := q.LockUserMFA(ctx, actor.UserID)
	if err != nil {
		return fmt.Errorf("lock account: %w", err)
	}
	if row.MfaEnabledAt == nil {
		return ErrMFANotEnabled
	}

	// The kind is deliberately not acted on. A recovery code spent to turn the
	// factor off is spent, but every code in the set is deleted three statements
	// below — telling somebody "a recovery code was used, you have nine left"
	// alongside "two-factor authentication is off, every code has been removed"
	// would be two notices about one act, one of which is untrue by the time they
	// read it.
	if _, err := m.consumeFactor(ctx, q, row.ID, row.MfaSecret, row.MfaLastStep, code); err != nil {
		return err
	}

	// One transaction for all three, which is m53.md's sentence rendered as a
	// unit of work: clearing `mfa_enabled_at` clears the secret and every unused
	// recovery code. The pending logins go too — a prompt that outlives the factor
	// it was prompting for is a credential with nothing left to check.
	if _, err := q.DisableUserMFA(ctx, actor.UserID); err != nil {
		return fmt.Errorf("disable second factor: %w", err)
	}
	if _, err := q.DeleteMFARecoveryCodes(ctx, actor.UserID); err != nil {
		return fmt.Errorf("clear recovery codes: %w", err)
	}
	if _, err := q.DeleteMFAPendingLoginsFor(ctx, actor.UserID); err != nil {
		return fmt.Errorf("clear pending logins: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	m.announce(ctx, actor, MFAChange{
		Kind: MFADisabled, UserID: actor.UserID, Email: row.Email,
	})
	return nil
}

// --- the login challenge --------------------------------------------------------------

// pendingSecondFactor is what Login returns instead of a session.
//
// On *Service rather than on MFAService, because Login is the caller and Login
// does not know whether an MFAService was wired. That is deliberate: an instance
// that lost MFA_SECRET_KEY still has enrolled accounts, and those accounts must
// still stop at the prompt rather than sign in on the password alone. The
// challenge is minted from the account row, which is all it needs.
func (s *Service) pendingSecondFactor(
	ctx context.Context, userID uuid.UUID, ip netip.Addr, userAgent string,
) (*LoginResult, error) {
	// Supersede whatever was outstanding, so there is never more than one live
	// prompt per account and an abandoned tab is not sharing its window with the
	// browser in front of the person. Same reasoning as recovery's
	// ConsumePasswordResets, and the same statement shape.
	if _, err := s.q.DeleteMFAPendingLoginsFor(ctx, userID); err != nil {
		return nil, fmt.Errorf("supersede pending logins: %w", err)
	}

	token, hash, err := NewOpaqueToken(sessionTokenBytes)
	if err != nil {
		return nil, err
	}
	ipPrefix := AnonymizeIP(ip)
	row, err := s.q.CreateMFAPendingLogin(ctx, dbgen.CreateMFAPendingLoginParams{
		ID:        uuid.Must(uuid.NewV7()),
		UserID:    userID,
		TokenHash: hash,
		IpPrefix:  nullable(ipPrefix),
		UserAgent: nullable(truncate(userAgent, 512)),
		ExpiresAt: time.Now().Add(MFAPendingTTL),
	})
	if err != nil {
		return nil, fmt.Errorf("create pending login: %w", err)
	}
	return &LoginResult{Pending: &PendingSecondFactor{
		Token: token, Expires: row.ExpiresAt,
	}}, nil
}

// CompleteSecondFactor finishes a sign-in that stopped at the prompt.
//
// The only place a pending login becomes a session, and the order below is the
// state machine m53.md wants adversarially tested:
//
//  1. The pending row is located and locked. Anything wrong with it —
//     unknown, lapsed, spent, an account that stopped being active — is one
//     refusal.
//  2. The factor is consumed: a TOTP code against the decrypted secret with its
//     step recorded, or a recovery code spent. A failure here increments the
//     account's own `failed_login_count` through the same policy a wrong password
//     does, so the two share a budget rather than the second factor handing out a
//     fresh one.
//  3. The pending row is spent, in the same transaction. Single use is the
//     statement's predicate, so two browsers presenting the same token race into
//     it and exactly one wins.
//  4. Only then is `RecordSuccessfulLogin` called and a session minted.
func (m *MFAService) CompleteSecondFactor(
	ctx context.Context, token, code string, ip netip.Addr, userAgent string,
) (*LoginResult, error) {
	if token == "" {
		return nil, ErrMFAChallengeInvalid
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := m.q.WithTx(tx)

	row, err := q.LockMFAPendingLogin(ctx, HashOpaqueToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMFAChallengeInvalid
		}
		return nil, fmt.Errorf("look up pending login: %w", err)
	}
	// Four refusals collapsed into one answer, each with its own test. The last
	// two are re-checked here rather than only at the password step because an
	// account's state can change while a prompt sits open — the same reasoning
	// recovery.Reset applies to a link sitting in an inbox.
	if row.ConsumedAt != nil ||
		!row.ExpiresAt.After(time.Now()) ||
		row.Status != "active" ||
		row.MfaEnabledAt == nil {
		return nil, ErrMFAChallengeInvalid
	}

	kind, err := m.consumeFactor(ctx, q, row.UserID, row.MfaSecret, row.MfaLastStep, code)
	if err != nil {
		// Rolled back *before* the counter is charged, and explicitly rather than
		// by the deferred call. The increment is a write to `users` from the pool,
		// and leaving this transaction open across it means a second connection
		// waiting on locks the first one is holding — which is the deadlock
		// mfaFactorKind describes, in its other form.
		_ = tx.Rollback(ctx)
		if errors.Is(err, ErrMFACodeInvalid) {
			// **The same counter and the same policy as a failed password.** A
			// second factor with a budget of its own would give an attacker who
			// already has the password an unlimited supply of guesses, which is
			// backwards; this is why Login does not clear the counter before the
			// factor is presented.
			m.recordFailedFactor(ctx, row.UserID)
		}
		return nil, err
	}

	spent, err := q.ConsumeMFAPendingLogin(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("spend pending login: %w", err)
	}
	if spent == 0 {
		// Unreachable while the row above was unconsumed and locked, and checked
		// anyway: zero rows means another request spent it inside this
		// transaction's window.
		return nil, ErrMFAChallengeInvalid
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// The sign-in has succeeded, and this is the point at which that is true. The
	// counter and the lockout clear here rather than at the password step.
	if err := m.q.RecordSuccessfulLogin(ctx, row.UserID); err != nil {
		return nil, fmt.Errorf("record login: %w", err)
	}

	// m53.md: *using one is audited and notifies the account, because a recovery
	// code being spent is the signal that either the phone is gone or somebody
	// else has it.* After the commit, because both writes touch tables with a
	// foreign key to the row this transaction was holding.
	if kind == factorRecoveryCode {
		m.announceRecoveryCodeUse(ctx, row.UserID)
	}

	// The network facts come from the pending row rather than from this request,
	// so the session records where the sign-in started. Both posts are the same
	// browser in every ordinary case; taking the first is what makes that a
	// recorded fact instead of an assumption. The user agent is the exception —
	// a prefix cannot be turned back into an address, and the pending row's
	// prefix is what the session column wants.
	loginIP := ip
	if row.IpPrefix != nil {
		if prefix, perr := netip.ParsePrefix(*row.IpPrefix); perr == nil {
			loginIP = prefix.Addr()
		}
	}
	agent := userAgent
	if row.UserAgent != nil {
		agent = *row.UserAgent
	}
	return m.cfg.Auth.mintSession(ctx, row.UserID, row.Email, row.Name, loginIP, agent)
}

// mfaFactorKind is which of the two credentials a presentation turned out to be.
//
// Returned rather than announced from inside consumeFactor, and that is a bug fix
// rather than a style: the notification for a spent recovery code inserts a row
// into `notifications`, which carries a foreign key to `users`, and an insert
// against a foreign key takes a KEY SHARE lock on the referenced row. Both callers
// hold an open transaction at that point and one of them holds `FOR UPDATE` on
// exactly that row, so announcing from there waits on a lock the same call stack
// is holding. It deadlocked until the test suite sat on it for seven minutes.
type mfaFactorKind int

const (
	factorTOTP mfaFactorKind = iota
	factorRecoveryCode
)

// consumeFactor accepts one presentation of a second factor, TOTP or recovery
// code, spends whatever it accepted, and says which it was.
//
// One function for the login prompt and for the disable form, because m53.md asks
// both to take either kind and two implementations that agree is how one of them
// stops agreeing. Everything it refuses is ErrMFACodeInvalid.
//
// **A recovery code is tried only when the TOTP code does not match**, and the
// order matters for a reason beyond cost. A six-digit string is not a recovery
// code's shape, so trying recovery first would spend a database round trip on
// every ordinary sign-in; trying it second means the only presentations that reach
// the table are the ones that were not a valid current code.
//
// **It announces nothing.** Everything it does is inside the caller's transaction,
// and an audit record or a notification written from in there either deadlocks —
// see mfaFactorKind — or commits a claim the transaction may still roll back.
func (m *MFAService) consumeFactor(
	ctx context.Context, q *dbgen.Queries,
	userID uuid.UUID, sealed *string, lastStep *int64, code string,
) (mfaFactorKind, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return factorTOTP, ErrMFACodeInvalid
	}

	// The TOTP limb, which needs the key. An instance that has lost
	// MFA_SECRET_KEY falls straight through to the recovery limb — the documented
	// fallback, and the reason losing the key is survivable.
	if sealed != nil && m.Available() {
		secret, err := m.cfg.Cipher.Open(*sealed)
		switch {
		case err == nil:
			if step, ok := TOTPVerify(secret, code, time.Now()); ok {
				// The replay guard, applied as a write. `AcceptMFAStep` refuses
				// anything not strictly greater than the step already recorded,
				// so a code that has just succeeded cannot succeed again inside
				// its own window — and the refusal is the statement affecting no
				// rows rather than a comparison the caller makes.
				n, aerr := q.AcceptMFAStep(ctx, dbgen.AcceptMFAStepParams{
					UserID: userID, Step: &step,
				})
				if aerr != nil {
					return factorTOTP, fmt.Errorf("accept totp step: %w", aerr)
				}
				if n == 0 {
					return factorTOTP, ErrMFACodeInvalid
				}
				return factorTOTP, nil
			}
		case errors.Is(err, ErrMFASecretUnreadable):
			// The operator's problem, and it is logged as one. Whoever is at the
			// prompt is told only that the code is not valid — see
			// ErrMFACodeInvalid — and their route on is a recovery code.
			m.log.Error("a stored second-factor secret could not be read; "+
				"MFA_SECRET_KEY is missing or is not the one it was written under. "+
				"Enrolled accounts can still sign in with a recovery code",
				slog.String("user", userID.String()))
		default:
			return factorTOTP, err
		}
	}
	_ = lastStep // the floor is enforced by AcceptMFAStep, not read here

	// The recovery limb. Spent by the statement that matches it, scoped to the
	// account, and single-use decided in SQL for the same reason the replay guard
	// is.
	spent, err := q.SpendMFARecoveryCode(ctx, dbgen.SpendMFARecoveryCodeParams{
		UserID: userID, CodeHash: HashOpaqueToken(normalizeRecoveryCode(code)),
	})
	if err != nil {
		return factorRecoveryCode, fmt.Errorf("spend recovery code: %w", err)
	}
	if spent == 0 {
		return factorRecoveryCode, ErrMFACodeInvalid
	}
	return factorRecoveryCode, nil
}

// recordFailedFactor charges a wrong second factor to the account's lockout.
//
// Best effort and never fatal: the refusal has already been decided, and losing
// the increment is better than answering a wrong code with a server error, which
// would itself distinguish this refusal from every other one.
func (m *MFAService) recordFailedFactor(ctx context.Context, userID uuid.UUID) {
	if _, err := m.q.RecordFailedLogin(ctx, dbgen.RecordFailedLoginParams{
		ID:             userID,
		Threshold:      m.cfg.Auth.lockout.ThresholdParam(),
		LockoutSeconds: m.cfg.Auth.lockout.WindowSecondsParam(),
	}); err != nil {
		m.log.Error("could not charge a failed second factor to the lockout counter",
			slog.String("user", userID.String()), slog.Any("error", err))
	}
}

// --- the two seams ------------------------------------------------------------------

// announce writes the audit record and the notification for a change the account
// holder made themselves.
//
// Logged and not returned on failure, deliberately, and for the reason
// recovery.record gives: the change has committed by the time this runs. Losing
// the record of it is bad; losing the change because the record failed is worse.
func (m *MFAService) announce(ctx context.Context, actor *Identity, ev MFAChange) {
	if m.cfg.Audit != nil {
		if err := m.cfg.Audit.RecordMFAChange(ctx, actor, ev); err != nil {
			m.log.Error("could not record a second-factor change",
				slog.String("kind", string(ev.Kind)),
				slog.String("user", ev.UserID.String()), slog.Any("error", err))
		}
	}
	if m.cfg.Notify != nil {
		if err := m.cfg.Notify.NotifyMFAChange(ctx, ev); err != nil {
			m.log.Error("could not notify a second-factor change",
				slog.String("kind", string(ev.Kind)),
				slog.String("user", ev.UserID.String()), slog.Any("error", err))
		}
	}
}

// announceRecoveryCodeUse is announce for the one event with no Identity to hand.
//
// A recovery code is spent at the login prompt, where there is no session and
// therefore no actor — the account itself is who acted, which is the choice
// recovery.record makes for a completed password reset and for the same reason:
// nobody else was present, and attributing it to `system` would lose the one fact
// worth recording. The identity is built rather than resolved because the audit
// seam needs only the id and the label.
func (m *MFAService) announceRecoveryCodeUse(ctx context.Context, userID uuid.UUID) {
	row, err := m.q.GetUserMFA(ctx, userID)
	if err != nil {
		m.log.Error("could not read the account a recovery code was spent on",
			slog.String("user", userID.String()), slog.Any("error", err))
		return
	}
	remaining, err := m.q.CountUnusedMFARecoveryCodes(ctx, userID)
	if err != nil {
		m.log.Error("could not count what is left of an account's recovery codes",
			slog.String("user", userID.String()), slog.Any("error", err))
	}
	m.announce(ctx, &Identity{UserID: userID, Email: row.Email, Name: row.Name}, MFAChange{
		Kind: MFARecoveryCodeUsed, UserID: userID, Email: row.Email,
		RecoveryCodesRemaining: remaining,
	})
}

// --- housekeeping ---------------------------------------------------------------------

// PurgePendingLogins removes lapsed and spent rows, reporting how many went.
//
// Called by the hourly maintenance pass beside the signup and recovery sweeps, for
// the reason those exist: a waiting room with no sweep is a table that grows
// forever with nothing watching it.
func (m *MFAService) PurgePendingLogins(ctx context.Context, batch int32) (int64, error) {
	n, err := m.q.PurgeFinishedMFAPendingLogins(ctx, batch)
	if err != nil {
		return 0, fmt.Errorf("auth: purge finished pending logins: %w", err)
	}
	return n, nil
}

// issuer is what an authenticator app files the entry under.
func (m *MFAService) issuer() string {
	if m.cfg.Issuer != "" {
		return m.cfg.Issuer
	}
	return "LinkCtrl"
}
