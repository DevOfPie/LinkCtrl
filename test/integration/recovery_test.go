//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/mail"
	"github.com/DevOfPie/LinkCtrl/internal/recovery"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Account recovery (M51), the milestone that closes F141 — *there is no account
// recovery in this product at all*.
//
// Every claim m51.md makes is asserted here rather than in internal/recovery's
// own package, because all of them are about rows: a token that exists only as a
// hash, sessions that stop existing, an outbox row that erases its own body, an
// audit entry with no organization. A unit test with a fake store would be
// asserting the fake.

// ─── Fixture ─────────────────────────────────────────────────────────────────

type recoveryFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	auth   *auth.Service
	svc    *recovery.Service
	mail   *mail.Service
	sender *recordingSender
	owner  *auth.Identity
}

// newRecovery builds an instance with one account. withMailer is the whole of
// this milestone's configuration surface: it is the difference between a product
// that can recover an account and one that refuses to pretend it can.
func newRecovery(t *testing.T, withMailer bool) *recoveryFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	f := &recoveryFixture{t: t, pool: pool, auth: authSvc, owner: owner}

	cfg := recovery.Config{
		AppURL: "https://links.example.com",
		Hasher: authSvc.Hasher(),
		Audit:  audit.NewService(pool),
	}
	if withMailer {
		f.sender = &recordingSender{}
		f.mail = newMailService(t, pool, f.sender)
		// The typed-nil trap main.go's recoveryMailer exists for: assigning a
		// *mail.Service straight into the interface is only correct when it is
		// genuinely non-nil, which is the branch this is.
		cfg.Mail = f.mail
	}

	svc, err := recovery.NewService(pool, cfg)
	if err != nil {
		t.Fatalf("recovery.NewService: %v", err)
	}
	f.svc = svc
	return f
}

// requestToken asks for a reset and returns the token out of the queued mail.
//
// Reading it from the outbox rather than from a return value is deliberate:
// the service never hands the plaintext back to its caller, and a helper that
// received one would be testing an API this milestone does not have.
func (f *recoveryFixture) requestToken(address string) string {
	f.t.Helper()
	before := len(f.outbox())
	if _, err := f.svc.Request(f.t.Context(), address); err != nil {
		f.t.Fatalf("Request(%q): %v", address, err)
	}
	rows := f.outbox()
	if len(rows) != before+1 {
		f.t.Fatalf("Request queued %d mails, want 1", len(rows)-before)
	}
	last := rows[len(rows)-1]
	if last.Kind != recovery.MailKind {
		f.t.Fatalf("queued mail kind = %q, want %q — no link was sent",
			last.Kind, recovery.MailKind)
	}
	return tokenFromResetBody(f.t, last.Body)
}

// tokenFromResetBody pulls the token out of the reset URL in a mail body.
func tokenFromResetBody(t *testing.T, body string) string {
	t.Helper()
	const marker = "/reset/"
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no reset link in the mail body:\n%s", body)
	}
	return strings.Fields(body[i+len(marker):])[0]
}

func (f *recoveryFixture) outbox() []outboxRow { return outboxRows(f.t, f.pool) }

// resetRows is every password_resets row, oldest first.
func (f *recoveryFixture) resetRows() []struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  []byte
	ExpiresAt  time.Time
	ConsumedAt *time.Time
} {
	f.t.Helper()
	rs, err := f.pool.Query(f.t.Context(), `
		SELECT id, user_id, token_hash, expires_at, consumed_at
		  FROM password_resets ORDER BY created_at, id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rs.Close()

	var out []struct {
		ID         uuid.UUID
		UserID     uuid.UUID
		TokenHash  []byte
		ExpiresAt  time.Time
		ConsumedAt *time.Time
	}
	for rs.Next() {
		var r struct {
			ID         uuid.UUID
			UserID     uuid.UUID
			TokenHash  []byte
			ExpiresAt  time.Time
			ConsumedAt *time.Time
		}
		if err := rs.Scan(&r.ID, &r.UserID, &r.TokenHash, &r.ExpiresAt, &r.ConsumedAt); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *recoveryFixture) liveSessions(userID uuid.UUID) int {
	f.t.Helper()
	var n int
	err := f.pool.QueryRow(f.t.Context(), `
		SELECT count(*) FROM sessions
		 WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()`, userID).Scan(&n)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *recoveryFixture) signIn(email, password string) {
	f.t.Helper()
	if _, err := f.auth.Login(f.t.Context(), auth.LoginInput{
		Email: email, Password: password, IP: netip.MustParseAddr("198.51.100.7"),
	}); err != nil {
		f.t.Fatalf("sign in as %s: %v", email, err)
	}
}

// ─── The mechanism ───────────────────────────────────────────────────────────

// The headline: a forgotten password stops being permanent.
//
// The whole flow end to end — a request from somebody holding nothing, a link in
// a mailbox, a password that then works and one that no longer does.
func TestAForgottenPasswordCanBeRecoveredFromTheMailbox(t *testing.T) {
	f := newRecovery(t, true)
	const newPassword = "a-brand-new-sufficiently-long-password"

	token := f.requestToken("owner@example.com")

	if _, err := f.svc.Reset(t.Context(), token, newPassword); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if _, err := f.auth.Login(t.Context(), auth.LoginInput{
		Email: "owner@example.com", Password: newPassword,
	}); err != nil {
		t.Fatalf("signing in with the recovered password failed: %v", err)
	}
	_, err := f.auth.Login(t.Context(), auth.LoginInput{
		Email: "owner@example.com", Password: "a-sufficiently-long-password",
	})
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("the old password still signs in (err %v); a reset that leaves it "+
			"working has recovered nothing", err)
	}
}

// No reversible form of the token is written anywhere.
//
// The row is read back and searched for the plaintext that was just issued, in
// the token column and in every other column of the table. m51.md asks for this
// by name, and it is the one property a reader cannot verify by looking at the
// schema: `token_hash bytea` says what the column is called, not what went into
// it.
func TestNoReversibleFormOfAResetTokenIsStored(t *testing.T) {
	f := newRecovery(t, true)
	token := f.requestToken("owner@example.com")

	rows := f.resetRows()
	if len(rows) != 1 {
		t.Fatalf("password_resets has %d rows, want 1", len(rows))
	}
	if bytes.Contains(rows[0].TokenHash, []byte(token)) {
		t.Error("token_hash contains the plaintext token")
	}
	if got := auth.HashOpaqueToken(token); !bytes.Equal(rows[0].TokenHash, got) {
		t.Error("token_hash is not the SHA-256 of the issued token, so the stored " +
			"value was produced some other way")
	}

	// And nowhere else in the row. A future column that quietly carried the
	// token — a copy for a link, a debug field — would pass the check above.
	var dump string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT to_jsonb(pr)::text FROM password_resets pr`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, token) {
		t.Errorf("the password_resets row contains the plaintext token somewhere: %s", dump)
	}
}

// ─── What it refuses, each with its own test ─────────────────────────────────

// A spent token is refused, and the refusal is the same one a token that never
// existed gets.
func TestAConsumedResetTokenIsRefused(t *testing.T) {
	f := newRecovery(t, true)
	token := f.requestToken("owner@example.com")

	if _, err := f.svc.Reset(t.Context(), token, "the-first-new-password-here"); err != nil {
		t.Fatalf("first Reset: %v", err)
	}
	_, err := f.svc.Reset(t.Context(), token, "a-second-new-password-here")
	if !errors.Is(err, recovery.ErrNotResettable) {
		t.Errorf("a second use of the same token returned %v, want ErrNotResettable", err)
	}
}

// An expired token is refused, and it is refused for being expired rather than
// for being absent — the row is still there when the refusal happens.
func TestAnExpiredResetTokenIsRefused(t *testing.T) {
	f := newRecovery(t, true)
	token := f.requestToken("owner@example.com")

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE password_resets SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.Reset(t.Context(), token, "a-brand-new-sufficiently-long-password")
	if !errors.Is(err, recovery.ErrNotResettable) {
		t.Errorf("an expired token returned %v, want ErrNotResettable", err)
	}
	if rows := f.resetRows(); len(rows) != 1 || rows[0].ConsumedAt != nil {
		t.Error("the expired row was consumed or removed by the refusal; it must be " +
			"left for the purge, so a refusal cannot be told from a use by the table")
	}
}

// A token that never existed is refused with the same error as the two above.
func TestAResetTokenThatNeverExistedIsRefused(t *testing.T) {
	f := newRecovery(t, true)
	_, err := f.svc.Reset(t.Context(), "not-a-token-anybody-ever-issued",
		"a-brand-new-sufficiently-long-password")
	if !errors.Is(err, recovery.ErrNotResettable) {
		t.Errorf("an unknown token returned %v, want ErrNotResettable", err)
	}
}

// An account whose status is not `active` is not recoverable.
//
// Both ends: no link is minted for it, and a link minted before the suspension
// stops working. The second half is why the check is repeated at Reset — a link
// lives in an inbox, and the account's state can change while it sits there.
func TestASuspendedAccountIsNotRecoverable(t *testing.T) {
	f := newRecovery(t, true)

	// A link that already exists, then the suspension.
	token := f.requestToken("owner@example.com")
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE users SET status = 'suspended' WHERE id = $1`, f.owner.UserID); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.Reset(t.Context(), token, "a-brand-new-sufficiently-long-password")
	if !errors.Is(err, recovery.ErrNotResettable) {
		t.Errorf("a suspended account's outstanding link returned %v, want ErrNotResettable", err)
	}

	// And no new link is minted for it.
	before := len(f.outbox())
	if _, err := f.svc.Request(t.Context(), "owner@example.com"); err != nil {
		t.Fatalf("Request for a suspended account must answer normally: %v", err)
	}
	rows := f.outbox()
	if len(rows) != before+1 {
		t.Fatalf("Request queued %d mails, want 1", len(rows)-before)
	}
	if got := rows[len(rows)-1].Kind; got != recovery.MailKindUnavailable {
		t.Errorf("a suspended account was mailed %q, want %q — a link it cannot use "+
			"is a worse answer than the message saying none was created",
			got, recovery.MailKindUnavailable)
	}
}

// An account with a null password_hash — the SSO-only seam — is not
// recoverable.
//
// Unreachable in practice today, because nothing in this phase writes a null
// hash on a live account. It is refused rather than left to do something
// surprising when M53 or a later phase makes it reachable: writing a password
// onto an account that deliberately has none would be recovery inventing a
// second way in.
func TestAnAccountWithNoPasswordIsNotRecoverable(t *testing.T) {
	f := newRecovery(t, true)

	token := f.requestToken("owner@example.com")
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE users SET password_hash = NULL WHERE id = $1`, f.owner.UserID); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.Reset(t.Context(), token, "a-brand-new-sufficiently-long-password")
	if !errors.Is(err, recovery.ErrNotResettable) {
		t.Errorf("an account with no password returned %v, want ErrNotResettable", err)
	}

	before := len(f.outbox())
	if _, err := f.svc.Request(t.Context(), "owner@example.com"); err != nil {
		t.Fatalf("Request for a password-less account must answer normally: %v", err)
	}
	rows := f.outbox()
	if got := rows[len(rows)-1].Kind; got != recovery.MailKindUnavailable || len(rows) != before+1 {
		t.Errorf("a password-less account was mailed %q, want %q",
			got, recovery.MailKindUnavailable)
	}
}

// With no mailer, the request refuses out loud and writes nothing.
//
// This is the one place the *degrades mail-free* pattern is deliberately broken
// (D1), and the assertion has two halves for that reason: the refusal, and the
// absence of the row a silent success would have left. A reset request that
// succeeded into a void is worse than the lockout it was meant to cure.
func TestWithNoMailerARecoveryRequestRefusesRatherThanDegrading(t *testing.T) {
	f := newRecovery(t, false)

	_, err := f.svc.Request(t.Context(), "owner@example.com")
	if !errors.Is(err, recovery.ErrNoMailer) {
		t.Errorf("Request on a mail-free instance returned %v, want ErrNoMailer", err)
	}
	if rows := f.resetRows(); len(rows) != 0 {
		t.Errorf("password_resets has %d rows on a mail-free instance; the refusal "+
			"must write nothing", len(rows))
	}
	if f.svc.MailerConfigured() {
		t.Error("MailerConfigured reports true with no mailer, so both surfaces " +
			"would draw a form that cannot work")
	}

	// And the second half of the seam: an instance whose relay was removed
	// after a link went out must not still be spending it.
	_, err = f.svc.Reset(t.Context(), "any-token", "a-brand-new-sufficiently-long-password")
	if !errors.Is(err, recovery.ErrNoMailer) {
		t.Errorf("Reset on a mail-free instance returned %v, want ErrNoMailer", err)
	}
}

// ─── Enumeration ─────────────────────────────────────────────────────────────

// A request answers identically whether or not the address has an account, and
// the answer to the mailbox is what differs.
//
// The whole returned value is compared rather than a field of it, which is the
// lesson F13's own test taught: the difference that survived the first fix there
// was three fractional digits on a timestamp neither branch was thinking about.
// The cheapest way not to repeat it is a struct with nothing in it that varies,
// and an assertion on the whole struct rather than on what it is believed to
// hold.
func TestARecoveryRequestCannotBeAskedWhetherAnAddressHasAnAccount(t *testing.T) {
	f := newRecovery(t, true)

	known, err := f.svc.Request(t.Context(), "Owner@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := f.svc.Request(t.Context(), "Nobody@Example.com")
	if err != nil {
		t.Fatal(err)
	}

	if *known != (recovery.Requested{Email: "owner@example.com"}) {
		t.Errorf("the known-address answer is %+v; it must carry the normalized "+
			"address and nothing else", *known)
	}
	if *unknown != (recovery.Requested{Email: "nobody@example.com"}) {
		t.Errorf("the unknown-address answer is %+v", *unknown)
	}

	// The answers differ only where they are allowed to: in the mailbox.
	rows := f.outbox()
	if len(rows) != 2 {
		t.Fatalf("outbox has %d rows, want 2", len(rows))
	}
	if rows[0].Kind != recovery.MailKind {
		t.Errorf("the registered address was mailed %q, want %q", rows[0].Kind, recovery.MailKind)
	}
	if rows[1].Kind != recovery.MailKindUnavailable {
		t.Errorf("the unregistered address was mailed %q, want %q — it is entitled to "+
			"know somebody tried, which is the same stance signup takes (F13)",
			rows[1].Kind, recovery.MailKindUnavailable)
	}
	if rows[1].Recipient != "nobody@example.com" {
		t.Errorf("the unavailable notice went to %q", rows[1].Recipient)
	}
}

// The unregistered branch pays for an argon2 hash it does not need.
//
// m51.md's enumeration claim has two halves and this is the second: the bodies
// are identical *and* the timing is, to within the noise of an argon2 hash the
// handler performs either way. Without that, closing the oracle in the response
// leaves it open to a stopwatch — which is precisely what F92 found on the login
// path, where a locked account refused in one round trip while every other
// refusal paid a hash.
//
// **Deliberately not run at fastParams.** The rest of this suite keeps argon2 at
// the RFC floor for speed, and at the floor the hash and one INSERT are the same
// order of magnitude, so a test written against it would pass with the equalizer
// deleted. A cost this test chooses for itself makes the difference
// unmissable — around a hundred milliseconds against a few — so the assertion is
// a ratio with an enormous margin rather than a threshold somebody tuned.
func TestARecoveryRequestSpendsTheSameHashOnAnAddressWithNoAccount(t *testing.T) {
	f := newRecovery(t, true)

	costly, err := recovery.NewService(f.pool, recovery.Config{
		AppURL: "https://links.example.com",
		Hasher: auth.NewHasher(auth.Params{
			MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 1, SaltLength: 16, KeyLength: 32,
		}),
		Mail: f.mail,
	})
	if err != nil {
		t.Fatal(err)
	}

	const runs = 3
	var known, unknown time.Duration
	for range runs {
		start := time.Now()
		if _, err := costly.Request(t.Context(), "owner@example.com"); err != nil {
			t.Fatal(err)
		}
		known += time.Since(start)

		start = time.Now()
		if _, err := costly.Request(t.Context(), "nobody@example.com"); err != nil {
			t.Fatal(err)
		}
		unknown += time.Since(start)
	}

	// The unknown branch skips an INSERT and a transaction, so it is allowed to
	// be somewhat faster. What it must not be is faster by the cost of the hash,
	// which at these parameters is the overwhelming majority of both. Half is a
	// bound the equalizer clears by a wide margin and its absence misses by one.
	if unknown*2 < known {
		t.Errorf("an address with no account answers in %s against %s for one with; "+
			"the equalizing hash is not being spent, so the oracle the identical "+
			"response closed is still open to a stopwatch", unknown/runs, known/runs)
	}
}

// ─── The token in a mail body ────────────────────────────────────────────────

// The reset mail carries a live token, so it depends on 03200's scrub to erase
// itself once delivered.
//
// **Asserted here, not assumed.** m51.md says so in as many words: the mail is
// driven to `sent` and the row read back. F32 is why the constraint exists, and
// this is the first milestone whose secret depends on it — the two before it
// were an invitation and a verification link, and neither sets a password.
func TestADeliveredResetMailNoLongerCarriesItsToken(t *testing.T) {
	f := newRecovery(t, true)
	token := f.requestToken("owner@example.com")

	if err := f.mail.Drain(t.Context()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	rows := f.outbox()
	if len(rows) != 1 {
		t.Fatalf("outbox has %d rows, want 1", len(rows))
	}
	if rows[0].Status != "sent" {
		t.Fatalf("status = %q, want sent: %+v", rows[0].Status, rows[0])
	}
	if rows[0].Body != "" {
		t.Errorf("a delivered reset mail still holds its body, and the body holds a "+
			"live password-reset token: %q", rows[0].Body)
	}
	if strings.Contains(rows[0].Body, token) {
		t.Error("the delivered row still contains the token")
	}
	// The relay did receive it, so the scrub erased a message that was sent
	// rather than one that was never rendered.
	sent := f.sender.delivered()
	if len(sent) != 1 || !strings.Contains(sent[0].Body, token) {
		t.Errorf("the relay received %d messages and the token was %v in the first; "+
			"the scrub must remove the stored copy, not the delivered one",
			len(sent), len(sent) == 1 && strings.Contains(sent[0].Body, token))
	}
}

// ─── Blast radius ────────────────────────────────────────────────────────────

// A successful reset ends every session and every other outstanding token, and
// leaves API keys alone.
//
// All three in one test because the claim is the *shape* of the blast radius:
// asserting the revocations without asserting the key would let "revoke
// everything" pass, and that is the outcome D9 and D87 refuse — a key is a
// separate credential with its own rotation story, and taking them out would
// make recovery an outage.
func TestASuccessfulResetEndsEverySessionAndEveryOtherToken(t *testing.T) {
	f := newRecovery(t, true)

	f.signIn("owner@example.com", "a-sufficiently-long-password")
	f.signIn("owner@example.com", "a-sufficiently-long-password")
	if got := f.liveSessions(f.owner.UserID); got != 2 {
		t.Fatalf("the fixture has %d live sessions, want 2", got)
	}

	keys, err := auth.NewAPIKeyService(f.pool, f.auth, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatalf("key service: %v", err)
	}
	key, err := keys.Create(t.Context(), f.owner, auth.CreateAPIKeyInput{
		Name: "ci", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatalf("mint a key: %v", err)
	}

	// Two outstanding links, asked for separately. The second supersedes the
	// first at request time; what is asserted below is that using either leaves
	// none live.
	first := f.requestToken("owner@example.com")
	second := f.requestToken("owner@example.com")
	if first == second {
		t.Fatal("two requests produced the same token")
	}

	if _, err := f.svc.Reset(t.Context(), second, "a-brand-new-sufficiently-long-password"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if got := f.liveSessions(f.owner.UserID); got != 0 {
		t.Errorf("%d sessions survived the reset; a recovery that leaves the thief's "+
			"session alive has recovered nothing", got)
	}
	for _, r := range f.resetRows() {
		if r.ConsumedAt == nil {
			t.Error("an outstanding reset token survived a successful reset")
		}
	}
	if _, err := f.svc.Reset(t.Context(), first, "yet-another-long-enough-password"); !errors.Is(
		err, recovery.ErrNotResettable) {
		t.Errorf("the superseded token still resets (err %v)", err)
	}

	// And the credential that is deliberately untouched.
	if _, err := keys.Authenticate(t.Context(), key.Key); err != nil {
		t.Errorf("the account's API key stopped working after a password reset: %v; "+
			"revoking keys here would make recovery an outage (D9, D87)", err)
	}
}

// ─── The audit record ────────────────────────────────────────────────────────

// The reset is audited against the account itself, with a network prefix and no
// address, and with no organization.
func TestAResetIsAuditedAgainstTheAccountWithAPrefixOnly(t *testing.T) {
	f := newRecovery(t, true)
	token := f.requestToken("owner@example.com")

	ctx := auth.WithClientIP(t.Context(), netip.MustParseAddr("203.0.113.42"))
	if _, err := f.svc.Reset(ctx, token, "a-brand-new-sufficiently-long-password"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	var (
		action     string
		actorLabel string
		actorID    *uuid.UUID
		orgID      *uuid.UUID
		ipPrefix   *string
	)
	err := f.pool.QueryRow(t.Context(), `
		SELECT action, actor_label, actor_user_id, organization_id, ip_prefix
		  FROM audit_logs WHERE action = $1`, audit.ActionPasswordReset).
		Scan(&action, &actorLabel, &actorID, &orgID, &ipPrefix)
	if err != nil {
		t.Fatalf("no %s record was written: %v", audit.ActionPasswordReset, err)
	}

	if actorLabel != "owner@example.com" {
		t.Errorf("actor_label = %q, want the account's own address: nobody else was "+
			"present, and `system` would lose the one fact worth recording", actorLabel)
	}
	if actorID == nil || *actorID != f.owner.UserID {
		t.Errorf("actor_user_id = %v, want %v", actorID, f.owner.UserID)
	}
	if orgID != nil {
		t.Errorf("organization_id = %v; a recovery is performed with no session and "+
			"therefore in no tenant, and an account may belong to several — stamping "+
			"one is the misattribution F36 named", orgID)
	}
	if ipPrefix == nil || *ipPrefix != "203.0.113.0/24" {
		t.Errorf("ip_prefix = %v, want 203.0.113.0/24: the privacy stance is "+
			"inherited and this is not where it bends", ipPrefix)
	}
}

// ─── Validation, and the order it happens in ─────────────────────────────────

// A password below the minimum is refused the same way whether or not the token
// is real.
//
// The order is what is being asserted. Checking the token first would let
// somebody guessing tokens tell a hit from a miss by submitting a password they
// knew would be rejected — a 422 for a real token against a 404 for a bogus
// one — without spending the token to find out.
func TestAShortPasswordIsRefusedBeforeTheTokenIsLookedAt(t *testing.T) {
	f := newRecovery(t, true)
	token := f.requestToken("owner@example.com")

	for _, tc := range []struct{ name, token string }{
		{"a real token", token},
		{"a token nobody issued", "not-a-token-anybody-ever-issued"},
	} {
		_, err := f.svc.Reset(t.Context(), tc.token, "short")
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s with a short password returned %v, want a validation error", tc.name, err)
		}
	}

	// And the real token is still live, so a typo did not cost the recovery.
	if _, err := f.svc.Reset(t.Context(), token, "a-brand-new-sufficiently-long-password"); err != nil {
		t.Errorf("the token was spent by a rejected password: %v", err)
	}
}

// ─── The purge ───────────────────────────────────────────────────────────────

// Expired and long-spent rows are swept; a live one is not.
//
// The batch bound is asserted too, because it is what makes this safe to run
// inside the hourly pass: an unbounded delete holds row locks for as long as the
// backlog takes.
func TestThePurgeTakesLapsedAndLongSpentResetsAndLeavesLiveOnes(t *testing.T) {
	f := newRecovery(t, true)
	q := dbgen.New(f.pool)

	live := f.requestToken("owner@example.com")
	_ = live

	// A lapsed row, and a spent one older than the retention window.
	mustInsertReset(t, f.pool, f.owner.UserID, "lapsed",
		time.Now().Add(-time.Hour), nil)
	old := time.Now().AddDate(0, 0, -(recovery.ConsumedRetentionDays + 1))
	mustInsertReset(t, f.pool, f.owner.UserID, "long-spent",
		time.Now().Add(-time.Hour), &old)
	// And a spent row inside the window, which stays.
	recent := time.Now().Add(-time.Hour)
	mustInsertReset(t, f.pool, f.owner.UserID, "recently-spent",
		time.Now().Add(-time.Hour), &recent)

	if got := len(f.resetRows()); got != 4 {
		t.Fatalf("the fixture has %d rows, want 4", got)
	}

	n, err := f.svc.PurgeFinished(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("the purge removed %d rows, want 2 (the lapsed one and the "+
			"long-spent one)", n)
	}
	if got := len(f.resetRows()); got != 2 {
		t.Errorf("%d rows survive, want 2", got)
	}

	// The batch bound. A second call with a batch of one must take exactly one
	// row when there is more than one to take.
	mustInsertReset(t, f.pool, f.owner.UserID, "lapsed-2", time.Now().Add(-time.Hour), nil)
	mustInsertReset(t, f.pool, f.owner.UserID, "lapsed-3", time.Now().Add(-time.Hour), nil)
	got, err := q.PurgeFinishedPasswordResets(t.Context(), dbgen.PurgeFinishedPasswordResetsParams{
		KeepDays: recovery.ConsumedRetentionDays, Batch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("a batch of 1 removed %d rows; the bound is what keeps this safe "+
			"inside the hourly pass", got)
	}
}

// mustInsertReset writes a row directly, for the states a request cannot
// produce: lapsed, and spent long ago.
func mustInsertReset(
	t *testing.T, pool *pgxpool.Pool, userID uuid.UUID,
	label string, expires time.Time, consumed *time.Time,
) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO password_resets (id, user_id, token_hash, expires_at, consumed_at)
		VALUES ($1, $2, $3, $4, $5)`,
		uuid.Must(uuid.NewV7()), userID, auth.HashOpaqueToken(label), expires, consumed)
	if err != nil {
		t.Fatalf("insert %s: %v", label, err)
	}
}
