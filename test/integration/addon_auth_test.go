//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// This file is m65.md's last bullet: **sabotage-verified end to end with a test
// module asserting a fake identity** — wrong subject, unlinked subject, locked
// account, and an add-on that does not hold `session.mint`, each refused at the
// host boundary. The last of those read "disabled add-on" until D300 amended it:
// this product has no mechanism that disables an add-on, so withholding the grant
// is the refusal the phrase can name and is the one driven below.
//
// It lives here rather than in internal/addon because every refusal in it is a
// statement about a *row*: an account that exists, one that is locked, one that is
// not active, a link that is there or is not. None of that can be asserted against
// a stub without the test asserting its own opinion about who may sign in — which
// is precisely the failure mode this milestone cannot afford, since a bug here is
// an account takeover rather than an outage.
//
// The assertions are made from the module's side wherever a module can make them.
// internal/addon/testdata/modules/identity is a real consumer of the published
// SDK, compiled the way an add-on is compiled, and it prints the whole record it
// was handed — so "no credential crossed" is checked against what a guest actually
// received rather than against a Go struct on this side.

const testIssuer = "https://idp.test"

// authFixture is the add-on host wired to a real session service, over a real
// database, with the M65 fixture installed.
type authFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	host  *addon.Host
	auth  *auth.Service
	audit *audit.Service
	owner *auth.Identity
	log   *logSink
}

func newAddonAuth(t *testing.T, tweak func(*addon.Manifest), overrides map[string]string) *authFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params:  fastParams,
		TTL:     auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
		Lockout: auth.LockoutPolicy{Threshold: 2, Window: 15 * time.Minute},
	})
	auditSvc := audit.NewService(pool)
	authSvc.SetSessionAuditor(auditSvc)

	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	root := t.TempDir()
	code := addonFixture(t, "identity")
	m := installAddon(t, root, "identity", code,
		[]string{"routes.own_prefix", addon.PermissionSessionMint}, nil)
	if tweak != nil {
		tweak(&m)
		writeManifest(t, filepath.Join(root, "identity"), m)
	}

	sink := &logSink{}
	host, err := addon.Open(t.Context(), addon.Options{
		Dir:       root,
		Logger:    slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Sessions:  authSvc,
		Overrides: func(string) map[string]string { return overrides },
	})
	if err != nil {
		t.Fatalf("the identity add-on did not load: %v\n%s", err, sink.String())
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	return &authFixture{t: t, pool: pool, host: host, auth: authSvc,
		audit: auditSvc, owner: owner, log: sink}
}

// callback drives the add-on's callback with one asserted subject.
func (f *authFixture) callback(path, subject string, as *auth.Identity) addon.Response {
	f.t.Helper()
	resp, err := f.host.Route(f.t.Context(), "identity", addon.RequestIn{
		Method: http.MethodGet, Path: path, Query: subject,
		ClientIP:  netip.MustParseAddr("198.51.100.9"),
		UserAgent: "integration/1.0",
		Identity:  as,
	})
	if err != nil {
		f.t.Fatalf("%s: %v\n%s", path, err, f.log.String())
	}
	return resp
}

// link connects a subject to an account, through the flow that is the only writer.
func (f *authFixture) link(subject string, as *auth.Identity) addon.Response {
	f.t.Helper()
	return f.callback("/connect", subject, as)
}

func (f *authFixture) sessions(userID uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM sessions WHERE user_id = $1`, userID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// The whole path, in the order somebody actually walks it: connect a provider
// while signed in, then come back with an assertion and be signed in for it.
func TestAnAssertionForALinkedSubjectMintsASession(t *testing.T) {
	f := newAddonAuth(t, nil, nil)

	if resp := f.link("subject-linked", f.owner); !strings.Contains(resp.Body, "link: <nil>") {
		t.Fatalf("the provider was not connected: %q", resp.Body)
	}

	resp := f.callback("/callback", "subject-linked", nil)

	if resp.Minted == nil {
		t.Fatalf("no session was minted: %q\n%s", resp.Body, f.log.String())
	}
	if resp.Minted.SecondFactorRequired {
		t.Error("an account with no second factor was told it owed one")
	}
	// The token resolves to the *linked* account, which is what makes "the host
	// decides who this is" checkable — and the fixture asserted somebody else's
	// address on purpose, so a host matching on email would have signed in nobody
	// or the wrong person.
	id, err := f.auth.Authenticate(t.Context(), resp.Minted.Token.Reveal())
	if err != nil {
		t.Fatalf("the minted token does not resolve to a session: %v", err)
	}
	if id.UserID != f.owner.UserID {
		t.Errorf("the session belongs to %s and the link names %s", id.UserID, f.owner.UserID)
	}
	if id.Email != "owner@example.com" {
		t.Errorf("the session's address is %q; the assertion carried "+
			"someone-else@elsewhere.test and it must not have been believed", id.Email)
	}
	// The link records that it was used, which is the question an operator asks
	// about a credential they are considering removing.
	var lastUsed *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT last_used_at FROM addon_identity_links WHERE subject = $1`,
		"subject-linked").Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if lastUsed == nil {
		t.Error("the link was used to mint a session and its last_used_at did not move")
	}
}

// The four refusals m65.md names, each against a real row, and each leaving no
// session behind. Table-driven because the property worth asserting is the same
// for all of them and stating it once is what stops a fifth being added without it.
func TestTheHostRefusesEveryAssertionItShould(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject string
		// prepare puts the account into the state under test and returns the
		// subject the callback should assert.
		prepare func(t *testing.T, f *authFixture) string
		// wants is the substring the module reported, which is the status it
		// branched on rather than the host's own prose.
		wants string
	}{
		{
			name: "a subject nobody has connected",
			prepare: func(_ *testing.T, _ *authFixture) string {
				// Nothing is linked at all. This is every first visit, and it is why
				// the answer is ErrNotFound rather than a failure.
				return "subject-unknown"
			},
			wants: "refused ErrNotFound",
		},
		{
			name: "a subject that is not the one linked",
			prepare: func(_ *testing.T, f *authFixture) string {
				f.link("subject-linked", f.owner)
				// One character different. The lookup is exact and there is no
				// fallback to any other column, so a near miss is a miss.
				return "subject-linke"
			},
			wants: "refused ErrNotFound",
		},
		{
			name: "an account that is locked out",
			prepare: func(t *testing.T, f *authFixture) string {
				f.link("subject-locked", f.owner)
				if _, err := f.pool.Exec(t.Context(),
					`UPDATE users SET locked_until = now() + interval '1 hour' WHERE id = $1`,
					f.owner.UserID); err != nil {
					t.Fatal(err)
				}
				return "subject-locked"
			},
			wants: "refused ErrDenied",
		},
		{
			name: "an account that is not active",
			prepare: func(t *testing.T, f *authFixture) string {
				f.link("subject-inactive", f.owner)
				if _, err := f.pool.Exec(t.Context(),
					`UPDATE users SET status = 'suspended' WHERE id = $1`,
					f.owner.UserID); err != nil {
					t.Fatal(err)
				}
				return "subject-inactive"
			},
			wants: "refused ErrDenied",
		},
		{
			name: "a browser that is already signed in",
			prepare: func(_ *testing.T, f *authFixture) string {
				f.link("subject-signedin", f.owner)
				return "subject-signedin"
			},
			wants: "refused ErrDenied",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAddonAuth(t, nil, nil)
			subject := tc.prepare(t, f)

			var as *auth.Identity
			if tc.name == "a browser that is already signed in" {
				as = f.owner
			}
			before := f.sessions(f.owner.UserID)
			resp := f.callback("/callback", subject, as)

			if !strings.Contains(resp.Body, tc.wants) {
				t.Errorf("the module reported %q, want %q", resp.Body, tc.wants)
			}
			if resp.Minted != nil {
				t.Errorf("a refused assertion still produced %+v", resp.Minted)
			}
			if after := f.sessions(f.owner.UserID); after != before {
				t.Errorf("a refused assertion changed the session count from %d to %d", before, after)
			}
		})
	}
}

// The disabled add-on, which is the fifth of the milestone's refusals and is a
// different kind: the module is installed and running, and the *grant* is what it
// does not have.
func TestAnAddonThatDidNotDeclareTheGrantMintsNothing(t *testing.T) {
	f := newAddonAuth(t, func(m *addon.Manifest) {
		m.Permissions = []string{"routes.own_prefix"}
	}, nil)

	// Nothing is linked, because nothing could be: the linking half costs the same
	// grant, which is what stops an add-on bootstrapping itself into being believed.
	resp := f.link("subject-x", f.owner)
	if !strings.Contains(resp.Body, "link: ErrDenied") {
		t.Errorf("an add-on without session.mint connected a provider: %q", resp.Body)
	}

	resp = f.callback("/callback", "subject-x", nil)
	if !strings.Contains(resp.Body, "refused ErrDenied") {
		t.Errorf("an add-on without session.mint reached the hook: %q", resp.Body)
	}
	if resp.Minted != nil {
		t.Error("an add-on without session.mint minted a session")
	}
	// The refusal is a grant refusal and not a lookup that failed, which is the
	// difference the counter an operator alerts on is keyed by.
	if !strings.Contains(f.log.String(), "it did not declare the permission the function needs") {
		t.Error("the refusal was not recorded as a permission refusal")
	}
}

// **MFA composes; it is not bypassed.** An account with a second factor enrolled
// meets it after an assertion rather than instead of it, and the default is the
// safe reading.
func TestAnAssertionDoesNotBypassASecondFactor(t *testing.T) {
	f := newAddonAuth(t, nil, nil)
	f.link("subject-mfa", f.owner)
	enrolSecondFactor(t, f.pool, f.owner.UserID)

	resp := f.callback("/callback", "subject-mfa", nil)

	if resp.Minted == nil {
		t.Fatalf("the assertion produced nothing at all: %q", resp.Body)
	}
	if !resp.Minted.SecondFactorRequired {
		t.Fatal("an account with TOTP enrolled was signed in on an add-on's word alone")
	}
	if resp.Minted.Token.Reveal() != "" {
		t.Error("a session token exists for somebody who still owes a second factor")
	}
	if f.sessions(f.owner.UserID) != 0 {
		t.Error("a session row was created before the second factor was met")
	}
	// A pending login is what was created instead, which is the same row the
	// password path creates at the same point.
	var pending int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM mfa_pending_logins WHERE user_id = $1 AND consumed_at IS NULL`,
		f.owner.UserID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("%d pending logins, want 1", pending)
	}
	if !strings.Contains(resp.Body, "second_factor_required=true") {
		t.Errorf("the module was not told a factor is owed: %q", resp.Body)
	}
}

// And the operator's escape hatch, which is a deliberate act with a documented
// consequence rather than a default.
func TestAnOperatorCanSayAProviderMetTheSecondFactor(t *testing.T) {
	f := newAddonAuth(t, nil, map[string]string{"mfa_satisfied": "true"})
	f.link("subject-mfa", f.owner)
	enrolSecondFactor(t, f.pool, f.owner.UserID)

	resp := f.callback("/callback", "subject-mfa", nil)

	if resp.Minted == nil || resp.Minted.SecondFactorRequired {
		t.Fatalf("the operator's answer was not honoured: %+v", resp.Minted)
	}
	if resp.Minted.Token.Reveal() == "" {
		t.Fatal("no session was minted")
	}
	if f.sessions(f.owner.UserID) != 1 {
		t.Errorf("%d sessions, want 1", f.sessions(f.owner.UserID))
	}
}

// The provenance record, which is the third limb of the milestone's first bullet.
//
// It carries which add-on and which issuer vouched, and **nothing about the
// external identity** — no subject, no asserted address, no display name. That
// absence is deliberate: M52's erasure sweep scrubs this column by the keys it
// knows, its coverage was counted site by site when F177 closed, and a person's
// provider identifier in a jsonb key nothing sweeps would be that count going
// wrong again.
func TestAMintedSessionRecordsWhichAddonVouchedForIt(t *testing.T) {
	f := newAddonAuth(t, nil, nil)
	f.link("subject-audited", f.owner)
	f.callback("/callback", "subject-audited", nil)

	var (
		action     string
		actorID    *uuid.UUID
		actorLabel string
		raw        []byte
	)
	if err := f.pool.QueryRow(t.Context(), `
		SELECT action, actor_user_id, actor_label, metadata
		  FROM audit_logs WHERE action = $1`, audit.ActionSessionMintedByAddon).
		Scan(&action, &actorID, &actorLabel, &raw); err != nil {
		t.Fatalf("no provenance record was written: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["minted_by"] != auth.MintedByLabel("identity") {
		t.Errorf("minted_by is %v, want %q", meta["minted_by"], auth.MintedByLabel("identity"))
	}
	if meta["issuer"] != testIssuer {
		t.Errorf("issuer is %v, want %q", meta["issuer"], testIssuer)
	}
	if actorID == nil || *actorID != f.owner.UserID {
		t.Errorf("the record names actor %v, want %s", actorID, f.owner.UserID)
	}
	// The absences, named one at a time because each is a value somebody could
	// reasonably have thought belonged here.
	for _, key := range []string{"subject", "email", "display_name", "groups"} {
		if _, present := meta[key]; present {
			t.Errorf("the provenance record carries %q, which the erasure sweep does not read", key)
		}
	}
	for _, value := range []string{"subject-audited", "someone-else@elsewhere.test", "Asserted Name"} {
		if strings.Contains(string(raw), value) {
			t.Errorf("the external identity reached the audit record: %s", raw)
		}
	}
}

// TestTheSessionASecondFactorCompletesStillNamesTheAddon.
//
// m65.md's first bullet asks the audit writer to record **the session's**
// provenance. For an account with no second factor that is the mint itself. For an
// account with TOTP enrolled the assertion produces no session at all — it
// produces a pending login, and `CompleteSecondFactor` mints the session minutes
// later on a path that has never heard of an add-on. So the account the provenance
// is *most* worth having for was the one account for which nothing named the
// minter, and this is the test that says otherwise.
//
// Two records under one action, told apart by `second_factor_required`: the
// assertion that stopped at the prompt, and the session that exists.
func TestTheSessionASecondFactorCompletesStillNamesTheAddon(t *testing.T) {
	f := newAddonAuth(t, nil, nil)
	f.link("subject-mfa-audited", f.owner)
	mfaSvc, secret := enrolThroughTheService(t, f)

	resp := f.callback("/callback", "subject-mfa-audited", nil)

	if resp.Minted == nil || !resp.Minted.SecondFactorRequired {
		t.Fatalf("the assertion did not stop at the prompt: %+v", resp.Minted)
	}
	pending := resp.Minted.PendingToken.Reveal()
	if pending == "" {
		t.Fatal("no challenge came back, so there is nothing to complete")
	}
	// Before the factor is met: one record, and it is about the prompt.
	if n := f.mintRecords(false); n != 0 {
		t.Errorf("%d records claim a session before one exists", n)
	}

	at := time.Now()
	code, err := auth.TOTPCode(secret, auth.TOTPStep(at))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mfaSvc.CompleteSecondFactor(t.Context(), pending, code,
		netip.MustParseAddr("198.51.100.9"), "integration/1.0"); err != nil {
		t.Fatalf("complete the second factor: %v", err)
	}

	if f.sessions(f.owner.UserID) != 1 {
		t.Fatalf("%d sessions after the factor was met, want 1", f.sessions(f.owner.UserID))
	}
	if n := f.mintRecords(false); n != 1 {
		t.Fatalf("%d provenance records for the session that exists, want 1. A TOTP "+
			"account would otherwise have a session nothing names an add-on as the "+
			"minter of", n)
	}
	if n := f.mintRecords(true); n != 1 {
		t.Errorf("%d records for the prompt, want 1; the two events are one action "+
			"told apart by second_factor_required and both are worth a record", n)
	}

	// And the record about the session says the same things the record about the
	// prompt said, because both go through one writer.
	var raw []byte
	if err := f.pool.QueryRow(t.Context(), `
		SELECT metadata FROM audit_logs
		 WHERE action = $1 AND metadata->>'second_factor_required' = 'false'`,
		audit.ActionSessionMintedByAddon).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["minted_by"] != auth.MintedByLabel("identity") {
		t.Errorf("minted_by is %v, want %q", meta["minted_by"], auth.MintedByLabel("identity"))
	}
	if meta["issuer"] != testIssuer {
		t.Errorf("issuer is %v, want %q", meta["issuer"], testIssuer)
	}
	// The same absences the other provenance record is held to. The pending row
	// carries two columns and neither is a person.
	for _, value := range []string{"subject-mfa-audited", "someone-else@elsewhere.test"} {
		if strings.Contains(string(raw), value) {
			t.Errorf("the external identity reached the audit record: %s", raw)
		}
	}
}

// The password form's own prompt acquires no add-on, which is the other half of
// the claim above: a nullable column is only honest if the ordinary case leaves it
// null.
func TestAPasswordPromptIsNotAttributedToAnAddon(t *testing.T) {
	f := newAddonAuth(t, nil, nil)
	mfaSvc, secret := enrolThroughTheService(t, f)

	if _, err := f.auth.Login(t.Context(), auth.LoginInput{
		Email: "owner@example.com", Password: "a-sufficiently-long-password",
		IP: netip.MustParseAddr("198.51.100.9"), UserAgent: "integration/1.0",
	}); err != nil {
		t.Fatalf("password post: %v", err)
	}
	var addonName *string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT minted_by_addon FROM mfa_pending_logins WHERE user_id = $1`,
		f.owner.UserID).Scan(&addonName); err != nil {
		t.Fatal(err)
	}
	if addonName != nil {
		t.Fatalf("a password post was attributed to %q", *addonName)
	}

	// And completing it writes no provenance record, because nothing vouched.
	var token string
	res, err := f.auth.Login(t.Context(), auth.LoginInput{
		Email: "owner@example.com", Password: "a-sufficiently-long-password",
		IP: netip.MustParseAddr("198.51.100.9"), UserAgent: "integration/1.0",
	})
	if err != nil {
		t.Fatalf("password post: %v", err)
	}
	token = res.Pending.Token
	code, err := auth.TOTPCode(secret, auth.TOTPStep(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mfaSvc.CompleteSecondFactor(t.Context(), token, code,
		netip.MustParseAddr("198.51.100.9"), "integration/1.0"); err != nil {
		t.Fatalf("complete the second factor: %v", err)
	}
	if n := f.mintRecords(false) + f.mintRecords(true); n != 0 {
		t.Errorf("%d add-on provenance records for a sign-in no add-on touched", n)
	}
}

// mintRecords counts provenance records of one kind.
func (f *authFixture) mintRecords(pending bool) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(), `
		SELECT count(*) FROM audit_logs
		 WHERE action = $1 AND metadata->>'second_factor_required' = $2`,
		audit.ActionSessionMintedByAddon, strconv.FormatBool(pending)).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// enrolThroughTheService enrols the fixture's account for real and hands back the
// service and the secret.
//
// Not enrolSecondFactor, which writes a secret nothing can decrypt: that is enough
// for a test that only needs the account to *have* a factor, and not enough for
// one that has to meet it.
func enrolThroughTheService(t *testing.T, f *authFixture) (*auth.MFAService, string) {
	t.Helper()
	cipher, err := auth.NewMFACipher(testMFAKey)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewMFAService(f.pool, auth.MFAConfig{
		Auth: f.auth, Issuer: "links.example.com", Cipher: cipher,
		Audit: f.audit, Notify: notify.NewService(f.pool),
	})
	if err != nil {
		t.Fatal(err)
	}
	offer, err := svc.BeginEnrolment(t.Context(), f.owner)
	if err != nil {
		t.Fatalf("begin enrolment: %v", err)
	}
	// The **previous** step's code, for the reason the MFA fixture's own enrol()
	// gives: confirming stamps the enrolling step as the replay floor, so enrolling
	// on the current step would leave every sign-in below unable to complete for
	// thirty seconds.
	code, err := auth.TOTPCode(offer.Secret, auth.TOTPStep(time.Now().Add(-auth.TOTPPeriod)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmEnrolment(t.Context(), f.owner, offer.Secret, code); err != nil {
		t.Fatalf("confirm enrolment: %v", err)
	}
	return svc, offer.Secret
}

// A link is a standing credential that admits somebody to an account with no
// password, so it goes with the account. This is the `password_resets` rule M53
// applied to its own two tables, applied here in the milestone that creates the
// table rather than deferred.
//
// Driven through **the statement the deletion path actually runs**, not through a
// DELETE this test writes: what is being asserted is that
// `DeleteAccountDependents` reaches this table, and a hand-written DELETE would
// assert that this test can write SQL.
func TestDeletingAnAccountTakesItsConnectedIdentities(t *testing.T) {
	f := newAddonAuth(t, nil, nil)
	f.link("subject-doomed", f.owner)

	removed, err := dbgen.New(f.pool).DeleteAccountDependents(t.Context(), f.owner.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if removed.AddonIdentityLinks != 1 {
		t.Errorf("the deletion statement removed %d connected identities, want 1; a link "+
			"left behind a deleted account is that account still signing in",
			removed.AddonIdentityLinks)
	}
	var left int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM addon_identity_links WHERE user_id = $1`,
		f.owner.UserID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d links survived the account", left)
	}
}

// --- the linking table's own three guarantees -------------------------------
//
// Each is published — in `addonauth.go`'s errors, in its comments, and in
// `docs/data-model.md`'s row — and none had a test. `ErrSubjectLinkedElsewhere`
// had **zero** references outside the file that declares it and the status mapping
// that translates it, which for the takeover this table exists to prevent is the
// gap worth closing first.

// TestOneExternalIdentityCannotBeConnectedToASecondAccount.
//
// The takeover in its simplest form: somebody's provider subject is already
// connected here, and a second account offers the same one. If that re-pointed the
// row, the second account would then be signed in by the first person's provider —
// or, worse, the first person's assertion would sign in the attacker's account and
// carry their sessions.
//
// The refusal is a *read* rather than a re-point that finds out afterwards, and
// what the module learns is `ErrDenied` with no mention of the other account: it
// is a dead end the person resolves on the account that holds it, and naming that
// account would make this endpoint an oracle for which addresses have connected
// which providers.
func TestOneExternalIdentityCannotBeConnectedToASecondAccount(t *testing.T) {
	f := newAddonAuth(t, nil, nil)
	stranger := f.register("stranger@example.com")

	if resp := f.link("subject-contested", f.owner); !strings.Contains(resp.Body, "link: <nil>") {
		t.Fatalf("the first connection did not succeed: %q", resp.Body)
	}

	resp := f.link("subject-contested", stranger)

	if !strings.Contains(resp.Body, "ErrDenied") {
		t.Fatalf("a second account connected an identity that was taken: %q\n%s",
			resp.Body, f.log.String())
	}
	if strings.Contains(resp.Body, "owner@example.com") ||
		strings.Contains(resp.Body, f.owner.UserID.String()) {
		t.Errorf("the refusal named the account that holds the link: %q", resp.Body)
	}
	// Warned rather than logged at debug: one identity offered to two accounts is a
	// person with two accounts or an attempt on one, and only an operator can tell.
	if !strings.Contains(f.log.String(), "level=WARN") ||
		!strings.Contains(f.log.String(), "refused an add-on's request to connect an external identity") {
		t.Errorf("the contested link was not warned about:\n%s", f.log.String())
	}
	// The row did not move, which is the half a status code cannot show.
	var owner uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT user_id FROM addon_identity_links WHERE subject = $1`,
		"subject-contested").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != f.owner.UserID {
		t.Fatalf("the link now points at %s and it was written for %s", owner, f.owner.UserID)
	}
	// And the assertion still signs in the account that connected it.
	minted := f.callback("/callback", "subject-contested", nil)
	if minted.Minted == nil {
		t.Fatalf("the surviving link stopped working: %q", minted.Body)
	}
	id, err := f.auth.Authenticate(t.Context(), minted.Minted.Token.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != f.owner.UserID {
		t.Errorf("the assertion signed in %s; the link belongs to %s", id.UserID, f.owner.UserID)
	}
	if f.sessions(stranger.UserID) != 0 {
		t.Error("the account that could not connect the identity has a session")
	}
}

// Connecting the same identity to the same account twice is a state that already
// holds, not a conflict. A person clicking connect a second time, or a browser
// retrying a callback, must not meet an error — and must not acquire a second row,
// because two rows for one subject would make *which account* an assertion resolves
// to depend on which one the query happened to read.
func TestConnectingTheSameIdentityTwiceSucceedsAndChangesNothing(t *testing.T) {
	f := newAddonAuth(t, nil, nil)

	if resp := f.link("subject-again", f.owner); !strings.Contains(resp.Body, "link: <nil>") {
		t.Fatalf("the first connection did not succeed: %q", resp.Body)
	}
	var id uuid.UUID
	var created time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id, created_at FROM addon_identity_links WHERE subject = $1`,
		"subject-again").Scan(&id, &created); err != nil {
		t.Fatal(err)
	}

	if resp := f.link("subject-again", f.owner); !strings.Contains(resp.Body, "link: <nil>") {
		t.Fatalf("connecting the same identity again was refused: %q\n%s",
			resp.Body, f.log.String())
	}

	var rows int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM addon_identity_links WHERE subject = $1`,
		"subject-again").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("connecting twice left %d rows for one subject", rows)
	}
	var again uuid.UUID
	var stillCreated time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id, created_at FROM addon_identity_links WHERE subject = $1`,
		"subject-again").Scan(&again, &stillCreated); err != nil {
		t.Fatal(err)
	}
	if again != id || !stillCreated.Equal(created) {
		t.Errorf("the row was rewritten rather than left alone: %s/%s became %s/%s",
			id, created, again, stillCreated)
	}
}

// **An API key is not a person and cannot be the signed-in party.** Connecting a
// provider decides how somebody *signs in*, so a credential that outlives their
// browser must not be able to arrange it: a leaked key that could connect an
// attacker's provider subject would have added a second, permanent way into the
// account it was issued from, and rotating the key would not take it away.
//
// D87's rule, and `LinkAddonIdentity` applies it at the top for that reason. It
// had no test on this path at all — the add-on boundary is the one place an
// external caller reaches it.
func TestAnAPIKeyCannotConnectAnIdentityProvider(t *testing.T) {
	f := newAddonAuth(t, nil, nil)

	keyID := uuid.Must(uuid.NewV7())
	asKey := *f.owner
	asKey.APIKeyID = &keyID

	resp := f.link("subject-by-key", &asKey)

	if !strings.Contains(resp.Body, "ErrDenied") {
		t.Fatalf("an API key connected an identity provider: %q\n%s",
			resp.Body, f.log.String())
	}
	var rows int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM addon_identity_links WHERE subject = $1`,
		"subject-by-key").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d links were written on an API key's say-so", rows)
	}
	// And the same claim from the same person's browser is fine, which is what
	// makes the refusal about the credential rather than about the claim.
	if resp := f.link("subject-by-key", f.owner); !strings.Contains(resp.Body, "link: <nil>") {
		t.Fatalf("the signed-in connection was refused too, so the test above proves "+
			"nothing about API keys: %q", resp.Body)
	}
}

// register adds a second account to the fixture, for the tests whose subject is
// what happens between two of them.
func (f *authFixture) register(email string) *auth.Identity {
	f.t.Helper()
	id, err := f.auth.Register(f.t.Context(), auth.RegisterInput{
		Email: email, Name: "Second", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		f.t.Fatalf("register %s: %v", email, err)
	}
	return id
}

// enrolSecondFactor puts an account into the state M53 calls enrolled, by the
// columns rather than through the enrolment flow: what this file is about is what
// the assertion does when it meets one, and the flow itself is mfa_test.go's.
func enrolSecondFactor(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE users SET mfa_enabled_at = now(), mfa_secret = $2 WHERE id = $1`,
		userID, []byte("not-a-real-secret")); err != nil {
		t.Fatal(err)
	}
}
