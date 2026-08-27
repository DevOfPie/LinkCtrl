//go:build integration

package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/account"
	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// This file is M69: the acceptance test the whole add-on foundation was built
// toward, and it is the only test in this repository that drives somebody through
// a sign-in from end to end.
//
// # What makes it an acceptance test rather than another add-on test
//
// Nothing in it is a fixture of this repository's. The module is
// `DevOfPie/LinkCtrl-OIDC` — a different repository, published to the module
// proxy, consuming only the SDK this repository publishes — rebuilt by
// `make oidc-fixture` into the exact artifact its release names, and installed
// under the exact manifest that release ships. The provider is dex, in
// docker-compose.integration.yml, pinned by digest, and it speaks the real
// protocol: real discovery, real PKCE, a real `client_secret_post` exchange, a
// real RS256 ID token and a real key set. Every other add-on test in this
// package loads a module written to make an assertion; this one loads a module
// written to sign somebody in, and asks whether it can.
//
// # What is emulated, and what is not
//
// The browser. There is no browser in this suite, so the test *is* the browser:
// it follows the provider's redirects, fills in the provider's own login form and
// carries the cookies. Two clients rather than one, because a jar keys on host and
// nothing else — so one client at the instance and one at the provider is the only
// arrangement in which this instance's session cookie is never sent to the
// provider, which is what a real browser's origin separation gives for free.
//
// The one address that is rewritten is the last hop. The provider redirects to
// `redirect_uri`, which is a name that does not resolve — `https://linkctrl.test/…`
// — and the test issues that request against the instance under test instead. The
// value itself is never touched: it is what the add-on sent, what dex validated
// against its client registration, and what the token exchange re-sends. What
// changes is only which address the *browser* dials, which is what a real
// deployment's DNS would decide.
//
// # The two bounds this test relaxes, and where
//
// [addon.Host.TestReach] and [addon.Host.TestTrust], both of which exist only
// under the `integration` build tag — see internal/addon/egress_integration.go for
// why a containerized provider is unreachable without them and what is *not*
// relaxed. Everything else about the egress door is the shipped one: https only,
// the operator's origin allowlist, no add-on-chosen header, the redirect rule, the
// size caps.

const (
	// The provider, as test/integration/testdata/idp/config.yaml declares it. An
	// issuer is compared byte for byte against the `iss` claim, so this constant,
	// that file, docker-compose.integration.yml's published port and the SAN in the
	// certificate scripts/idp.sh generates are one fact in four places.
	oidcIssuer = "https://127.0.0.1:5554/dex"
	oidcOrigin = "https://127.0.0.1:5554"

	oidcClientID     = "linkctrl-integration"
	oidcClientSecret = "linkctrl-integration-secret"

	// Registered with dex, and deliberately a name that does not resolve. Nothing
	// dials it: the browser leg stops at this redirect and re-issues it against the
	// instance under test. A value that *did* resolve would be a real address a
	// failing test could deliver an authorization code to.
	oidcRedirectURI = "https://linkctrl.test/addons/oidc/callback"

	// The one person the provider knows about, and the password is the word
	// `password` — see the config file, where the bcrypt hash of it is written out
	// rather than hidden.
	oidcPersonEmail    = "person@example.com"
	oidcPersonPassword = "password"

	// The account on this instance, which is a different thing entirely and is the
	// point of the linking half: the addresses do not have to match and matching
	// them would not link anything, because this host matches on subject and issuer
	// and never on an email address.
	oidcOwnerEmail    = "owner@example.com"
	oidcOwnerPassword = "a-sufficiently-long-password"

	oidcFixtureDir = "testdata/oidc"
	oidcCACert     = "testdata/idp/tls/idp.crt"
)

// oidcFixture is a stock instance with the released OIDC add-on installed, and a
// browser at each of the two hosts the flow visits.
type oidcFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	host   *addon.Host
	auth   *auth.Service
	server *httptest.Server
	idp    *http.Client
	log    *logSink
	owner  *auth.Identity
}

func newOIDC(t *testing.T) *oidcFixture {
	t.Helper()
	manifest, module := oidcRelease(t)
	ca := providerCA(t)

	pool, dsn, root := newAddonDB(t, "oidc")

	// The add-on's directory is its name, and its name is the manifest's. Written
	// out here rather than through installAddon, because installAddon composes a
	// manifest of its own and what has to be installed is the *published* one:
	// M60's loader hashes the module and compares it against the `sha256` that
	// manifest carries, and a manifest this test wrote would be this test agreeing
	// with itself.
	dir := filepath.Join(root, "oidc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, addon.ManifestFile), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oidc.wasm"), module, 0o644); err != nil {
		t.Fatal(err)
	}

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params:  fastParams,
		TTL:     auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
		Lockout: auth.LockoutPolicy{Threshold: 5, Window: 15 * time.Minute},
	})
	auditSvc := audit.NewService(pool)
	authSvc.SetSessionAuditor(auditSvc)

	owner, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: oidcOwnerEmail, Name: "Owner", Password: oidcOwnerPassword, IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("register the account the provider will be linked to: %v", err)
	}

	sink := &logSink{}
	metrics := observability.NewMetrics()
	host, err := addon.Open(t.Context(), addon.Options{
		Dir:      root,
		Logger:   slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics:  metrics,
		DB:       pool,
		DSN:      dsn,
		Sessions: authSvc,
		Audit:    auditSvc,
		// The operator's answers. Only the five the add-on cannot run without: the
		// other six have defaults in the manifest, and leaving them unset is what an
		// operator who filled in the form actually has.
		Settings: func(string, []string) map[string]config.Secret {
			return map[string]config.Secret{
				"issuer":           oidcIssuer,
				"client_id":        oidcClientID,
				"client_secret":    oidcClientSecret,
				"redirect_uri":     oidcRedirectURI,
				"provider_origins": oidcOrigin,
			}
		},
		// Room, not a relaxed product bound. A route invocation instantiates a
		// 5.5 MB module and then makes three outbound requests, and under `-race` on
		// a hosted runner that is nothing like the shipped defaults' budget — F326 is
		// what a suite that only proves this machine is fast costs. The *defaults*
		// are asserted where they belong: internal/config tests the nesting rule and
		// internal/addon tests the three-fetch arithmetic.
		RouteDeadline:       60 * time.Second,
		InstantiateDeadline: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("the released OIDC add-on did not load: %v\n%s", err, sink.String())
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	// The two integration-only relaxations. See internal/addon/egress_integration.go.
	host.TestReach(netip.MustParseAddr("127.0.0.1"))
	if err := host.TestTrust(ca); err != nil {
		t.Fatalf("trusting the provider's certificate: %v", err)
	}

	f := &oidcFixture{t: t, pool: pool, host: host, auth: authSvc, log: sink, owner: owner}
	f.server = httptest.NewServer(httpx.NewRouter(stockDeps(t, pool, authSvc, host)))
	t.Cleanup(f.server.Close)

	f.idp = &http.Client{
		Transport: &http.Transport{TLSClientConfig: providerTLS(t, ca)},
		Jar:       mustJar(t),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 20 * time.Second,
	}
	return f
}

// stockDeps is the application tree an instance runs, with the add-on host wired
// where cmd/linkctrl wires it.
//
// Everything here is what main.go passes. The point of assembling it rather than
// calling the add-on host directly is that this milestone's claim is about the
// *instance*: the route has to be mounted where M64 mounts it, the response has to
// go through the wrapping M64 imposes, and the session cookie has to be written by
// internal/httpx from `resp.Minted` — which is the one place a module's word turns
// into a credential.
func stockDeps(t *testing.T, pool *pgxpool.Pool, authSvc *auth.Service, host *addon.Host,
) httpx.Deps {
	t.Helper()
	cfg := config.Config{
		AppEnv:        config.Development,
		BaseURL:       "http://links.test",
		SecureCookies: false,
	}
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour

	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.BaseURL,
	})
	stats := analytics.NewReader(pool)
	notifySvc := notify.NewService(pool)
	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	accountSvc, err := account.NewService(pool, account.Config{
		Auth: authSvc, Audit: audit.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}
	return httpx.Deps{
		Config:   cfg,
		Health:   &httpx.Health{DB: pool},
		Auth:     authSvc,
		Keys:     keySvc,
		Links:    linkSvc,
		Stats:    stats,
		Notify:   notifySvc,
		Accounts: accountSvc,
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc, Keys: keySvc,
			Links: linkSvc, Stats: stats, Notify: notifySvc,
			Accounts: accountSvc,
			Addons:   host,
		},
	}
}

// --- the fixture's two artifacts --------------------------------------------

// oidcRelease reads the published add-on, and refuses to skip when it is absent.
//
// A skip would be a green run of the one test whose subject is whether this
// product's add-on foundation works, which is the failure mode
// `make ci-integration` refuses for the Redis tier and refuses here for the same
// reason. Both make targets that run this suite take `oidc-fixture` first.
func oidcRelease(t *testing.T) (manifest, module []byte) {
	t.Helper()
	manifest = mustFixtureFile(t, filepath.Join(oidcFixtureDir, addon.ManifestFile))
	module = mustFixtureFile(t, filepath.Join(oidcFixtureDir, "oidc.wasm"))
	return manifest, module
}

func providerCA(t *testing.T) []byte {
	t.Helper()
	return mustFixtureFile(t, oidcCACert)
}

// providerTLS is what the *test's own* browser verifies the provider with. It is
// a separate pool from the one the host is given (TestTrust) and deliberately so:
// this one is the browser's trust and has nothing to do with what this product
// will dial.
func providerTLS(t *testing.T, ca []byte) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatalf("%s holds no PEM certificate", oidcCACert)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func mustFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v\n\n"+
			"M69's acceptance test needs the published OIDC add-on and a running "+
			"identity provider. Both are make targets:\n"+
			"  make oidc-fixture   # fetches and rebuilds github.com/DevOfPie/LinkCtrl-OIDC\n"+
			"  make idp-up         # starts dex and waits for its discovery document\n"+
			"`make test-integration` and `make ci-integration` take both.", err)
	}
	return b
}

// --- the browser --------------------------------------------------------------

// begin asks the instance to start a flow and answers where it sent the browser.
func (f *oidcFixture) begin(browser *http.Client, path string) string {
	f.t.Helper()
	resp := f.visit(browser, f.server.URL+path)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusFound {
		f.t.Fatalf("%s answered %d, not a redirect to the provider:\n%s\n%s",
			path, resp.StatusCode, body, f.log.String())
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, oidcIssuer) {
		f.t.Fatalf("%s redirected to %q, which is not the configured provider", path, loc)
	}
	return loc
}

// finish delivers the provider's answer to the instance, as the browser would.
func (f *oidcFixture) finish(browser *http.Client, q url.Values) *http.Response {
	f.t.Helper()
	return f.visit(browser, f.server.URL+"/addons/oidc/callback?"+q.Encode())
}

func (f *oidcFixture) visit(browser *http.Client, target string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, target, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("User-Agent", "linkctrl-integration/1.0")
	resp, err := browser.Do(req)
	if err != nil {
		f.t.Fatalf("GET %s: %v", target, err)
	}
	return resp
}

// loginForm is dex's own sign-in form. The image is pinned by digest, so the
// shape of this page is fixed for as long as the pin is — which is the whole
// reason a form may be read at all rather than driven through a browser.
var loginForm = regexp.MustCompile(`(?is)<form[^>]*method="post"[^>]*action="([^"]*)"`)

// authenticate walks the provider's pages until it hands back an authorization
// code, and answers the query the provider redirected to `redirect_uri` with.
//
// Written as a loop over hops rather than as four named requests on purpose: what
// this test is asserting is the protocol either side of the provider, and a
// provider that inserts or removes one of its own pages has not changed anything
// this milestone claims.
func (f *oidcFixture) authenticate(start string) url.Values {
	f.t.Helper()
	next := start
	var form url.Values

	for hop := 0; hop < 12; hop++ {
		req := f.providerRequest(next, form)
		form = nil
		resp, err := f.idp.Do(req)
		if err != nil {
			f.t.Fatalf("the provider did not answer %s: %v", next, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if loc := resp.Header.Get("Location"); loc != "" &&
			resp.StatusCode >= 300 && resp.StatusCode < 400 {
			target := f.resolve(next, loc)
			if strings.HasPrefix(target, oidcRedirectURI) {
				u, err := url.Parse(target)
				if err != nil {
					f.t.Fatalf("the provider's redirect is not a URL: %v", err)
				}
				return u.Query()
			}
			next = target
			continue
		}
		if resp.StatusCode != http.StatusOK {
			f.t.Fatalf("the provider answered %d at %s:\n%s", resp.StatusCode, next, body)
		}
		m := loginForm.FindSubmatch(body)
		if m == nil {
			f.t.Fatalf("the provider answered a page with no sign-in form at %s:\n%s",
				next, body)
		}
		next = f.resolve(next, html.UnescapeString(string(m[1])))
		form = url.Values{"login": {oidcPersonEmail}, "password": {oidcPersonPassword}}
	}
	f.t.Fatalf("the provider never redirected to %s", oidcRedirectURI)
	return nil
}

func (f *oidcFixture) providerRequest(target string, form url.Values) *http.Request {
	f.t.Helper()
	if form == nil {
		req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, target, nil)
		if err != nil {
			f.t.Fatal(err)
		}
		return req
	}
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost, target,
		strings.NewReader(form.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func (f *oidcFixture) resolve(base, ref string) string {
	f.t.Helper()
	b, err := url.Parse(base)
	if err != nil {
		f.t.Fatalf("the page this hop came from is not a URL: %v", err)
	}
	r, err := url.Parse(ref)
	if err != nil {
		f.t.Fatalf("the provider's Location is not a URL: %v", err)
	}
	return b.ResolveReference(r).String()
}

// browser is a fresh client at the instance: its own jar, and no session.
func (f *oidcFixture) browser() *http.Client {
	f.t.Helper()
	return &http.Client{
		Jar: mustJar(f.t),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 90 * time.Second,
	}
}

// signIn puts a browser into the owner's session through the ordinary form, which
// is the half of "linking comes first" that has nothing to do with add-ons.
func (f *oidcFixture) signIn(browser *http.Client) {
	f.t.Helper()
	resp, err := browser.PostForm(f.server.URL+"/login", url.Values{
		"email": {oidcOwnerEmail}, "password": {oidcOwnerPassword},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("the owner could not sign in with a password: %d\n%s", resp.StatusCode, body)
	}
}

func mustJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := newCookieJar()
	if err != nil {
		t.Fatal(err)
	}
	return jar
}

func (f *oidcFixture) links() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM addon_identity_links`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *oidcFixture) sessionCount() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM sessions WHERE user_id = $1`, f.owner.UserID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// --- the milestone's own claims ----------------------------------------------

// The whole of m69.md's first two bullets, in the order somebody walks them.
//
// Both linking directions are here rather than in two tests because the second is
// only reachable through the first: an unlinked subject has to be refused *before*
// anything links it, and the same subject has to sign in *after*. Split into two
// fixtures they would be two flows against two instances, and the claim that the
// same external identity changes status because a link was made would not be the
// thing being asserted.
func TestAStockInstanceSignsSomebodyInThroughOIDC(t *testing.T) {
	f := newOIDC(t)

	// --- refused, because nobody has connected this provider to an account -----
	//
	// M65's boundary under a real protocol: the ID token is valid, signed by the
	// provider's own key, and asserts a subject this instance has never seen. The
	// host answers ErrNotFound and mints nothing. Matching the address against
	// `owner@example.com` would be the account-takeover shape, and the two
	// addresses differ on purpose so a host that did it would sign somebody in
	// here.
	stranger := f.browser()
	refused := f.finish(stranger, f.authenticate(f.begin(stranger, "/addons/oidc/start")))
	body, _ := io.ReadAll(refused.Body)
	_ = refused.Body.Close()

	if refused.StatusCode == http.StatusFound {
		t.Fatalf("an unlinked subject was signed in: the callback answered a redirect "+
			"to %q\n%s", refused.Header.Get("Location"), f.log.String())
	}
	if cookies := refused.Cookies(); sessionCookieIn(cookies) {
		t.Fatal("an unlinked subject was refused and a session cookie was written anyway")
	}
	if n := f.sessionCount(); n != 0 {
		t.Fatalf("an unlinked subject left %d session(s) behind", n)
	}
	if n := f.links(); n != 0 {
		t.Fatalf("a refused sign-in wrote %d identity link(s)", n)
	}
	// The page a person meets, and it is the host's wrapping rather than the
	// module's own markup — M64's rule, which an authentication add-on is the most
	// likely thing to want an exception to.
	if ct := refused.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("the refusal answered %q rather than a page", ct)
	}
	if strings.Contains(string(body), oidcPersonEmail) {
		t.Errorf("the refusal page shows the address the provider asserted, which "+
			"nobody who reached it has proved they own:\n%s", body)
	}

	// --- linked, by somebody who is already signed in --------------------------
	//
	// The other direction, and the only one that writes a link: the host makes a
	// link for whoever holds the session, at that moment, in that browser. The
	// add-on refuses `/start` for a signed-in browser and refuses `/link` for one
	// that is not, so this leg cannot be reached by accident from the other.
	owner := f.browser()
	f.signIn(owner)

	linked := f.finish(owner, f.authenticate(f.begin(owner, "/addons/oidc/link")))
	linkedBody, _ := io.ReadAll(linked.Body)
	_ = linked.Body.Close()
	if linked.StatusCode != http.StatusFound {
		t.Fatalf("linking answered %d rather than a redirect:\n%s\n%s",
			linked.StatusCode, linkedBody, f.log.String())
	}
	if got := linked.Header.Get("Location"); got != "/dashboard" {
		t.Errorf("a completed link landed on %q, not the after_link default", got)
	}
	if n := f.links(); n != 1 {
		t.Fatalf("linking wrote %d rows, not one", n)
	}

	var subject, issuer string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT subject, issuer FROM addon_identity_links`).Scan(&subject, &issuer); err != nil {
		t.Fatal(err)
	}
	if issuer != oidcIssuer {
		t.Errorf("the link records the issuer as %q; the provider calls itself %q",
			issuer, oidcIssuer)
	}
	if subject == "" || subject == oidcPersonEmail {
		t.Errorf("the link is keyed on %q, which is not an opaque subject — this host "+
			"must never match an external identity on an email address", subject)
	}

	// --- signed in, in a browser that has never been signed in -----------------
	//
	// A different browser with no session, so the session that exists at the end of
	// it is one this instance minted on the add-on's word and on nothing else.
	fresh := f.browser()
	signedIn := f.finish(fresh, f.authenticate(f.begin(fresh, "/addons/oidc/start")))
	signedInBody, _ := io.ReadAll(signedIn.Body)
	_ = signedIn.Body.Close()
	if signedIn.StatusCode != http.StatusFound {
		t.Fatalf("the callback answered %d rather than signing somebody in:\n%s\n%s",
			signedIn.StatusCode, signedInBody, f.log.String())
	}
	if got := signedIn.Header.Get("Location"); got != "/dashboard" {
		t.Errorf("a minted session landed on %q, not the after_sign_in default", got)
	}

	token := sessionCookieValue(signedIn.Cookies())
	if token == "" {
		t.Fatalf("no session cookie was written:\n%v", signedIn.Cookies())
	}
	id, err := f.auth.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatalf("the cookie the host wrote does not resolve to a session: %v", err)
	}
	if id.UserID != f.owner.UserID {
		t.Errorf("the session belongs to %s; the link names %s", id.UserID, f.owner.UserID)
	}
	if id.Email != oidcOwnerEmail {
		t.Errorf("the session's address is %q, and the assertion carried %q — the host "+
			"must resolve the account through the link and never through the claim",
			id.Email, oidcPersonEmail)
	}

	// The cookie works on a page that requires one, which is what "signs a user in"
	// means to the person doing it.
	dash := f.visit(fresh, f.server.URL+"/dashboard")
	dashBody, _ := io.ReadAll(dash.Body)
	_ = dash.Body.Close()
	if dash.StatusCode != http.StatusOK {
		t.Fatalf("the dashboard answered %d to a session minted through OIDC:\n%s",
			dash.StatusCode, dashBody)
	}

	// The session an operator sees is the one the sign-in form would have made:
	// where the browser was and what it was, recorded by the host because the host
	// is what has them — no ABI record carries either. Addressed by the session's
	// own id, because the owner also signed in with a password to make the link, so
	// this account has two sessions and a query that took either would be a query
	// that passed for the wrong one.
	var prefix, agent *string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT ip_prefix::text, user_agent FROM sessions WHERE id = $1`,
		id.SessionID).Scan(&prefix, &agent); err != nil {
		t.Fatal(err)
	}
	if prefix == nil || *prefix == "" {
		t.Error("the minted session has no ip_prefix, so it is not one an operator can " +
			"recognise in the list on their own account page")
	}
	if agent == nil || !strings.Contains(*agent, "linkctrl-integration") {
		t.Errorf("the minted session's user_agent is %q, and the browser sent "+
			"linkctrl-integration/1.0", strOrEmpty(agent))
	}

	// --- provenance -----------------------------------------------------------
	//
	// The instance-wide record that says a session was minted on an add-on's word,
	// naming which add-on and which provider — and carrying neither the external
	// subject nor the address the assertion arrived with, which is what makes the
	// erasure sweep's coverage true.
	var meta []byte
	if err := f.pool.QueryRow(t.Context(),
		`SELECT metadata FROM audit_logs WHERE action = $1`,
		audit.ActionSessionMintedByAddon).Scan(&meta); err != nil {
		t.Fatalf("no audit record for a session minted through OIDC: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(meta, &record); err != nil {
		t.Fatal(err)
	}
	if record["addon"] != "oidc" {
		t.Errorf("the audit record names the add-on as %v", record["addon"])
	}
	if record["issuer"] != oidcIssuer {
		t.Errorf("the audit record names the issuer as %v", record["issuer"])
	}
	if strings.Contains(string(meta), subject) {
		t.Errorf("the audit record carries the external subject:\n%s", meta)
	}
	if strings.Contains(string(meta), oidcPersonEmail) {
		t.Errorf("the audit record carries the address the assertion asserted:\n%s", meta)
	}
}

// The add-on consumes the published SDK, at a version nobody can move.
//
// m69.md's third bullet, as amended on 2026-08-25: a Go pseudo-version is
// immutable and publicly resolvable, which is what the bullet is for; the word
// *tagged* is what it does not satisfy, and M70 pays that by bumping the add-on to
// v0.4.0 at the close.
//
// Read off the go.mod `make oidc-fixture` copied out of the module proxy's own
// cache — so what is asserted is the go.mod of the module that was published,
// not a file in a checkout beside this one. That the version resolves *at all* is
// asserted by the fixture existing: it was fetched from the proxy and built.
func TestTheOIDCAddonConsumesOnlyThePublishedSDK(t *testing.T) {
	gomod := string(mustFixtureFile(t, filepath.Join(oidcFixtureDir, "go.mod")))

	if !strings.Contains(gomod, "module github.com/DevOfPie/LinkCtrl-OIDC") {
		t.Errorf("the add-on's module path is not the published one:\n%s", gomod)
	}
	// No replace, no exclude: either would mean the module that builds is not the
	// module the proxy serves, which is the whole of what "no fork" asks.
	for _, forbidden := range []string{"replace ", "exclude "} {
		if strings.Contains(gomod, forbidden) {
			t.Errorf("the add-on's go.mod carries a %q directive:\n%s", forbidden, gomod)
		}
	}

	sdkLine := ""
	for _, line := range strings.Split(gomod, "\n") {
		if strings.Contains(line, "github.com/DevOfPie/LinkCtrl ") {
			sdkLine = strings.TrimSpace(line)
		}
	}
	if sdkLine == "" {
		t.Fatalf("the add-on does not require github.com/DevOfPie/LinkCtrl at all:\n%s", gomod)
	}
	if strings.Contains(sdkLine, "// indirect") {
		t.Errorf("the SDK is an indirect requirement, so something else is pulling it "+
			"in: %s", sdkLine)
	}
	// One requirement, and it is this product. Anything else in the graph would be
	// a dependency the ABI does not permit an add-on to reach through.
	for _, line := range strings.Split(gomod, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "require") && !strings.Contains(line, "github.com/") {
			continue
		}
		if strings.Contains(line, "github.com/DevOfPie/LinkCtrl") {
			continue
		}
		t.Errorf("the add-on requires something other than the SDK: %q", line)
	}
}

// The add-on may be used, which is a precondition rather than a courtesy.
//
// m69.md's owed-work #4: an unlicensed add-on repository means nobody may use the
// worked example this milestone exists to produce, and this tree's part is to
// check it and refuse to close while it is absent. Read off the LICENSE the
// module proxy served for the *pinned* version, so it is a fact about what was
// published rather than about a checkout beside this one.
func TestTheOIDCAddonMayBeUsed(t *testing.T) {
	licence := string(mustFixtureFile(t, filepath.Join(oidcFixtureDir, "LICENSE")))
	if !strings.Contains(licence, "MIT License") {
		t.Errorf("the add-on's LICENSE is not MIT:\n%s", licence)
	}
	if !strings.Contains(licence, "Permission is hereby granted, free of charge") {
		t.Errorf("the add-on's LICENSE carries no grant:\n%s", licence)
	}
}

// --- small readers ------------------------------------------------------------

func sessionCookieIn(cookies []*http.Cookie) bool { return sessionCookieValue(cookies) != "" }

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func sessionCookieValue(cookies []*http.Cookie) string {
	for _, c := range cookies {
		if c.Name == auth.CookieName(false) && c.Value != "" {
			return c.Value
		}
	}
	return ""
}
