//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// webFixture drives the HTML dashboard the way a browser does: a cookie jar,
// form-encoded posts, and no transparent redirect following — every 303 is
// asserted, not skipped over.
type webFixture struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	pool   *pgxpool.Pool
}

func newWeb(t *testing.T) *webFixture { return newWebOn(t, newDB(t)) }

// newWebOn is newWeb against a database the caller made, for the tests that need
// something attached to the pool — a query tracer, say — before the app uses it.
func newWebOn(t *testing.T, pool *pgxpool.Pool) *webFixture {
	t.Helper()

	cfg := config.Config{
		AppEnv:        config.Development,
		BaseURL:       "http://links.test",
		SecureCookies: false,
		DocsEnabled:   true,
	}
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
		// The shipped default, wired as main.go wires it. Left unset this is
		// zero, which the service reads as "no lockout" — so a fixture without
		// it lets a test about what a locked account answers pass by never
		// locking one (finding F92).
		Lockout: auth.LockoutPolicy{Threshold: 5, Window: 15 * time.Minute},
	})
	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}
	linkSvc := link.NewService(pool, link.Config{
		Policy:  link.DefaultDestinationPolicy(),
		BaseURL: cfg.BaseURL,
	})
	stats := analytics.NewReader(pool)
	notifySvc := notify.NewService(pool)

	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg,
		Health: &httpx.Health{DB: pool},
		Auth:   authSvc,
		Keys:   keySvc,
		Links:  linkSvc,
		Stats:  stats,
		Notify: notifySvc,
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc, Keys: keySvc,
			Links: linkSvc, Stats: stats, Notify: notifySvc,
		},
	}))
	t.Cleanup(srv.Close)

	jar, _ := newCookieJar()
	return &webFixture{
		t:      t,
		server: srv,
		pool:   pool,
		client: &http.Client{
			Jar: jar,
			// Redirects are the assertions in these tests; following them
			// silently would hide a wrong Location behind a plausible 200.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (f *webFixture) get(path string, hdr map[string]string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *webFixture) postForm(path string, vals url.Values, hdr map[string]string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost,
		f.server.URL+path, strings.NewReader(vals.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *webFixture) body(resp *http.Response) string {
	f.t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *webFixture) wantRedirect(resp *http.Response, to string) {
	f.t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != to {
		f.t.Fatalf("Location = %q, want %q", loc, to)
	}
}

const webPassword = "a-sufficiently-long-password"

// claim runs the first-run setup form, leaving the client signed in.
func (f *webFixture) claim() {
	f.t.Helper()
	resp := f.postForm("/setup", url.Values{
		"name": {"Owner"}, "email": {"owner@example.com"}, "password": {webPassword},
	}, nil)
	f.wantRedirect(resp, "/dashboard")
}

// createLink makes a link through the form and returns its detail path.
func (f *webFixture) createLink(dest, alias string) string {
	f.t.Helper()
	resp := f.postForm("/links", url.Values{"url": {dest}, "alias": {alias}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("create link returned %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/links/") {
		f.t.Fatalf("create link redirected to %q", loc)
	}
	return strings.SplitN(loc, "?", 2)[0]
}

func TestWebSetupFlow(t *testing.T) {
	f := newWeb(t)

	// A fresh instance walks the operator to the setup form.
	f.wantRedirect(f.get("/", nil), "/login")
	f.wantRedirect(f.get("/login", nil), "/setup")

	page := f.body(f.get("/setup", nil))
	if !strings.Contains(page, "Create account") {
		t.Fatal("setup page did not render the form")
	}

	f.claim()

	// Signed in and landed.
	dash := f.body(f.get("/dashboard", nil))
	if !strings.Contains(dash, "owner@example.com") {
		t.Error("dashboard does not show the signed-in user")
	}

	// The setup page is gone the moment it is used.
	f.wantRedirect(f.get("/setup", nil), "/login")

	// And "/" now lands on the dashboard.
	f.wantRedirect(f.get("/", nil), "/dashboard")
}

func TestWebLoginAndLogout(t *testing.T) {
	f := newWeb(t)
	f.claim()
	resp := f.postForm("/logout", nil, nil)
	f.wantRedirect(resp, "/login?signedout=1")

	// Signed out: protected pages bounce to login with a way back.
	f.wantRedirect(f.get("/links", nil), "/login?next=/links")

	// Wrong password re-renders the form with one generic message.
	fail := f.postForm("/login", url.Values{
		"email": {"owner@example.com"}, "password": {"wrong-but-long-enough"},
	}, nil)
	if fail.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login returned %d, want 401", fail.StatusCode)
	}
	if !strings.Contains(f.body(fail), "email or password is incorrect") {
		t.Error("bad login did not explain itself")
	}

	// Right password honours the next param.
	ok := f.postForm("/login", url.Values{
		"email": {"owner@example.com"}, "password": {webPassword}, "next": {"/links"},
	}, nil)
	f.wantRedirect(ok, "/links")

	// An external next must not turn the login form into an open redirect.
	_ = f.body(f.postForm("/logout", nil, nil))
	evil := f.postForm("/login", url.Values{
		"email": {"owner@example.com"}, "password": {webPassword}, "next": {"//evil.example/phish"},
	}, nil)
	f.wantRedirect(evil, "/dashboard")
}

func TestWebLinkLifecycle(t *testing.T) {
	f := newWeb(t)
	f.claim()

	detail := f.createLink("https://example.com/launch", "webflow")

	page := f.body(f.get(detail+"?created=1", nil))
	if !strings.Contains(page, "/webflow") || !strings.Contains(page, "Link created") {
		t.Fatal("detail page did not show the new link")
	}

	list := f.body(f.get("/links", nil))
	if !strings.Contains(list, "/webflow") {
		t.Error("list page does not show the link")
	}

	// Edit through the form: full update, tags split from the comma field.
	resp := f.postForm(detail, url.Values{
		"url": {"https://example.com/launch-v2"}, "alias": {"webflow"},
		"title": {"Launch page"}, "description": {""},
		"expires_at": {""}, "tags": {"launch, marketing"},
	}, nil)
	f.wantRedirect(resp, detail+"?saved=1")

	page = f.body(f.get(detail, nil))
	for _, want := range []string{"Launch page", "launch-v2", "launch", "marketing"} {
		if !strings.Contains(page, want) {
			t.Errorf("after edit, detail page is missing %q", want)
		}
	}

	// A dangerous destination re-renders the form with the field error.
	bad := f.postForm("/links", url.Values{"url": {"https://169.254.169.254/meta"}}, nil)
	if bad.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("dangerous destination returned %d, want 422", bad.StatusCode)
	}
	if !strings.Contains(f.body(bad), "url") {
		t.Error("validation failure did not mark the url field")
	}

	// Archive, then the page offers Restore.
	f.wantRedirect(f.postForm(detail+"/archive", nil, nil), detail+"?archived=1")
	page = f.body(f.get(detail, nil))
	if !strings.Contains(page, "archived") || !strings.Contains(page, "Restore") {
		t.Error("archived link does not offer restore")
	}

	// Delete lands on the list with the trash notice, and the row is gone.
	f.wantRedirect(f.postForm(detail+"/delete", nil, nil), "/links?deleted=1")
	list = f.body(f.get("/links?deleted=1", nil))
	if strings.Contains(list, "/webflow") {
		t.Error("deleted link still listed")
	}
	// The notice must describe the window without implying a restore button:
	// there is no trash view in Phase 1 and RestoreLink refuses soft-deleted
	// rows, so "restorable for 30 days" sent people looking for one.
	if !strings.Contains(list, "alias stays reserved for 30 days") {
		t.Error("delete notice missing")
	}
	if strings.Contains(list, "restorable") {
		t.Error("delete notice still promises the link is restorable, which no " +
			"product surface offers")
	}
}

func TestWebSearchPartialViaHTMX(t *testing.T) {
	f := newWeb(t)
	f.claim()
	f.createLink("https://example.com/a", "alpha")

	// An htmx search swaps just the table: a fragment, not a page.
	frag := f.body(f.get("/links?search=alpha", map[string]string{"HX-Request": "true"}))
	if strings.Contains(frag, "<!doctype html>") {
		t.Fatal("htmx request rendered the whole page; the swap would nest a page inside the page")
	}
	if !strings.Contains(frag, `id="links-table"`) || !strings.Contains(frag, "/alpha") {
		t.Error("fragment is missing the table or the match")
	}

	// The same URL without the htmx header renders the full page, which is
	// what hx-push-url depends on.
	full := f.body(f.get("/links?search=alpha", nil))
	if !strings.Contains(full, "<!doctype html>") {
		t.Error("plain navigation did not render the full page")
	}

	// Signed out, an htmx request gets HX-Redirect rather than a login page
	// swapped into the table.
	_ = f.body(f.postForm("/logout", nil, nil))
	resp := f.get("/links", map[string]string{"HX-Request": "true"})
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("HX-Redirect") == "" {
		t.Error("htmx request while signed out got no HX-Redirect")
	}
}

func TestWebCrossOriginFormsAreRejected(t *testing.T) {
	f := newWeb(t)
	f.claim()

	for name, hdr := range map[string]map[string]string{
		"foreign origin": {"Origin": "https://evil.example"},
		"sec-fetch-site": {"Sec-Fetch-Site": "cross-site"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := f.postForm("/links", url.Values{"url": {"https://example.com/csrf"}}, hdr)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("cross-site post returned %d, want 403", resp.StatusCode)
			}
		})
	}

	// Nothing was created by either attempt.
	var n int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM links WHERE primary_url = 'https://example.com/csrf'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d link(s) created by cross-site posts", n)
	}

	// A same-origin post still works: the protection must not lock the real
	// user out.
	resp := f.postForm("/links", url.Values{"url": {"https://example.com/fine"}},
		map[string]string{"Origin": f.server.URL})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-origin post returned %d, want 303", resp.StatusCode)
	}
}

var keyTokenPattern = regexp.MustCompile(`lk_live_[a-z2-7]{8}_[A-Za-z0-9_-]{43}`)

func TestWebKeysLifecycle(t *testing.T) {
	f := newWeb(t)
	f.claim()

	page := f.body(f.get("/keys", nil))
	if !strings.Contains(page, "Create a key") {
		t.Fatal("keys page did not render the create form")
	}

	// Create shows the token exactly once, in the direct response.
	created := f.postForm("/keys", url.Values{
		"name": {"dashboard-test"}, "scopes": {"links.read"},
	}, nil)
	if created.StatusCode != http.StatusOK {
		t.Fatalf("create key returned %d", created.StatusCode)
	}
	html := f.body(created)
	token := keyTokenPattern.FindString(html)
	if token == "" {
		t.Fatal("created page does not contain the key token")
	}

	// The token authenticates against the API — the same key, two surfaces.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/api/v1/links", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	apiResp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusOK {
		t.Fatalf("key minted in the dashboard returned %d from the API", apiResp.StatusCode)
	}

	// A reload of the list no longer shows the token.
	page = f.body(f.get("/keys", nil))
	if strings.Contains(page, token) {
		t.Error("the key list still shows the full token; it must appear exactly once")
	}
	if !strings.Contains(page, "dashboard-test") {
		t.Error("the key list does not show the new key")
	}

	// Revoke through the form; the key dies on its next API request.
	// The prefix is the fixed-length public part — never found by splitting on
	// "_", which the base64url secret can legitimately contain.
	var keyID string
	prefix := token[:auth.APIKeyPrefixLength]
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM api_keys WHERE prefix = $1`, prefix).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	f.wantRedirect(f.postForm("/keys/"+keyID+"/revoke", nil, nil), "/keys?revoked=1")

	req2, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/api/v1/links", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	apiResp2, err := (&http.Client{}).Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiResp2.Body.Close()
	if apiResp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked key returned %d from the API, want 401", apiResp2.StatusCode)
	}
}

func TestWebPasswordChange(t *testing.T) {
	f := newWeb(t)
	f.claim()

	const newPassword = "an-even-longer-password-42"

	// Mismatched confirmation is caught before anything changes.
	miss := f.postForm("/account/password", url.Values{
		"current_password": {webPassword},
		"new_password":     {newPassword},
		"confirm_password": {"something-else-entirely"},
	}, nil)
	if miss.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched confirmation returned %d, want 422", miss.StatusCode)
	}
	if !strings.Contains(f.body(miss), "do not match") {
		t.Error("mismatch message missing")
	}

	ok := f.postForm("/account/password", url.Values{
		"current_password": {webPassword},
		"new_password":     {newPassword},
		"confirm_password": {newPassword},
	}, nil)
	f.wantRedirect(ok, "/account?changed=1")

	// This browser's session survives the change.
	still := f.get("/account?changed=1", nil)
	if still.StatusCode != http.StatusOK {
		t.Fatalf("session died with the password change: %d", still.StatusCode)
	}
	_ = still.Body.Close()

	// The old password is dead, the new one works.
	_ = f.body(f.postForm("/logout", nil, nil))
	old := f.postForm("/login", url.Values{
		"email": {"owner@example.com"}, "password": {webPassword},
	}, nil)
	if old.StatusCode != http.StatusUnauthorized {
		t.Errorf("old password still logs in: %d", old.StatusCode)
	}
	_ = old.Body.Close()
	f.wantRedirect(f.postForm("/login", url.Values{
		"email": {"owner@example.com"}, "password": {newPassword},
	}, nil), "/dashboard")
}

func TestDocsPage(t *testing.T) {
	f := newWeb(t)

	// Public by default: no session required, per the documented choice.
	page := f.get("/docs", nil)
	body := f.body(page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("/docs returned %d", page.StatusCode)
	}
	for _, want := range []string{"swagger-ui", "vendor/swagger-ui-bundle.js", "js/docs.js", "/api/v1/openapi.json"} {
		if !strings.Contains(body, want) {
			t.Errorf("/docs page is missing %q", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("/docs contains an inline script; the CSP forbids it and the browser would refuse to boot the viewer")
	}

	// The one-page CSP waiver: inline styles allowed here, scripts still not.
	csp := page.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Errorf("/docs CSP does not permit inline styles; Swagger UI renders unstyled: %q", csp)
	}
	if !strings.Contains(csp, "script-src 'self';") {
		t.Errorf("/docs CSP relaxed scripts too: %q", csp)
	}

	// The spec endpoints serve both forms, anonymously.
	spec := f.get("/api/v1/openapi.json", nil)
	specBody := f.body(spec)
	if spec.StatusCode != http.StatusOK {
		t.Fatalf("openapi.json returned %d", spec.StatusCode)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct{ Title string }
	}
	if err := json.Unmarshal([]byte(specBody), &doc); err != nil || doc.OpenAPI == "" {
		t.Errorf("openapi.json is not the spec: %v", err)
	}

	// Swagger UI's assets come fingerprinted from the same static pipeline.
	css := f.get("/static/vendor/swagger-ui.css", nil)
	_ = f.body(css)
	if css.StatusCode != http.StatusOK {
		t.Errorf("swagger css returned %d", css.StatusCode)
	}
}

func TestWebStaticAssetsAndHeaders(t *testing.T) {
	f := newWeb(t)

	// Assets are public: no session, no redirect to login.
	css := f.get("/static/css/app.css", nil)
	defer func() { _ = css.Body.Close() }()
	if css.StatusCode != http.StatusOK {
		t.Fatalf("stylesheet returned %d; run `make css` before the integration suite", css.StatusCode)
	}
	if ct := css.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("stylesheet Content-Type = %q", ct)
	}

	// Pages carry the strict CSP and are never cached. On a fresh instance
	// /login redirects to /setup, so the rendered page to inspect is /setup.
	login := f.get("/setup", nil)
	defer func() { _ = login.Body.Close() }()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("setup page returned %d", login.StatusCode)
	}
	csp := login.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP missing or weak: %q", csp)
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP contains an unsafe- waiver: %q", csp)
	}
	if cc := login.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("page Cache-Control = %q, want no-store", cc)
	}
}

// TestTheLinkPageNamesTheDomainTheLinkIsServedOn is F89's second half.
//
// The bot control's explanation read `DomainSettings` — the instance default's
// row — for every link, whatever hostname the link is actually served on. So a
// link on a verified custom hostname (M40) had a control enabled or disabled by
// another domain's policy, and the sentence beneath it named a hostname the link
// is not served on. The API's own refusal has always read the link's own domain
// (`Update`, via `GetDomainBotSettings`), so the two surfaces m32.5.md says are
// "asserted by test at both surfaces" disagreed.
//
// The hostname is what this asserts rather than the policy, because the policy is
// instance-wide and therefore the same on both rows by construction — the
// hostname is the part that differs on a state this product actually produces.
func TestTheLinkPageNamesTheDomainTheLinkIsServedOn(t *testing.T) {
	f := newWeb(t)
	f.claim()
	path := f.createLink("https://example.com/onhost", "onhost")

	// A verified hostname owned by this workspace, and the link moved onto it.
	// Written directly because this test is about which row the page reads, and
	// registering and verifying a hostname is custom_domains_test.go's subject.
	const hostname = "go.page.test"
	var linkID, workspaceID, orgID uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT l.id, l.workspace_id, w.organization_id
		   FROM links l JOIN workspaces w ON w.id = l.workspace_id
		  WHERE l.alias = 'onhost'`).
		Scan(&linkID, &workspaceID, &orgID); err != nil {
		t.Fatalf("read the link: %v", err)
	}
	// Both owner columns, because domains_ownership_states refuses a workspace
	// without the organization it implies.
	if _, err := f.pool.Exec(t.Context(), `
		WITH d AS (
		    INSERT INTO domains (id, organization_id, workspace_id, hostname,
		                         verification_token, verified_at, ssl_status)
		    VALUES (gen_random_uuid(), $1, $2, $3, 'tok', now(), 'active')
		    RETURNING id
		)
		UPDATE links SET domain_id = (SELECT id FROM d) WHERE id = $4`,
		orgID, workspaceID, hostname, linkID); err != nil {
		t.Fatalf("put the link on a custom hostname: %v", err)
	}

	body := f.body(f.get(path, nil))
	if !strings.Contains(body, hostname) {
		t.Errorf("the link detail page does not name %q, the hostname the link is "+
			"served on", hostname)
	}
	// 'default' is the placeholder 00700 seeds the instance default with, and it
	// is what the page named for every link.
	if strings.Contains(body, "every link on default") ||
		strings.Contains(body, "because default ") ||
		strings.Contains(body, "what default does today") {
		t.Errorf("the link detail page explains the bot control in terms of the "+
			"instance default domain for a link served on %q", hostname)
	}
}
