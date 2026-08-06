//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/mail"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
	"github.com/DevOfPie/LinkCtrl/internal/team"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// M29. Self-serve signup, and the one property everything here is about:
// `LINKCTRL_SIGNUP_MODE` is the mode and the operator is the only one who sets
// it (D38). Nothing reachable from a session or an API call changes it, and the
// only thing the process derives on top of it is D1's mailer rule.

const signupPassword = "a-sufficiently-long-password"

type signupFixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	auth    *auth.Service
	keys    *auth.APIKeyService
	signup  *signup.Service
	invites *invite.Service
	team    *team.Service
	mail    *mail.Service
	owner   *auth.Identity
	server  *httptest.Server
	client  *http.Client
	// lastLocation is the Location header of the most recent response, so a
	// form post can assert where it sent the browser without following it.
	lastLocation string
}

type signupOptions struct {
	// Mode is what LINKCTRL_SIGNUP_MODE was set to. Empty means `closed`, which
	// is what a fresh instance ships with.
	Mode signup.Mode
	// WithMailer configures a relay. Without one the effective mode is `invite`
	// whatever the environment says, because an address cannot be verified (D1).
	WithMailer bool
}

func newSignupFixture(t *testing.T, opts signupOptions) *signupFixture {
	t.Helper()
	pool := newDB(t)

	if opts.Mode == "" {
		opts.Mode = signup.Closed
	}

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: signupPassword,
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}

	f := &signupFixture{t: t, pool: pool, auth: authSvc, keys: keySvc, owner: owner}

	cfg := signup.Config{
		Mode:   opts.Mode,
		AppURL: "http://links.test",
		Hasher: authSvc.Hasher(),
	}
	if opts.WithMailer {
		f.mail = newMailService(t, pool, &recordingSender{})
		cfg.Mail = f.mail
	}
	if f.signup, err = signup.NewService(pool, cfg); err != nil {
		t.Fatalf("signup.NewService: %v", err)
	}

	f.invites, err = invite.NewService(pool, invite.Config{
		AppURL:      "http://links.test",
		TTL:         168 * time.Hour,
		NewAccounts: f.signup.Effective().AdmitsNewAccounts(),
		Hasher:      authSvc.Hasher(),
		Audit:       audit.NewService(pool),
		Notify:      notify.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.team = team.NewService(pool, team.Config{Audit: audit.NewService(pool)})
	return f
}

// serve brings up the HTTP surface, for the assertions that are about what a
// client can observe rather than what a service returns.
func (f *signupFixture) serve() *httptest.Server {
	f.t.Helper()
	if f.server != nil {
		return f.server
	}
	renderer, err := ui.New()
	if err != nil {
		f.t.Fatalf("parse templates: %v", err)
	}
	cfg := config.Config{
		AppEnv: config.Development, BaseURL: "http://links.test", SecureCookies: false,
	}
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour

	f.server = httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg, Auth: f.auth, Keys: f.keys,
		Invites: f.invites, Team: f.team, Signup: f.signup,
		Notify: notify.NewService(f.pool),
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: f.auth, Keys: f.keys,
			Invites: f.invites, Team: f.team, Signup: f.signup,
			Notify: notify.NewService(f.pool),
		},
	}))
	f.t.Cleanup(f.server.Close)

	jar, _ := newCookieJar()
	f.client = &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return f.server
}

func (f *signupFixture) get(path string) (int, string) {
	f.t.Helper()
	f.serve()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.send(req)
}

func (f *signupFixture) postForm(path string, vals url.Values) (int, string, string) {
	f.t.Helper()
	f.serve()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost,
		f.server.URL+path, strings.NewReader(vals.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, body := f.send(req)
	return status, body, f.lastLocation
}

func (f *signupFixture) postJSON(path string, body map[string]string) (int, string) {
	f.t.Helper()
	return f.json(http.MethodPost, path, body)
}

func (f *signupFixture) json(method, path string, body map[string]string) (int, string) {
	f.t.Helper()
	f.serve()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(f.t.Context(), method,
		f.server.URL+path, bytes.NewReader(payload))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return f.send(req)
}

func (f *signupFixture) send(req *http.Request) (int, string) {
	f.t.Helper()
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	f.lastLocation = resp.Header.Get("Location")
	return resp.StatusCode, string(raw)
}

// signIn puts a session for the named account in the jar.
func (f *signupFixture) signIn(email string) {
	f.t.Helper()
	status, _, loc := f.postForm("/login", url.Values{
		"email": {email}, "password": {signupPassword},
	})
	if status != http.StatusSeeOther {
		f.t.Fatalf("sign in as %s: status %d", email, status)
	}
	if loc != "/dashboard" {
		f.t.Fatalf("sign in as %s redirected to %q", email, loc)
	}
}

func (f *signupFixture) scalar(sql string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.pool.QueryRow(f.t.Context(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// verificationLink pulls the token out of whatever was queued for an address.
func (f *signupFixture) verificationLink(email string) string {
	f.t.Helper()
	var body string
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT body FROM mail_outbox WHERE recipient = $1 AND kind = $2
		  ORDER BY created_at DESC LIMIT 1`, email, signup.MailKind).Scan(&body); err != nil {
		f.t.Fatalf("no verification mail for %s: %v", email, err)
	}
	const prefix = "http://links.test/verify/"
	i := strings.Index(body, prefix)
	if i < 0 {
		f.t.Fatalf("the verification mail carries no link:\n%s", body)
	}
	rest := body[i+len(prefix):]
	return strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
}

// ─── the operator sets the mode, and nobody else (D38) ──────────────────────

// The milestone's central claim, and the one a reviewer should read first.
//
// D38 removed the runtime toggle rather than narrowing who could use it, so what
// is asserted here is an absence: none of the surfaces a toggle would have had
// exists, the schema carries neither a settings row nor a permission that could
// gate one, and the mode a signed-in owner sees is the one the process was
// started with.
func TestNoSessionOrAPICallCanChangeTheSignupMode(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Invite, WithMailer: true})
	f.signIn("owner@example.com")

	// Every surface the removed toggle had. An owner is signed in, so these are
	// not 401s in disguise — there is nothing there to reach.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/settings/signup"},
		{http.MethodPut, "/api/v1/settings/signup"},
		{http.MethodPost, "/api/v1/settings/signup"},
		{http.MethodGet, "/settings"},
		{http.MethodPost, "/settings/signup"},
	} {
		status, body := f.json(c.method, c.path, map[string]string{"mode": "open"})
		if status != http.StatusNotFound {
			t.Errorf("%s %s returned %d, want 404 — nothing may change the signup mode\n%s",
				c.method, c.path, status, body)
		}
	}

	// And nothing in the schema could gate one if it were added back by
	// accident: there is no settings row to write and no permission to hold.
	if n := f.scalar(`SELECT count(*) FROM information_schema.tables
	                   WHERE table_schema = 'public' AND table_name = 'settings'`); n != 0 {
		t.Error("a settings table exists; D38 removed the storage as well as the endpoints")
	}
	if n := f.scalar(`SELECT count(*) FROM permissions WHERE slug LIKE 'settings.%'`); n != 0 {
		t.Errorf("%d settings.* permissions are seeded; the mode is the operator's (D38)", n)
	}

	// The mode is still the environment's, and registration still answers to it.
	if got := f.signup.Effective(); got != signup.Invite {
		t.Errorf("effective mode = %q after every attempt above, want invite", got)
	}
	status, body := f.postJSON("/api/v1/auth/register", map[string]string{
		"email": "stranger@example.com", "password": signupPassword,
	})
	if status != http.StatusForbidden {
		t.Errorf("registration returned %d under a mode of invite, want 403\n%s", status, body)
	}
	if n := f.scalar(`SELECT count(*) FROM pending_registrations`); n != 0 {
		t.Errorf("%d registrations were started while public signup was shut", n)
	}
}

// The other half of the same claim: the mode is a property of the process, not
// of the database. Two services over one database disagree, because each was
// told a different thing by its environment — which is what makes changing it an
// `.env` edit and a restart rather than anything a request can do.
func TestTheModeComesFromTheEnvironmentAndNotTheDatabase(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Open, WithMailer: true})

	if got := f.signup.Effective(); got != signup.Open {
		t.Fatalf("effective mode = %q, want open", got)
	}
	closed, err := signup.NewService(f.pool, signup.Config{
		Mode: signup.Closed, AppURL: "http://links.test", Hasher: f.auth.Hasher(), Mail: f.mail,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := closed.Effective(); got != signup.Closed {
		t.Errorf("a process started with closed reads %q from the same database; "+
			"the mode must not be shared state", got)
	}
	if _, err := closed.Register(t.Context(), signup.RegisterInput{
		Email: "nope@example.com", Password: signupPassword,
	}); err == nil {
		t.Error("the closed process accepted a registration")
	}
}

// ─── `open` requires a mailer (D1) ──────────────────────────────────────────

// With no relay there is no way to prove an address, so an instance configured
// `open` is `invite` in fact. Stated in configuration.md beside the variable,
// and surfaced by refusing on GET — nobody fills in a password and discovers it
// at submit time.
func TestWithNoMailerTheEffectiveModeIsInvite(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Open})

	if got := f.signup.Effective(); got != signup.Invite {
		t.Errorf("effective mode = %q with LINKCTRL_SIGNUP_MODE=open and no relay, want invite", got)
	}
	if f.signup.Configured() != signup.Open {
		t.Error("the configured mode was rewritten; the derivation is a reading, not an edit")
	}
	if f.signup.MailerConfigured() {
		t.Error("MailerConfigured is true on an instance with no relay")
	}

	status, body := f.get("/signup")
	if status != http.StatusForbidden {
		t.Errorf("GET /signup with no mailer returned %d, want 403 — the refusal belongs "+
			"on the page, not after the password has been typed\n%s", status, body)
	}
	// And it says nothing about which of the two bounds applies. Both are the
	// operator's business and neither is a stranger's.
	if strings.Contains(body, "LINKCTRL_SIGNUP_MODE") || strings.Contains(body, "SMTP") {
		t.Error("the refusal describes the instance's configuration to a stranger")
	}
	if status, body := f.postJSON("/api/v1/auth/register", map[string]string{
		"email": "nope@example.com", "password": signupPassword,
	}); status != http.StatusForbidden {
		t.Errorf("registration returned %d with no mailer, want 403\n%s", status, body)
	}
	if n := f.scalar(`SELECT count(*) FROM users`); n != 1 {
		t.Errorf("%d accounts exist, want only the owner's", n)
	}
}

// ─── registration verifies the address before the account is usable ─────────

func TestOpenRegistrationCreatesNothingUntilTheAddressIsProven(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Open, WithMailer: true})

	status, body := f.postJSON("/api/v1/auth/register", map[string]string{
		"email": "New@Example.com", "name": "New", "password": signupPassword,
	})
	if status != http.StatusAccepted {
		t.Fatalf("register returned %d, want 202 — nothing exists yet\n%s", status, body)
	}
	if n := f.scalar(`SELECT count(*) FROM users WHERE email_lower = 'new@example.com'`); n != 0 {
		t.Fatal("registration created an account before the address was verified")
	}
	if n := f.scalar(`SELECT count(*) FROM pending_registrations WHERE consumed_at IS NULL`); n != 1 {
		t.Fatalf("%d pending registrations, want exactly 1", n)
	}
	// No tenancy either. An unverified address must not leave an organization
	// behind, which is the whole reason the account is not created here.
	if n := f.scalar(`SELECT count(*) FROM organizations`); n != 1 {
		t.Errorf("%d organizations, want only the owner's", n)
	}

	link := f.verificationLink("new@example.com")

	// The link is a page with a button, not a URL that acts. A mail scanner
	// fetching it must not finish somebody else's registration.
	if status, page := f.get("/verify/" + link); status != http.StatusOK {
		t.Fatalf("GET the verification page returned %d\n%s", status, page)
	}
	if n := f.scalar(`SELECT count(*) FROM users WHERE email_lower = 'new@example.com'`); n != 0 {
		t.Fatal("opening the verification page created the account; a GET must not")
	}

	status, page, loc := f.postForm("/verify/"+link, nil)
	if status != http.StatusSeeOther {
		t.Fatalf("confirming returned %d, want 303\n%s", status, page)
	}
	if loc != "/login?verified=1" {
		t.Errorf("confirming redirected to %q", loc)
	}

	// D6: the account, an organization of its own, a workspace in it, and owner
	// membership — the opposite of what an invitation produces.
	var verified *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT email_verified_at FROM users WHERE email_lower = 'new@example.com'`,
	).Scan(&verified); err != nil {
		t.Fatalf("the account was not created: %v", err)
	}
	if verified == nil {
		t.Error("email_verified_at is null on an account created by following a verification link")
	}
	if n := f.scalar(`
		SELECT count(*) FROM memberships m
		  JOIN users u ON u.id = m.user_id
		  JOIN roles r ON r.id = m.role_id
		 WHERE u.email_lower = 'new@example.com' AND r.slug = 'owner'`); n != 1 {
		t.Errorf("the self-registered account holds %d owner memberships, want 1 (D6)", n)
	}
	if n := f.scalar(`
		SELECT count(*) FROM workspaces w
		  JOIN memberships m ON m.organization_id = w.organization_id
		  JOIN users u ON u.id = m.user_id
		 WHERE u.email_lower = 'new@example.com'`); n != 1 {
		t.Errorf("the self-registered account reaches %d workspaces, want its own 1 (D6)", n)
	}

	// Single-use. The second confirmation says the same thing every dead link
	// gets, and creates nothing.
	if status, _, _ := f.postForm("/verify/"+link, nil); status != http.StatusNotFound {
		t.Errorf("a spent verification link returned %d, want 404", status)
	}

	// And the account works, which is what "usable" means.
	status, _, loc = f.postForm("/login", url.Values{
		"email": {"new@example.com"}, "password": {signupPassword},
	})
	if status != http.StatusSeeOther || loc != "/dashboard" {
		t.Errorf("the verified account could not sign in: %d %q", status, loc)
	}
}

// Registration cannot be asked whether an address already has an account.
//
// Until 0.2.0 it could: a taken address answered 409 and a free one answered
// 202, on an endpoint that is unauthenticated whenever the mode is `open`, so
// a leaked address list could be tested for membership against this instance.
// M27 already spends argon2 work so redemption cannot be asked the same
// question (D27), which is what made the disagreement worth closing rather
// than documenting.
//
// The assertion is deliberately about the *response* and not about the mail: an
// oracle is anything the caller can tell apart, so the status and the body are
// compared directly rather than checked against expectations one at a time.
// What differs is what lands in the outbox, and that reaches the address rather
// than whoever typed it.
func TestRegistrationCannotBeAskedWhetherAnAddressIsTaken(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Open, WithMailer: true})

	// owner@example.com is the account the fixture registers. free@example.com
	// has never been seen.
	takenStatus, takenBody := f.postJSON("/api/v1/auth/register", map[string]string{
		"email": "owner@example.com", "name": "Somebody", "password": signupPassword,
	})
	freeStatus, freeBody := f.postJSON("/api/v1/auth/register", map[string]string{
		"email": "free@example.com", "name": "Somebody", "password": signupPassword,
	})

	if takenStatus != http.StatusAccepted {
		t.Errorf("a taken address answered %d, want 202 — the same as a free one\n%s",
			takenStatus, takenBody)
	}
	if takenStatus != freeStatus {
		t.Errorf("taken answered %d and free answered %d; the difference is the oracle",
			takenStatus, freeStatus)
	}
	// Two fields must differ and carry nothing: the address is the caller's own
	// input echoed back, and the expiry is a clock reading. Everything else is
	// compared literally, which is how the fractional-second precision was
	// caught — the taken branch returned Go's nanosecond clock while the free
	// one returned a value Postgres had truncated to a microsecond, so the
	// digit count answered the question the status code no longer did.
	normalize := func(body, email string) string {
		out := strings.Replace(body, email, "<address>", 1)
		if i := strings.Index(out, `"expires_at":"`); i >= 0 {
			j := strings.Index(out[i+14:], `"`)
			if j >= 0 {
				out = out[:i+14] + "<when>" + out[i+14+j:]
			}
		}
		return out
	}
	takenShape := normalize(takenBody, "owner@example.com")
	freeShape := normalize(freeBody, "free@example.com")
	if takenShape != freeShape {
		t.Errorf("the bodies differ beyond the address and the clock, which is the "+
			"oracle moved rather than closed:\ntaken: %s\nfree:  %s", takenShape, freeShape)
	}
	if strings.Contains(takenShape, "@") {
		t.Errorf("the taken body still carries an address after normalization: %s", takenShape)
	}

	// Nothing is written for the taken address. A pending registration would
	// supersede whatever the real owner had outstanding, so a stranger could
	// invalidate their link by typing their address into the form.
	if n := f.scalar(
		`SELECT count(*) FROM pending_registrations WHERE email = 'owner@example.com'`); n != 0 {
		t.Errorf("%d pending registrations for an address that already has an account, want 0", n)
	}
	if n := f.scalar(
		`SELECT count(*) FROM pending_registrations WHERE email = 'free@example.com'`); n != 1 {
		t.Errorf("%d pending registrations for the free address, want 1", n)
	}

	// The answer went to the address instead, and it is not the verification
	// mail — sending that would be worse than the 409, because it would put a
	// working link to somebody else's account in the post.
	if n := f.scalar(
		`SELECT count(*) FROM mail_outbox WHERE recipient = 'owner@example.com' AND kind = $1`,
		signup.MailKindExists); n != 1 {
		t.Errorf("%d account-exists messages queued for the taken address, want 1", n)
	}
	if n := f.scalar(
		`SELECT count(*) FROM mail_outbox WHERE recipient = 'owner@example.com' AND kind = $1`,
		signup.MailKind); n != 0 {
		t.Errorf("%d verification messages queued for an address that already has an "+
			"account; that link would create nothing and says the wrong thing", n)
	}
}

// An address the pattern accepts and the mailer cannot parse is refused before
// anything is written, rather than committing a row and then answering 500 with
// a status the API does not declare (F53).
func TestAnUnsendableAddressIsRefusedRatherThanCommitted(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Open, WithMailer: true})

	// Every one of these matches auth's deliberately permissive pattern and is
	// rejected by net/mail.ParseAddress, which is the parser the mailer uses.
	for _, addr := range []string{"a<b@c.de", "a,b@c.de", "user@exa(mple.com", `a"b@c.de`} {
		status, body := f.postJSON("/api/v1/auth/register", map[string]string{
			"email": addr, "name": "Somebody", "password": signupPassword,
		})
		if status != http.StatusUnprocessableEntity {
			t.Errorf("register %q answered %d, want 422\n%s", addr, status, body)
		}
	}
	if n := f.scalar(`SELECT count(*) FROM pending_registrations`); n != 0 {
		t.Errorf("%d pending registrations committed for addresses nothing can send to", n)
	}
}

// Closing sign-ups closes the ones already in flight. A link lives for a day,
// and an operator can lower LINKCTRL_SIGNUP_MODE and restart inside that window
// — so verification asks the mode again rather than trusting that it was open
// when the mail went out. D7's bound is a state the instance is in, not a moment
// a request passed through.
func TestClosingSignupsStopsARegistrationAlreadyInFlight(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Open, WithMailer: true})

	if status, body := f.postJSON("/api/v1/auth/register", map[string]string{
		"email": "late@example.com", "password": signupPassword,
	}); status != http.StatusAccepted {
		t.Fatalf("register returned %d\n%s", status, body)
	}
	link := f.verificationLink("late@example.com")

	// The same database, read by the process the operator restarted.
	closed, err := signup.NewService(f.pool, signup.Config{
		Mode: signup.Closed, AppURL: "http://links.test", Hasher: f.auth.Hasher(), Mail: f.mail,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := closed.Verify(t.Context(), link); err == nil {
		t.Error("a verification link completed after sign-ups were closed")
	}
	if n := f.scalar(`SELECT count(*) FROM users WHERE email_lower = 'late@example.com'`); n != 0 {
		t.Error("an account was created after sign-ups were closed")
	}
}

// ─── `closed` admits no new account by any path (D7) ────────────────────────

// The form, the API and an invitation, all refused by one word. `closed` is the
// shipped default, so this is what a fresh instance does.
func TestClosedAdmitsNobodyByAnyPath(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Closed, WithMailer: true})

	status, body := f.get("/signup")
	if status != http.StatusForbidden {
		t.Errorf("GET /signup on a closed instance returned %d, want 403", status)
	}
	if strings.Contains(body, "LINKCTRL_SIGNUP_MODE") || strings.Contains(body, "SMTP") {
		t.Error("the refusal describes the instance's configuration to a stranger")
	}
	if status, body, _ := f.postForm("/signup", url.Values{
		"email": {"nope@example.com"}, "password": {signupPassword},
	}); status != http.StatusForbidden {
		t.Errorf("POST /signup on a closed instance returned %d, want 403\n%s", status, body)
	}
	if status, body := f.postJSON("/api/v1/auth/register", map[string]string{
		"email": "nope@example.com", "password": signupPassword,
	}); status != http.StatusForbidden {
		t.Errorf("POST /auth/register on a closed instance returned %d, want 403\n%s", status, body)
	}

	// And the path an invitation would take. M27 already refuses this; what M29
	// adds is that the refusal now follows the *effective* mode, which is the
	// signup service's answer rather than a second reading of the variable.
	inv, err := f.invites.Create(t.Context(), f.owner, invite.CreateInput{
		Email: "invited@example.com", Role: "editor",
	})
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	if _, err := f.invites.Redeem(t.Context(), invite.RedeemInput{
		Token:    strings.TrimPrefix(inv.URL, "http://links.test/invite/"),
		Email:    "invited@example.com",
		Password: signupPassword,
	}); err == nil {
		t.Error("an invitation created an account on a closed instance (D7)")
	}
	if n := f.scalar(`SELECT count(*) FROM users`); n != 1 {
		t.Errorf("%d accounts exist on a closed instance, want only the owner's", n)
	}

	// The sign-in page does not advertise a form that answers 403.
	if _, page := f.get("/login"); strings.Contains(page, `href="/signup"`) {
		t.Error("the sign-in page offers a link to a form that answers 403")
	}
}

// ─── the browser form ───────────────────────────────────────────────────────

// The page works with JavaScript switched off, which for this dashboard means
// it is an ordinary form: no script tag, no hx- attribute deciding whether the
// post happens at all.
func TestTheSignupPageIsAnOrdinaryFormAndSaysWhatAnAccountGets(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Open, WithMailer: true})

	status, body := f.get("/signup")
	if status != http.StatusOK {
		t.Fatalf("GET /signup returned %d\n%s", status, body)
	}
	if !strings.Contains(body, `<form method="post" action="/signup"`) {
		t.Error("the page has no plain form; it cannot work without JavaScript")
	}
	// The layout loads htmx on every page, so its presence proves nothing. What
	// matters is that this form does not use it: an hx- attribute here would
	// make the submission itself depend on script running.
	if strings.Contains(body, "hx-post") || strings.Contains(body, "hx-get") ||
		strings.Contains(body, "hx-confirm") {
		t.Error("the sign-up form depends on script to submit")
	}
	// D6's wording. Somebody who came here expecting to join a team has to read
	// it before they type, which is the whole reason this milestone ships after
	// invitations.
	if !strings.Contains(body, "organization and a workspace of your own") {
		t.Error("the form does not say what an account gets (D6)")
	}
	if !strings.Contains(body, "invitation") {
		t.Error("the form does not say that joining an existing organization needs an invitation")
	}

	status, body, _ = f.postForm("/signup", url.Values{
		"email": {"browser@example.com"}, "name": {"B"}, "password": {signupPassword},
	})
	if status != http.StatusOK {
		t.Fatalf("POST /signup returned %d\n%s", status, body)
	}
	if !strings.Contains(body, "Check your inbox") {
		t.Errorf("the page does not tell the reader to check their mail:\n%s", body)
	}
	if n := f.scalar(`SELECT count(*) FROM users WHERE email_lower = 'browser@example.com'`); n != 0 {
		t.Error("the form created an account before the address was verified")
	}

	// The sign-in page advertises it, but only while it is actually reachable.
	if _, page := f.get("/login"); !strings.Contains(page, `href="/signup"`) {
		t.Error("the sign-in page does not offer the form on an open instance")
	}
}

// ─── rate limiting ──────────────────────────────────────────────────────────

// "Registration is rate-limited like login" means the same limiter, not a
// second one with the same number: alternating between the two surfaces must not
// double an attacker's budget.
func TestRegistrationSharesTheLoginRateLimit(t *testing.T) {
	f := newLimited(t, func(c *config.Config) { c.Auth.LoginRatePerMin = 4 })

	// Spend the whole budget on the login endpoint.
	for range 4 {
		resp := f.do(http.MethodPost, "/api/v1/auth/login",
			map[string]string{"email": "nobody@example.com", "password": "wrong-but-long-enough"})
		_ = resp.Body.Close()
	}
	resp := f.do(http.MethodPost, "/api/v1/auth/register",
		map[string]string{"email": "late@example.com", "password": signupPassword})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("registration returned %d after the login budget was spent, want 429; "+
			"the two surfaces are not sharing a limiter", resp.StatusCode)
	}
}

// ─── the sweep ──────────────────────────────────────────────────────────────

// A waiting room with no sweep is the one table that grows forever with nothing
// watching it, which is the shape D5 and M21 exist to stop repeating.
func TestLapsedRegistrationsAreSweptAway(t *testing.T) {
	f := newSignupFixture(t, signupOptions{Mode: signup.Open, WithMailer: true})

	if status, body := f.postJSON("/api/v1/auth/register", map[string]string{
		"email": "lapsed@example.com", "password": signupPassword,
	}); status != http.StatusAccepted {
		t.Fatalf("register returned %d\n%s", status, body)
	}
	// Nothing is swept while the window is open.
	if n, err := f.signup.PurgeLapsed(t.Context()); err != nil || n != 0 {
		t.Fatalf("PurgeLapsed removed %d live registrations (err %v)", n, err)
	}

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE pending_registrations SET expires_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	n, err := f.signup.PurgeLapsed(t.Context())
	if err != nil {
		t.Fatalf("PurgeLapsed: %v", err)
	}
	if n != 1 {
		t.Errorf("PurgeLapsed removed %d rows, want 1", n)
	}
	if got := f.scalar(`SELECT count(*) FROM pending_registrations`); got != 0 {
		t.Errorf("%d registrations survived the sweep", got)
	}
}
