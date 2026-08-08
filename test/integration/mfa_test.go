//go:build integration

package integration

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/account"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/team"
)

// The second factor (M53).
//
// Every claim m53.md makes is asserted here rather than in internal/auth's own
// package, for the reason M51's tests give: all of them are about rows. A secret
// that appears only when a code has verified, a step counter that refuses to go
// backwards, a lockout counter shared with the password, a pending login that can
// be spent exactly once — a unit test with a fake store would be asserting the
// fake, and the state machine is the thing most worth not getting wrong.
//
// The arithmetic itself — RFC 6238's vectors, the URI shape, the cipher — is in
// internal/auth/totp_test.go, which needs no database.

// testMFAKey is the encryption key for the fixtures. Length matters: NewMFACipher
// refuses anything under MFAKeyMinBytes, so a short one here would fail every test
// for the wrong reason.
const testMFAKey = "integration-test-mfa-secret-key-48-bytes-or-more"

// ─── Fixture ─────────────────────────────────────────────────────────────────

type mfaFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	auth  *auth.Service
	svc   *auth.MFAService
	q     *dbgen.Queries
	owner *auth.Identity
	// secret is the base32 the account enrolled with, kept so a test can compute
	// the code the person's phone would be showing.
	secret string
	// codes are the recovery codes the enrolment issued, shown once and kept here
	// for the same reason.
	codes []string
}

const mfaPassword = "a-sufficiently-long-password"

// newMFA builds an instance with one account and no second factor yet.
//
// withKey is the whole of this milestone's configuration surface, exactly as
// withMailer is M51's: it is the difference between an instance that can hold a
// secret and one that has to fall back to recovery codes.
func newMFA(t *testing.T, withKey bool) *mfaFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params:  fastParams,
		TTL:     auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
		Lockout: auth.DefaultLockout,
	})
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: mfaPassword,
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	cfg := auth.MFAConfig{
		Auth:   authSvc,
		Issuer: "links.example.com",
		Audit:  audit.NewService(pool),
		Notify: notify.NewService(pool),
	}
	if withKey {
		cipher, cerr := auth.NewMFACipher(testMFAKey)
		if cerr != nil {
			t.Fatal(cerr)
		}
		cfg.Cipher = cipher
	}
	svc, err := auth.NewMFAService(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}

	return &mfaFixture{
		t: t, pool: pool, auth: authSvc, svc: svc, q: dbgen.New(pool), owner: owner,
	}
}

// enrol takes the account all the way through enrolment and records what came
// back. The ordinary starting state for everything below.
func (f *mfaFixture) enrol() {
	f.t.Helper()
	offer, err := f.svc.BeginEnrolment(f.t.Context(), f.owner)
	if err != nil {
		f.t.Fatalf("begin enrolment: %v", err)
	}
	// **The previous step's code, and that is not a trick.** EnableUserMFA stamps
	// the enrolling step as the replay floor, so the code that completed an
	// enrolment cannot also be the code that signs somebody in — the two prompts
	// are seconds apart and would otherwise share a window. A person enrolling
	// sees that as their first sign-in needing the next code rather than the one
	// still on screen; a fixture that enrolled with the *current* step would leave
	// every test below unable to sign in for thirty seconds.
	code := f.codeFor(offer.Secret, time.Now().Add(-auth.TOTPPeriod))
	out, err := f.svc.ConfirmEnrolment(f.t.Context(), f.owner, offer.Secret, code)
	if err != nil {
		f.t.Fatalf("confirm enrolment: %v", err)
	}
	f.secret = offer.Secret
	f.codes = out.RecoveryCodes
}

// codeFor is the six digits the person's authenticator would be showing.
func (f *mfaFixture) codeFor(secret string, at time.Time) string {
	f.t.Helper()
	code, err := auth.TOTPCode(secret, auth.TOTPStep(at))
	if err != nil {
		f.t.Fatalf("compute code: %v", err)
	}
	return code
}

// user reads the account row.
func (f *mfaFixture) user() dbgen.GetUserMFARow {
	f.t.Helper()
	row, err := f.q.GetUserMFA(f.t.Context(), f.owner.UserID)
	if err != nil {
		f.t.Fatalf("read account: %v", err)
	}
	return row
}

// login posts the password and returns whatever Login decided.
func (f *mfaFixture) login() *auth.LoginResult {
	f.t.Helper()
	res, err := f.auth.Login(f.t.Context(), auth.LoginInput{
		Email: "owner@example.com", Password: mfaPassword,
		IP: netip.MustParseAddr("203.0.113.42"), UserAgent: "integration",
	})
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	return res
}

func (f *mfaFixture) failedLoginCount() int32 {
	f.t.Helper()
	var n int32
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT failed_login_count FROM users WHERE id = $1`, f.owner.UserID).Scan(&n); err != nil {
		f.t.Fatalf("read lockout counter: %v", err)
	}
	return n
}

// ─── Enrolment ───────────────────────────────────────────────────────────────

// TestAnAbandonedEnrolmentLeavesTheAccountAlone.
//
// m53.md: *an enrolment that is started and abandoned leaves the account exactly
// as it was, asserted by test. Half-enrolled is not a state this product has.*
//
// The whole row is compared, not only the two MFA columns, because the claim is
// about the account rather than about the feature — a candidate secret parked
// anywhere would be the state the sentence forbids.
func TestAnAbandonedEnrolmentLeavesTheAccountAlone(t *testing.T) {
	f := newMFA(t, true)
	before := f.user()

	// Three offers, none confirmed. Reloading the page is what somebody does when
	// they cannot find their phone, and it must cost the account nothing.
	for i := 0; i < 3; i++ {
		if _, err := f.svc.BeginEnrolment(t.Context(), f.owner); err != nil {
			t.Fatalf("begin enrolment: %v", err)
		}
	}
	// And one wrong code against a real offer, which is the other way an
	// enrolment ends without completing.
	offer, err := f.svc.BeginEnrolment(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ConfirmEnrolment(t.Context(), f.owner, offer.Secret, "000000"); !errors.Is(err, auth.ErrMFACodeInvalid) {
		t.Fatalf("a wrong code at enrolment answered %v, want ErrMFACodeInvalid", err)
	}

	after := f.user()
	if after != before {
		t.Errorf("the account row changed during an abandoned enrolment.\n before: %+v\n after:  %+v\n"+
			"Half-enrolled is not a state this product has: the secret reaches the "+
			"row only in the statement that also sets mfa_enabled_at", before, after)
	}

	var codes int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1`, f.owner.UserID).
		Scan(&codes); err != nil {
		t.Fatal(err)
	}
	if codes != 0 {
		t.Errorf("%d recovery codes exist for an account that never enrolled", codes)
	}
}

// TestEnrolmentWritesTheSecretEncryptedAndIssuesTenCodes.
//
// Two claims in one row read. The secret is encrypted rather than hashed — TOTP
// needs it back — and it is unreadable in the column: `mfa_secret` holding the
// base32 verbatim would mean a database dump handed over every second factor on
// the instance.
func TestEnrolmentWritesTheSecretEncryptedAndIssuesTenCodes(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	row := f.user()
	if row.MfaEnabledAt == nil {
		t.Fatal("mfa_enabled_at is still null after a confirmed enrolment")
	}
	if row.MfaSecret == nil {
		t.Fatal("mfa_secret is still null after a confirmed enrolment")
	}
	if strings.Contains(*row.MfaSecret, f.secret) {
		t.Errorf("mfa_secret contains the base32 secret verbatim: %q. It is stored "+
			"encrypted precisely so a dump does not hand over every second factor", *row.MfaSecret)
	}
	if row.MfaLastStep == nil {
		t.Error("mfa_last_step was not stamped at enrolment, so the code just used to " +
			"enrol could also sign somebody in inside the same window")
	}

	if len(f.codes) != auth.MFARecoveryCodeCount {
		t.Errorf("enrolment issued %d recovery codes, want %d",
			len(f.codes), auth.MFARecoveryCodeCount)
	}
	// Stored as hashes and not as codes, the same HashOpaqueToken pattern the rest
	// of the product uses.
	for _, code := range f.codes {
		var n int
		if err := f.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1 AND code_hash = $2`,
			f.owner.UserID, auth.HashOpaqueToken(strings.ReplaceAll(code, "-", ""))).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("recovery code %q is not stored as its SHA-256", code)
		}
	}
	var plaintext int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1
		   AND encode(code_hash, 'escape') = ANY($2::text[])`,
		f.owner.UserID, f.codes).Scan(&plaintext); err != nil {
		t.Fatal(err)
	}
	if plaintext != 0 {
		t.Errorf("%d recovery codes are stored in a readable form", plaintext)
	}
}

// TestEnrolmentIsRefusedWithNoKey.
//
// An instance with no MFA_SECRET_KEY has nowhere safe to keep a secret, so it
// refuses rather than storing one in the clear.
func TestEnrolmentIsRefusedWithNoKey(t *testing.T) {
	f := newMFA(t, false)
	if _, err := f.svc.BeginEnrolment(t.Context(), f.owner); !errors.Is(err, auth.ErrMFAUnavailable) {
		t.Fatalf("BeginEnrolment on a keyless instance answered %v, want ErrMFAUnavailable", err)
	}
	if f.svc.Available() {
		t.Error("Available() is true with no cipher, so both surfaces would draw an offer " +
			"that cannot be honoured")
	}
}

// ─── The login flow ──────────────────────────────────────────────────────────

// TestARightPasswordMintsNoSessionForAnEnrolledAccount.
//
// m53.md: *the password is verified, and no session token exists until the second
// factor is.* Asserted against the `sessions` table rather than against the
// returned struct, because the struct is what a handler reads and the table is
// what a stolen cookie would be checked against.
func TestARightPasswordMintsNoSessionForAnEnrolledAccount(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	res := f.login()
	if !res.SecondFactorRequired() {
		t.Fatal("an enrolled account signed in on the password alone")
	}
	if res.Token != "" || res.Identity != nil {
		t.Errorf("the pending result carries a session: token=%q identity=%v. "+
			"A surface that ignores Pending must set an empty cookie, not a working one",
			res.Token, res.Identity)
	}

	var sessions int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM sessions WHERE user_id = $1`, f.owner.UserID).
		Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Errorf("%d sessions exist after a password post that stopped at the second factor", sessions)
	}
}

// TestThePendingLoginLivesForMinutes.
//
// m53.md: *its TTL is minutes and is asserted by test.* Read off the row rather
// than off the constant, so a constant changed without thinking about the row
// still fails here.
func TestThePendingLoginLivesForMinutes(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	before := time.Now()
	res := f.login()
	life := res.Pending.Expires.Sub(before)

	if life <= 0 || life > 15*time.Minute {
		t.Errorf("the pending login lives %s. Minutes is the claim: the window is a "+
			"person reading six digits off a phone already in their hand, and a "+
			"credential lying about for longer is one lying about for no reason", life)
	}
	if life < time.Minute {
		t.Errorf("the pending login lives %s, which is under a minute — short enough to "+
			"fail somebody whose phone locked while they typed their password", life)
	}

	var stored time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT expires_at FROM mfa_pending_logins WHERE user_id = $1`, f.owner.UserID).
		Scan(&stored); err != nil {
		t.Fatalf("the pending login is not in the database: %v", err)
	}
	if !stored.Equal(res.Pending.Expires) {
		t.Errorf("the returned expiry %s is not the stored one %s", res.Pending.Expires, stored)
	}
}

// TestThePendingTokenIsStoredOnlyAsItsHash.
//
// The same claim sessions, invitations, registrations and password resets each
// make. It matters as much here as for any of them: this token mints a session.
func TestThePendingTokenIsStoredOnlyAsItsHash(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()
	res := f.login()

	var hash []byte
	if err := f.pool.QueryRow(t.Context(),
		`SELECT token_hash FROM mfa_pending_logins WHERE user_id = $1`, f.owner.UserID).
		Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if string(hash) == res.Pending.Token {
		t.Fatal("the pending token is stored verbatim")
	}
	if want := auth.HashOpaqueToken(res.Pending.Token); string(hash) != string(want) {
		t.Error("the stored value is not HashOpaqueToken of the issued token")
	}
}

// TestACodeCompletesTheSignInAndMintsExactlyOneSession.
func TestACodeCompletesTheSignInAndMintsExactlyOneSession(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()
	res := f.login()

	out, err := f.svc.CompleteSecondFactor(t.Context(), res.Pending.Token,
		f.codeFor(f.secret, time.Now()), netip.MustParseAddr("203.0.113.42"), "integration")
	if err != nil {
		t.Fatalf("complete second factor: %v", err)
	}
	if out.Token == "" || out.Identity == nil {
		t.Fatal("a completed second factor produced no session")
	}

	var sessions int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM sessions WHERE user_id = $1`, f.owner.UserID).
		Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Errorf("%d sessions after one completed sign-in, want 1", sessions)
	}

	// The pending login is spent, and spending it is what makes it single use.
	var consumed *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT consumed_at FROM mfa_pending_logins WHERE user_id = $1`, f.owner.UserID).
		Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed == nil {
		t.Error("the pending login was not consumed by the sign-in it completed")
	}

	// The session records the network the sign-in *started* from, taken from the
	// pending row rather than from the second post.
	var prefix *string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT ip_prefix FROM sessions WHERE user_id = $1`, f.owner.UserID).
		Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	if prefix == nil || *prefix != "203.0.113.0/24" {
		t.Errorf("session ip_prefix = %v, want 203.0.113.0/24 — a prefix, never an address", prefix)
	}
}

// TestAPendingLoginIsSpentExactlyOnce.
//
// The single-use claim, driven the way an attacker would: replay the whole
// challenge, not just the code.
func TestAPendingLoginIsSpentExactlyOnce(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()
	res := f.login()
	token := res.Pending.Token

	if _, err := f.svc.CompleteSecondFactor(t.Context(), token,
		f.codeFor(f.secret, time.Now()), netip.MustParseAddr("203.0.113.42"), "integration"); err != nil {
		t.Fatalf("first completion: %v", err)
	}
	// A *different* code, so the refusal is the pending row's and not the replay
	// guard's — the two protect different things and a test that could not tell
	// them apart would pass with either one missing.
	next := f.codeFor(f.secret, time.Now().Add(auth.TOTPPeriod))
	_, err := f.svc.CompleteSecondFactor(t.Context(), token, next,
		netip.MustParseAddr("203.0.113.42"), "integration")
	if !errors.Is(err, auth.ErrMFAChallengeInvalid) {
		t.Fatalf("a spent pending login answered %v, want ErrMFAChallengeInvalid", err)
	}
}

// TestAFreshPasswordPostSupersedesTheOutstandingPrompt.
//
// There is never more than one live prompt per account, so an abandoned tab is not
// sharing its window with the browser in front of the person.
func TestAFreshPasswordPostSupersedesTheOutstandingPrompt(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	first := f.login()
	second := f.login()

	var live int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM mfa_pending_logins WHERE user_id = $1`, f.owner.UserID).
		Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Errorf("%d pending logins outstanding, want 1", live)
	}
	if _, err := f.svc.CompleteSecondFactor(t.Context(), first.Pending.Token,
		f.codeFor(f.secret, time.Now()), netip.MustParseAddr("203.0.113.42"), "x"); !errors.Is(err, auth.ErrMFAChallengeInvalid) {
		t.Errorf("the superseded prompt answered %v, want ErrMFAChallengeInvalid", err)
	}
	if _, err := f.svc.CompleteSecondFactor(t.Context(), second.Pending.Token,
		f.codeFor(f.secret, time.Now()), netip.MustParseAddr("203.0.113.42"), "x"); err != nil {
		t.Errorf("the newest prompt failed: %v", err)
	}
}

// TestACodeThatHasSucceededCannotSucceedAgain.
//
// m53.md: *replay is refused: a code that has just succeeded cannot succeed again
// inside its own window.* This is the sentence a shoulder-surfer, a screenshot or
// a proxy log makes worth having.
func TestACodeThatHasSucceededCannotSucceedAgain(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	now := time.Now()
	code := f.codeFor(f.secret, now)

	first := f.login()
	if _, err := f.svc.CompleteSecondFactor(t.Context(), first.Pending.Token, code,
		netip.MustParseAddr("203.0.113.42"), "x"); err != nil {
		t.Fatalf("first use of the code: %v", err)
	}

	// A fresh pending login, so the only thing standing in the way is the replay
	// guard — the same code, well inside its own thirty-second window.
	second := f.login()
	_, err := f.svc.CompleteSecondFactor(t.Context(), second.Pending.Token, code,
		netip.MustParseAddr("203.0.113.42"), "x")
	if !errors.Is(err, auth.ErrMFACodeInvalid) {
		t.Fatalf("the same code succeeded twice inside one window (answered %v). A code "+
			"seen over a shoulder is then usable for the rest of its step", err)
	}

	var step *int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT mfa_last_step FROM users WHERE id = $1`, f.owner.UserID).Scan(&step); err != nil {
		t.Fatal(err)
	}
	if step == nil || *step != auth.TOTPStep(now) {
		t.Errorf("mfa_last_step = %v, want %d — the guard is the recorded step and "+
			"nothing else", step, auth.TOTPStep(now))
	}
}

// TestAFailedSecondFactorSpendsTheSameLockoutBudgetAsAPassword.
//
// m53.md: *failed second-factor attempts count against the same lockout policy as
// failed passwords. A separate counter would give an attacker a fresh budget for
// having got the password right, which is backwards.*
//
// The interesting half is what happens to the counter at the *password* step.
// RecordSuccessfulLogin clears `failed_login_count`, so an implementation that
// called it before the second factor would reset the budget on every post and the
// lockout would never fire — which is the backwards shape stated from the other
// direction, and the reason Login guards that call.
func TestAFailedSecondFactorSpendsTheSameLockoutBudgetAsAPassword(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	for attempt := 1; attempt <= auth.DefaultLockout.Threshold; attempt++ {
		res, err := f.auth.Login(t.Context(), auth.LoginInput{
			Email: "owner@example.com", Password: mfaPassword,
			IP: netip.MustParseAddr("203.0.113.42"), UserAgent: "integration",
		})
		if err != nil {
			// The threshold has been reached and the password post itself is now
			// refused, which is the outcome this test wants.
			if errors.Is(err, auth.ErrAccountLocked) {
				return
			}
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if !res.SecondFactorRequired() {
			t.Fatalf("attempt %d signed in without a second factor", attempt)
		}
		if _, err := f.svc.CompleteSecondFactor(t.Context(), res.Pending.Token, "000000",
			netip.MustParseAddr("203.0.113.42"), "x"); !errors.Is(err, auth.ErrMFACodeInvalid) {
			t.Fatalf("attempt %d: a wrong code answered %v", attempt, err)
		}
		if got := f.failedLoginCount(); got != int32(attempt) {
			t.Fatalf("after %d wrong codes the lockout counter is %d. A right password "+
				"must not clear it: doing so hands somebody who already has the password "+
				"an unlimited supply of guesses at six digits", attempt, got)
		}
	}

	// One more password post, which must now be refused by the lockout the codes
	// filled.
	_, err := f.auth.Login(t.Context(), auth.LoginInput{
		Email: "owner@example.com", Password: mfaPassword,
		IP: netip.MustParseAddr("203.0.113.42"), UserAgent: "integration",
	})
	if !errors.Is(err, auth.ErrAccountLocked) {
		t.Fatalf("after %d wrong codes the account answered %v, want ErrAccountLocked",
			auth.DefaultLockout.Threshold, err)
	}
}

// TestASuccessfulSecondFactorClearsTheLockoutCounter.
//
// The other half of the guard above: the counter has to be cleared *somewhere*, or
// a person who mistypes a code four times and then gets it right stays four
// attempts from a lockout forever.
func TestASuccessfulSecondFactorClearsTheLockoutCounter(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	res := f.login()
	if _, err := f.svc.CompleteSecondFactor(t.Context(), res.Pending.Token, "000000",
		netip.MustParseAddr("203.0.113.42"), "x"); !errors.Is(err, auth.ErrMFACodeInvalid) {
		t.Fatal("a wrong code was accepted")
	}
	if f.failedLoginCount() == 0 {
		t.Fatal("a wrong code did not charge the counter, so this test proves nothing")
	}

	res = f.login()
	if _, err := f.svc.CompleteSecondFactor(t.Context(), res.Pending.Token,
		f.codeFor(f.secret, time.Now()), netip.MustParseAddr("203.0.113.42"), "x"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := f.failedLoginCount(); got != 0 {
		t.Errorf("the lockout counter is %d after a completed sign-in, want 0", got)
	}
}

// ─── Recovery codes ──────────────────────────────────────────────────────────

// TestARecoveryCodeSignsInOnceAndIsAuditedAndNotified.
//
// m53.md: *using one is audited and notifies the account, because a recovery code
// being spent is the signal that either the phone is gone or somebody else has
// it.*
func TestARecoveryCodeSignsInOnceAndIsAuditedAndNotified(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()
	code := f.codes[0]

	res := f.login()
	if _, err := f.svc.CompleteSecondFactor(t.Context(), res.Pending.Token, code,
		netip.MustParseAddr("203.0.113.42"), "x"); err != nil {
		t.Fatalf("a recovery code did not sign in: %v", err)
	}

	// Spent, and refused the second time.
	res = f.login()
	if _, err := f.svc.CompleteSecondFactor(t.Context(), res.Pending.Token, code,
		netip.MustParseAddr("203.0.113.42"), "x"); !errors.Is(err, auth.ErrMFACodeInvalid) {
		t.Error("a recovery code worked twice")
	}

	var remaining int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1 AND used_at IS NULL`,
		f.owner.UserID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if want := int64(auth.MFARecoveryCodeCount - 1); remaining != want {
		t.Errorf("%d unspent codes remain, want %d", remaining, want)
	}

	assertMFAAudit(t, f.pool, audit.ActionMFARecoveryCodeUsed, 1)
	assertMFANotification(t, f.pool, f.owner.UserID, "recovery_code_used")
}

// TestARecoveryCodeIsTypedHoweverThePersonWroteItDown.
//
// Case and grouping are the two things somebody transcribing from paper changes,
// and a code that only matches its canonical form is one that fails the person it
// exists for.
func TestARecoveryCodeIsTypedHoweverThePersonWroteItDown(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	for i, typed := range []string{
		strings.ToUpper(f.codes[0]),
		strings.ReplaceAll(f.codes[1], "-", ""),
		" " + f.codes[2] + " ",
	} {
		res := f.login()
		if _, err := f.svc.CompleteSecondFactor(t.Context(), res.Pending.Token, typed,
			netip.MustParseAddr("203.0.113.42"), "x"); err != nil {
			t.Errorf("code %d typed as %q was refused: %v", i, typed, err)
		}
	}
}

// TestRegeneratingVoidsThePreviousSetInFull.
//
// Spent ones included. The previous set is void, and a count of leftovers from a
// void set would be a lie.
func TestRegeneratingVoidsThePreviousSetInFull(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()
	old := f.codes

	// Spend one, so the test covers the spent rows too.
	res := f.login()
	if _, err := f.svc.CompleteSecondFactor(t.Context(), res.Pending.Token, old[0],
		netip.MustParseAddr("203.0.113.42"), "x"); err != nil {
		t.Fatal(err)
	}

	fresh, err := f.svc.RegenerateRecoveryCodes(t.Context(), f.owner)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if len(fresh) != auth.MFARecoveryCodeCount {
		t.Errorf("regeneration issued %d codes, want %d", len(fresh), auth.MFARecoveryCodeCount)
	}

	var total int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1`, f.owner.UserID).
		Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != int64(auth.MFARecoveryCodeCount) {
		t.Errorf("%d rows remain after regeneration, want %d — spent rows from the "+
			"void set were kept", total, auth.MFARecoveryCodeCount)
	}

	// An unspent code from the previous set no longer opens anything.
	res = f.login()
	if _, err := f.svc.CompleteSecondFactor(t.Context(), res.Pending.Token, old[1],
		netip.MustParseAddr("203.0.113.42"), "x"); !errors.Is(err, auth.ErrMFACodeInvalid) {
		t.Error("a code from the previous set still signs in")
	}
	assertMFAAudit(t, f.pool, audit.ActionMFARecoveryCodesRegenerated, 1)
}

// TestARecoveryCodeWorksWithNoEncryptionKey.
//
// The bound m53.md puts on losing MFA_SECRET_KEY: *every enrolled account loses
// the second factor and no further.* Recovery codes are SHA-256 and the key is not
// involved in them, so the documented chain — recovery code, then disable, then
// enrol again — is a chain that exists.
func TestARecoveryCodeWorksWithNoEncryptionKey(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()
	codes := f.codes

	// The same database, served by a service whose key has gone.
	keyless, err := auth.NewMFAService(f.pool, auth.MFAConfig{
		Auth: f.auth, Audit: audit.NewService(f.pool), Notify: notify.NewService(f.pool),
	})
	if err != nil {
		t.Fatal(err)
	}

	res := f.login()
	if !res.SecondFactorRequired() {
		t.Fatal("an enrolled account signed in on the password alone after the key went. " +
			"Losing the key must lock the second factor, never silently drop it")
	}
	// The authenticator code cannot be verified, because the secret cannot be read.
	if _, err := keyless.CompleteSecondFactor(t.Context(), res.Pending.Token,
		f.codeFor(f.secret, time.Now()), netip.MustParseAddr("203.0.113.42"), "x"); !errors.Is(err, auth.ErrMFACodeInvalid) {
		t.Errorf("a TOTP code verified with no key: %v", err)
	}
	// The recovery code does.
	res = f.login()
	if _, err := keyless.CompleteSecondFactor(t.Context(), res.Pending.Token, codes[0],
		netip.MustParseAddr("203.0.113.42"), "x"); err != nil {
		t.Fatalf("a recovery code was refused on a keyless instance: %v. That is the "+
			"documented route back, and without it losing the key destroys accounts", err)
	}
}

// ─── Disabling ───────────────────────────────────────────────────────────────

// TestDisablingNeedsThePasswordAndACode.
//
// Either alone is a downgrade somebody else can perform: a stolen session would
// otherwise remove the factor it was meant to be stopped by.
func TestDisablingNeedsThePasswordAndACode(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	if err := f.svc.Disable(t.Context(), f.owner, "wrong-password",
		f.codeFor(f.secret, time.Now())); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("a wrong password answered %v, want ErrInvalidCredentials", err)
	}
	if err := f.svc.Disable(t.Context(), f.owner, mfaPassword, "000000"); !errors.Is(err, auth.ErrMFACodeInvalid) {
		t.Errorf("a wrong code answered %v, want ErrMFACodeInvalid", err)
	}
	if f.user().MfaEnabledAt == nil {
		t.Fatal("the second factor was removed by a refused attempt")
	}

	if err := f.svc.Disable(t.Context(), f.owner, mfaPassword,
		f.codeFor(f.secret, time.Now())); err != nil {
		t.Fatalf("disable: %v", err)
	}
}

// TestDisablingClearsTheSecretAndEveryCode.
//
// m53.md: *clearing `mfa_enabled_at` clears the secret and every unused recovery
// code in the same transaction.*
func TestDisablingClearsTheSecretAndEveryCode(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()
	f.login() // leaves a pending row outstanding

	if err := f.svc.Disable(t.Context(), f.owner, mfaPassword, f.codes[0]); err != nil {
		t.Fatalf("disable with a recovery code: %v", err)
	}

	row := f.user()
	if row.MfaEnabledAt != nil || row.MfaSecret != nil || row.MfaLastStep != nil {
		t.Errorf("the account row still carries second-factor state: %+v", row)
	}
	for _, table := range []string{"mfa_recovery_codes", "mfa_pending_logins"} {
		var n int
		if err := f.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM `+table+` WHERE user_id = $1`, f.owner.UserID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d rows remain in %s after disabling", n, table)
		}
	}
	assertMFAAudit(t, f.pool, audit.ActionMFADisabled, 1)

	// And the account signs in on the password alone again.
	res := f.login()
	if res.SecondFactorRequired() {
		t.Error("the account still stops at the second factor after it was removed")
	}
}

// TestDisablingIsRefusedToAnAPIKey.
//
// m53.md: *API-key authentication is unaffected. A key is not a person and has no
// second factor; D87 already says the session is the authority for operations
// whose subject is the person. Disabling MFA is such an operation, so
// `requireSessionActor` guards it. Asserted by test.*
func TestDisablingIsRefusedToAnAPIKey(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	keyID := uuid.Must(uuid.NewV7())
	asKey := *f.owner
	asKey.APIKeyID = &keyID

	code := f.codeFor(f.secret, time.Now())
	if err := f.svc.Disable(t.Context(), &asKey, mfaPassword, code); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an API key disabling a second factor answered %v, want a refusal", err)
	}
	if _, err := f.svc.BeginEnrolment(t.Context(), &asKey); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an API key beginning an enrolment answered %v, want a refusal", err)
	}
	if _, err := f.svc.RegenerateRecoveryCodes(t.Context(), &asKey); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an API key regenerating recovery codes answered %v, want a refusal", err)
	}
	if f.user().MfaEnabledAt == nil {
		t.Fatal("the second factor was removed by a key")
	}
}

// ─── How M51 and M53 interact ────────────────────────────────────────────────

// TestAPasswordResetDoesNotBypassTheSecondFactor.
//
// m53.md, decided here rather than discovered: *proving control of a mailbox is
// one factor; letting it stand in for the other makes MFA worth exactly the
// mailbox. A reset therefore lands the account at the code prompt.*
//
// The reset itself is M51's and is not re-tested; what this asserts is the
// interaction, by writing a new password the way recovery does and then signing in
// with it.
func TestAPasswordResetDoesNotBypassTheSecondFactor(t *testing.T) {
	f := newMFA(t, true)
	f.enrol()

	const fresh = "an-entirely-new-password-2026"
	if err := auth.WritePassword(t.Context(), f.q, f.auth.Hasher(), f.owner.UserID, fresh); err != nil {
		t.Fatalf("write the recovered password: %v", err)
	}

	res, err := f.auth.Login(t.Context(), auth.LoginInput{
		Email: "owner@example.com", Password: fresh,
		IP: netip.MustParseAddr("203.0.113.42"), UserAgent: "integration",
	})
	if err != nil {
		t.Fatalf("sign in with the recovered password: %v", err)
	}
	if !res.SecondFactorRequired() {
		t.Fatal("a recovered password signed straight in. A mailbox would then be worth " +
			"exactly as much as the second factor, which is the whole thing MFA is for")
	}
	if f.user().MfaEnabledAt == nil {
		t.Error("the reset removed the second factor")
	}
}

// TestDeletingAnAccountTakesItsSecondFactorWithIt.
//
// M52's deletion is a soft delete and fires no foreign key, so the two tables this
// milestone adds have to be in `DeleteAccountDependents` or a recovery code
// outlives the account it opens — the `password_resets` defect in a new table,
// found and closed in the same phase.
func TestDeletingAnAccountTakesItsSecondFactorWithIt(t *testing.T) {
	f := newMFA(t, true)

	// A second account, because deletion is refused to an organization's only
	// owner (M28.5) and the instance refuses to lose its last organization. The
	// owner from the fixture keeps the instance viable; this one enrols, signs in
	// as far as the prompt, hands nothing over and leaves. The demo seeder goes
	// through the same door for the same reason.
	departing, err := f.auth.Register(t.Context(), auth.RegisterInput{
		Email: "departing@example.com", Name: "Departing", Password: mfaPassword,
	})
	if err != nil {
		t.Fatalf("register the departing account: %v", err)
	}

	offer, err := f.svc.BeginEnrolment(t.Context(), departing)
	if err != nil {
		t.Fatalf("begin enrolment: %v", err)
	}
	if _, err := f.svc.ConfirmEnrolment(t.Context(), departing, offer.Secret,
		f.codeFor(offer.Secret, time.Now())); err != nil {
		t.Fatalf("confirm enrolment: %v", err)
	}
	// A password post, which leaves a pending login outstanding — the second of
	// the two tables this asserts.
	if _, err := f.auth.Login(t.Context(), auth.LoginInput{
		Email: "departing@example.com", Password: mfaPassword,
		IP: netip.MustParseAddr("203.0.113.42"), UserAgent: "integration",
	}); err != nil {
		t.Fatalf("password post: %v", err)
	}

	teamSvc := team.NewService(f.pool, team.Config{Audit: audit.NewService(f.pool)})
	if err := teamSvc.DeleteOrganization(t.Context(), departing, departing.OrgID); err != nil {
		t.Fatalf("delete the personal organization: %v", err)
	}
	accounts, err := account.NewService(f.pool, account.Config{
		Auth: f.auth, Audit: audit.NewService(f.pool),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.Delete(t.Context(), departing, mfaPassword); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	for _, table := range []string{"mfa_recovery_codes", "mfa_pending_logins"} {
		var n int
		if err := f.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM `+table+` WHERE user_id = $1`, departing.UserID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d rows remain in %s after the account was deleted. A soft delete "+
				"fires no cascade, so this table has to be named in "+
				"DeleteAccountDependents", n, table)
		}
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func assertMFAAudit(t *testing.T, pool *pgxpool.Pool, action string, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Errorf("%d audit records for %s, want %d", n, action, want)
	}
	// Instance-wide, on the reasoning password.reset and account.deleted already
	// established: a second factor is a property of a person, not of a tenant.
	var scoped int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = $1 AND organization_id IS NOT NULL`,
		action).Scan(&scoped); err != nil {
		t.Fatal(err)
	}
	if scoped != 0 {
		t.Errorf("%d %s records are filed under an organization. An account may belong "+
			"to several or to none, and filing it under one is F36's misattribution",
			scoped, action)
	}
	// And nothing in the metadata is a secret.
	rows, err := pool.Query(context.Background(),
		`SELECT metadata::text FROM audit_logs WHERE action = $1`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var meta string
		if err := rows.Scan(&meta); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"secret", "code_hash", "token"} {
			if strings.Contains(meta, forbidden) {
				t.Errorf("the %s metadata names %q: %s", action, forbidden, meta)
			}
		}
	}
}

func assertMFANotification(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, change string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications
		  WHERE user_id = $1 AND kind = $2 AND data->>'change' = $3`,
		userID, notify.KindMFAChanged, change).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d %s notifications with change=%s, want 1", n, notify.KindMFAChanged, change)
	}
}
