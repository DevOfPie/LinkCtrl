//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/account"
	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/recovery"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
	"github.com/DevOfPie/LinkCtrl/internal/store"
	"github.com/DevOfPie/LinkCtrl/internal/team"
)

// apiFixture is a running server plus a cookie-jar client, so tests drive the
// real HTTP surface rather than calling services directly. Middleware, routing,
// JSON shape and status codes are all in scope.
type apiFixture struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	pool   *pgxpool.Pool
	auth   *auth.Service
	keys   *auth.APIKeyService
}

func newAPI(t *testing.T) *apiFixture {
	t.Helper()
	pool, dsn := newDBWithDSN(t)

	cfg := config.Config{
		AppEnv:        config.Development,
		BaseURL:       "http://links.test",
		SecureCookies: false,
		DocsEnabled:   true,
	}
	cfg.Auth.SignupMode = config.SignupOpen
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
	})
	linkSvc := link.NewService(pool, link.Config{
		Policy:  link.DefaultDestinationPolicy(),
		BaseURL: cfg.BaseURL,
		// Wired as main.go wires it, since M30. A blocked destination writes an
		// audit record, and a fixture without a recorder would let the tests that
		// read that record pass by never producing one.
		Audit: audit.NewService(pool),
		// The gates (M35), for the same reason: without them a link password is
		// refused for want of a hasher and a signed URL cannot be minted, so the
		// contract test would be replaying a surface this fixture disabled.
		Hasher: authSvc.Hasher(),
		Gates:  gate.NewService(pool, gate.Config{Hasher: authSvc.Hasher()}),
	})

	// Not started: the usage tracker's ticker is not wanted in tests, and the
	// tests that care about last_used_at call FlushUsage directly rather than
	// sleeping through an interval.
	// The auditor is wired as main.go wires it (M44): rotation is the one key
	// operation no human is present for, so it is the one that writes a record,
	// and a fixture without a recorder would let the test that reads that record
	// pass by never producing one.
	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{
		Pepper: testPepper, Auditor: audit.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A mailer, unlike most fixtures. It is what makes `open` reachable: with
	// no relay the effective signup mode drops to `invite` (D1) and no
	// registration is possible, so the contract test could not exercise the
	// endpoint at all.
	mailSvc := newMailService(t, pool, &recordingSender{})

	signupSvc, err := signup.NewService(pool, signup.Config{
		Mode:   signup.Mode(cfg.Auth.SignupMode),
		AppURL: cfg.AppOrigin(),
		Hasher: authSvc.Hasher(),
		Mail:   mailSvc,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Account recovery (M51). Wired because the mailer above exists: with none
	// its two endpoints refuse everything with 503, and the contract test would
	// be replaying a refusal instead of the operation.
	recoverySvc, err := recovery.NewService(pool, recovery.Config{
		AppURL: cfg.AppOrigin(),
		Hasher: authSvc.Hasher(),
		Mail:   mailSvc,
		Audit:  audit.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}

	inviteSvc, err := invite.NewService(pool, invite.Config{
		AppURL:      cfg.AppOrigin(),
		TTL:         168 * time.Hour,
		NewAccounts: signupSvc.Effective().AdmitsNewAccounts(),
		Hasher:      authSvc.Hasher(),
		Audit:       audit.NewService(pool),
		Notify:      notify.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}

	teamSvc := team.NewService(pool, team.Config{Audit: audit.NewService(pool)})

	// The appeal path for a blocked destination (M31). Wired as main.go wires
	// it, with the link service as its judge: which tier refused a destination
	// has one answer in this program, and a second evaluator here would be a
	// second answer waiting to disagree.
	disputeSvc, err := dispute.NewService(pool, dispute.Config{
		Judge:  linkSvc,
		Audit:  audit.NewService(pool),
		Notify: notify.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The instance-level principal's roster (M45, D98). Wired as main.go wires
	// it: the account this fixture creates claims the instance through
	// /auth/setup, so it is the principal, and without this service its two
	// endpoints would be unregistered and the contract test would report them as
	// spec operations nothing exercises.
	instanceSvc := instance.NewService(pool, instance.Config{Audit: audit.NewService(pool)})

	// Account deletion and erasure (M52). Wired for the same reason as the
	// roster above: without it the endpoint is unregistered and the contract
	// test reports a spec operation nothing exercises.
	accountSvc, err := account.NewService(pool, account.Config{
		Auth: authSvc, Audit: audit.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The second factor (M53). Wired with a real cipher for the reason the
	// mailer above is real: with none, every enrolment endpoint answers 503 and
	// the contract test would be replaying a refusal instead of the operation.
	mfaCipher, err := auth.NewMFACipher(testMFAKey)
	if err != nil {
		t.Fatal(err)
	}
	mfaSvc, err := auth.NewMFAService(pool, auth.MFAConfig{
		Auth: authSvc, Cipher: mfaCipher, Issuer: "linkctrl.test",
		Audit: audit.NewService(pool), Notify: notify.NewService(pool),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The add-on host (M67, and M68's manager). An empty directory, **the fixture's
	// own database**, and **its DSN**, and each of the three is load-bearing for a
	// different replay.
	//
	// The pool is what M68 first forced: the manager's settings write lands in
	// `addon_settings`, a host table in this database rather than anything the
	// add-on owns, and without a pool that write answers `503` and the spec's `200`
	// describes nothing.
	//
	// The DSN is what an add-on that owns a *schema* connects with as its own
	// confined role, and it is here so the contract fixture can install one. Without
	// it `openStorage` logs and returns nil, no schema is created, a removal leaves
	// nothing behind, and `GET /addons/orphaned-data` can only ever answer `[]` —
	// so `AddonOrphan` and the `200` on `purgeAddonData` would both be validated
	// against a document nothing produces. That is the whole reason the fixture's
	// module declares storage.
	//
	// Without a host at all the operations are unregistered and the contract test
	// reports them as spec operations nothing exercises, which is the same reason
	// every service above is wired.
	addonHost, err := addon.Open(context.Background(), addon.Options{
		Dir:   t.TempDir(),
		DB:    pool,
		DSN:   dsn,
		Audit: audit.NewService(pool),
	})
	if err != nil {
		t.Fatalf("open an add-on host: %v", err)
	}
	t.Cleanup(func() { _ = addonHost.Close(context.Background()) })
	// The database goes with the fixture; the login role does not, because a role is
	// a cluster object and this one outlives the database it owned a schema in. Best
	// effort and after the pool is closed — a `DROP ROLE` that fails leaves a role
	// whose password the next `EnsureAddonSchema` resets anyway, so the tidying is
	// worth doing and not worth failing a test over.
	t.Cleanup(func() {
		cleanup, err := pgxpool.New(context.Background(), dsnFor("postgres"))
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(),
			fmt.Sprintf("DROP ROLE IF EXISTS %s", store.AddonSchema(contractAddonName)))
	})

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config:     cfg,
		Health:     &httpx.Health{DB: pool},
		Auth:       authSvc,
		Keys:       keySvc,
		Links:      linkSvc,
		Stats:      analytics.NewReader(pool),
		Audit:      audit.NewService(pool),
		Notify:     notify.NewService(pool),
		Invites:    inviteSvc,
		Team:       teamSvc,
		Signup:     signupSvc,
		Recovery:   recoverySvc,
		Accounts:   accountSvc,
		MFA:        mfaSvc,
		Disputes:   disputeSvc,
		Instance:   instanceSvc,
		AddonAdmin: addonHost,
	}))
	t.Cleanup(srv.Close)

	jar, _ := newCookieJar()
	return &apiFixture{
		t:      t,
		server: srv,
		client: &http.Client{Jar: jar},
		pool:   pool,
		auth:   authSvc,
		keys:   keySvc,
	}
}

func (f *apiFixture) do(method, path string, body any) *http.Response {
	f.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, f.server.URL+path, rdr)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func (f *apiFixture) decode(resp *http.Response, dst any) {
	f.t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if dst == nil {
		return
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		f.t.Fatalf("decode response: %v", err)
	}
}

// registerAccount makes a second account on this instance, through the service
// rather than through the signup form.
//
// The form is deliberately not used: with a mailer configured, self-serve
// registration writes a pending row and answers 202, and the account only exists
// once the emailed link is followed (M29). Walking that here would be replaying
// the registration surface inside a test about a different one. The service call
// is the same one the verification handler makes.
func (f *apiFixture) registerAccount(email string) {
	f.t.Helper()
	if _, err := f.auth.Register(f.t.Context(), auth.RegisterInput{
		Email: email, Name: email, Password: "a-sufficiently-long-password",
	}); err != nil {
		f.t.Fatalf("register %s: %v", email, err)
	}
}

// setupOwner claims the instance and leaves the client signed in.
func (f *apiFixture) setupOwner() {
	f.t.Helper()
	resp := f.do(http.MethodPost, "/api/v1/auth/setup", map[string]string{
		"email": "owner@example.com", "name": "Owner", "password": "a-sufficiently-long-password",
	})
	if resp.StatusCode != http.StatusCreated {
		f.t.Fatalf("setup returned %d", resp.StatusCode)
	}
	f.decode(resp, nil)

	resp = f.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "a-sufficiently-long-password",
	})
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("login returned %d", resp.StatusCode)
	}
	f.decode(resp, nil)
}

func TestAPIRequiresAuthentication(t *testing.T) {
	f := newAPI(t)

	// Every protected endpoint must reject an anonymous caller. Checking them
	// as a set catches a route added later without RequireAuth.
	endpoints := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/me"},
		{http.MethodGet, "/api/v1/links"},
		{http.MethodPost, "/api/v1/links"},
		{http.MethodGet, "/api/v1/tags"},
	}
	for _, e := range endpoints {
		t.Run(e.method+" "+e.path, func(t *testing.T) {
			resp := f.do(e.method, e.path, map[string]string{})
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}
		})
	}
}

func TestSetupIsSingleUse(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Once claimed, the endpoint must disappear. A second call creating
	// another owner would be a trivial takeover of a fresh instance.
	resp := f.do(http.MethodPost, "/api/v1/auth/setup", map[string]string{
		"email": "attacker@example.com", "password": "a-sufficiently-long-password",
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second setup returned %d, want 404", resp.StatusCode)
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	f := newAPI(t)
	resp := f.do(http.MethodPost, "/api/v1/auth/setup", map[string]string{
		"email": "owner@example.com", "password": "a-sufficiently-long-password",
	})
	f.decode(resp, nil)

	resp = f.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "a-sufficiently-long-password",
	})
	defer func() { _ = resp.Body.Close() }()

	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName(false) {
			session = c
		}
	}
	if session == nil {
		t.Fatal("login set no session cookie")
	}

	// These regress silently: a missing HttpOnly is invisible until an XSS
	// turns into full account takeover.
	if !session.HttpOnly {
		t.Error("session cookie is not HttpOnly; script could read it")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", session.SameSite)
	}
	if session.Path != "/" {
		t.Errorf("Path = %q, want /", session.Path)
	}
	if session.Domain != "" {
		t.Errorf("Domain = %q, want empty so the cookie is host-only", session.Domain)
	}
}

func TestLinkLifecycleThroughAPI(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Create.
	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/target", "title": "Example", "tags": []string{"docs", "public"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create returned %d", resp.StatusCode)
	}
	var created struct {
		ID       string                  `json:"id"`
		Alias    string                  `json:"alias"`
		ShortURL string                  `json:"short_url"`
		URL      string                  `json:"url"`
		Status   string                  `json:"status"`
		Tags     []struct{ Name string } `json:"tags"`
	}
	f.decode(resp, &created)

	if created.Alias == "" || created.ID == "" {
		t.Fatal("create returned no alias or id")
	}
	if created.ShortURL != "http://links.test/"+created.Alias {
		t.Errorf("short_url = %q, want it built from the configured base URL", created.ShortURL)
	}
	if len(created.Tags) != 2 {
		t.Errorf("got %d tags, want 2 created implicitly", len(created.Tags))
	}

	// Read back.
	resp = f.do(http.MethodGet, "/api/v1/links/"+created.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get returned %d", resp.StatusCode)
	}
	f.decode(resp, nil)

	// Edit the destination without changing the alias. This is the core
	// product promise, so it gets an explicit assertion.
	resp = f.do(http.MethodPatch, "/api/v1/links/"+created.ID, map[string]any{
		"url": "https://example.org/moved",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch returned %d", resp.StatusCode)
	}
	var updated struct {
		Alias string `json:"alias"`
		URL   string `json:"url"`
	}
	f.decode(resp, &updated)
	if updated.URL != "https://example.org/moved" {
		t.Errorf("url = %q, want the new destination", updated.URL)
	}
	if updated.Alias != created.Alias {
		t.Errorf("alias changed from %q to %q; editing a destination must not "+
			"change the short URL", created.Alias, updated.Alias)
	}

	// The denormalized column the hot path reads must have been updated by the
	// trigger, not left stale.
	var primaryURL string
	if err := f.pool.QueryRow(t.Context(),
		"SELECT primary_url FROM links WHERE id = $1", created.ID).Scan(&primaryURL); err != nil {
		t.Fatal(err)
	}
	if primaryURL != "https://example.org/moved" {
		t.Errorf("links.primary_url = %q, want it synced by the trigger", primaryURL)
	}

	// Archive, restore, delete.
	resp = f.do(http.MethodPost, "/api/v1/links/"+created.ID+"/archive", nil)
	f.decode(resp, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive returned %d", resp.StatusCode)
	}

	resp = f.do(http.MethodPost, "/api/v1/links/"+created.ID+"/restore", nil)
	f.decode(resp, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore returned %d", resp.StatusCode)
	}

	resp = f.do(http.MethodDelete, "/api/v1/links/"+created.ID, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete returned %d", resp.StatusCode)
	}

	// Soft delete: gone from the API, but the row survives so it can be
	// restored and the alias stays reserved.
	resp2 := f.do(http.MethodGet, "/api/v1/links/"+created.ID, nil)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", resp2.StatusCode)
	}
	var deletedAt *time.Time
	if err := f.pool.QueryRow(t.Context(),
		"SELECT deleted_at FROM links WHERE id = $1", created.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("row was hard-deleted; it should be recoverable: %v", err)
	}
	if deletedAt == nil {
		t.Error("deleted_at not set")
	}
}

func TestCreateRejectsDangerousDestinations(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	for _, bad := range []string{
		"javascript:alert(1)",
		"http://10.0.0.1/admin",
		"file:///etc/passwd",
		"not-a-url",
	} {
		t.Run(bad, func(t *testing.T) {
			resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{"url": bad})
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", resp.StatusCode)
			}
			var p httpx.Problem
			if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			// Field-level detail, so a form can highlight the input.
			if len(p.Errors) == 0 || p.Errors[0].Field != "url" {
				t.Errorf("problem = %+v, want a field error against url", p)
			}
		})
	}
}

// The gate fields used to answer 422 `not_implemented` here, and M35 is the
// milestone that made that answer false. What replaces it is not "they are
// accepted" — that is TestGateFieldsAreAcceptedAndReported, in gates_test.go,
// where the fixture has the gate service wired. What is left here is the half
// this file is about: the contract's *shape*, and the one refusal that is still
// a refusal.
func TestGateFieldsAreAcceptedByTheContract(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// A link with no gates asked for is still a link with no gates, and reports
	// as such. This is the case a regression would break silently: a default
	// that flipped to "on" would gate every link on the instance.
	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/plain",
	})
	var plain struct {
		HasPassword      bool   `json:"has_password"`
		MaxClicks        *int64 `json:"max_clicks"`
		OneTime          bool   `json:"one_time"`
		RequireSignature bool   `json:"require_signature"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("plain create = %d, want 201", resp.StatusCode)
	}
	f.decode(resp, &plain)
	if plain.HasPassword || plain.OneTime || plain.RequireSignature || plain.MaxClicks != nil {
		t.Errorf("a link created with no gates reports %+v; every gate is off "+
			"unless asked for", plain)
	}

	// A password shorter than the account floor is refused, and by name, so a
	// form can put the message beside the box.
	resp = f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com", "password": "short",
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a five-character link password = %d, want 422", resp.StatusCode)
	}
	var p httpx.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if len(p.Errors) == 0 || p.Errors[0].Field != "password" {
		t.Errorf("problem = %+v, want a field error against password", p)
	}
}

func TestCustomAliasValidationAndConflict(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Reserved aliases must be refused, or a link could shadow a real route.
	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com", "alias": "api",
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("reserved alias returned %d, want 422", resp.StatusCode)
	}

	// A valid custom alias works.
	resp = f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com", "alias": "My-Link",
	})
	var first struct{ Alias string }
	f.decode(resp, &first)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("custom alias returned %d", resp.StatusCode)
	}
	if first.Alias != "my-link" {
		t.Errorf("alias = %q, want it canonicalized to lowercase", first.Alias)
	}

	// Reusing it is a conflict, including with different casing.
	resp = f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/other", "alias": "MY-LINK",
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate alias returned %d, want 409", resp.StatusCode)
	}
}

func TestListPaginationSearchAndTagFilter(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	for i := 0; i < 30; i++ {
		tag := "even"
		if i%2 == 1 {
			tag = "odd"
		}
		resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
			"url":   "https://example.com/page" + itoa(i),
			"title": "Page " + itoa(i),
			"tags":  []string{tag},
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("seed %d returned %d", i, resp.StatusCode)
		}
		f.decode(resp, nil)
	}

	// Page through with a cursor, asserting no duplicates and no omissions.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/api/v1/links?limit=7"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		resp := f.do(http.MethodGet, path, nil)
		var page struct {
			Items      []struct{ ID string } `json:"items"`
			NextCursor string                `json:"next_cursor"`
			HasMore    bool                  `json:"has_more"`
		}
		f.decode(resp, &page)

		for _, it := range page.Items {
			if seen[it.ID] {
				t.Fatalf("link %s returned on two pages; keyset pagination is unstable", it.ID)
			}
			seen[it.ID] = true
		}
		pages++
		if !page.HasMore || pages > 10 {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 30 {
		t.Errorf("paged over %d links, want 30", len(seen))
	}

	// Search.
	resp := f.do(http.MethodGet, "/api/v1/links?search=page7", nil)
	var searched struct {
		Items []struct{ Title string } `json:"items"`
	}
	f.decode(resp, &searched)
	if len(searched.Items) == 0 {
		t.Error("search for an existing title returned nothing")
	}

	// A punctuation-only search must mean "no filter", not "match nothing":
	// websearch_to_tsquery yields an empty query for input like "!!".
	resp = f.do(http.MethodGet, "/api/v1/links?search="+url.QueryEscape("!!"), nil)
	var punct struct {
		Items []struct{ ID string } `json:"items"`
	}
	f.decode(resp, &punct)
	if len(punct.Items) == 0 {
		t.Error("a punctuation-only search returned zero results; it should be treated as no filter")
	}

	// Total is opt-in.
	resp = f.do(http.MethodGet, "/api/v1/links?include_total=true&limit=5", nil)
	var withTotal struct {
		Total *int64 `json:"total"`
	}
	f.decode(resp, &withTotal)
	if withTotal.Total == nil || *withTotal.Total != 30 {
		t.Errorf("total = %v, want 30", withTotal.Total)
	}

	resp = f.do(http.MethodGet, "/api/v1/links?limit=5", nil)
	var withoutTotal struct {
		Total *int64 `json:"total"`
	}
	f.decode(resp, &withoutTotal)
	if withoutTotal.Total != nil {
		t.Error("total was returned without include_total; the count should not be paid for by default")
	}
}

// Pagination has to hold under every sort, not just the default. The cursor
// used to compare (created_at, id) whatever the sort was, so a ?sort=clicks
// page filtered on a column it was not ordered by: page 2 dropped links that
// belonged on it and repeated links already shown. Only the default sort was
// ever paged in a test, which is why that survived.
func TestPaginationIsStableUnderEverySort(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	ctx := t.Context()

	const total = 30
	ids := make([]string, 0, total)
	for i := range total {
		resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
			"url": "https://example.com/sorted" + itoa(i), "title": "Sorted " + itoa(i),
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("seed %d returned %d", i, resp.StatusCode)
		}
		var c struct{ ID string }
		f.decode(resp, &c)
		ids = append(ids, c.ID)
	}

	// Click counts that deliberately disagree with creation order, including a
	// block of ties — ties are what the id tiebreaker exists for, and a cursor
	// that cannot break them loops or skips.
	for i, id := range ids {
		clicks := (i * 7) % 11
		if _, err := f.pool.Exec(ctx,
			`UPDATE links SET click_count = $2 WHERE id = $1`, id, clicks); err != nil {
			t.Fatal(err)
		}
	}

	for _, sort := range []string{"newest", "oldest", "clicks"} {
		t.Run(sort, func(t *testing.T) {
			seen := map[string]bool{}
			order := make([]string, 0, total)
			cursor := ""
			for pages := 0; pages <= 10; pages++ {
				path := "/api/v1/links?limit=7&sort=" + sort
				if cursor != "" {
					path += "&cursor=" + url.QueryEscape(cursor)
				}
				resp := f.do(http.MethodGet, path, nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("page %d returned %d", pages, resp.StatusCode)
				}
				var page struct {
					Items []struct {
						ID         string `json:"id"`
						ClickCount int64  `json:"click_count"`
					} `json:"items"`
					NextCursor string `json:"next_cursor"`
					HasMore    bool   `json:"has_more"`
				}
				f.decode(resp, &page)

				for _, it := range page.Items {
					if seen[it.ID] {
						t.Fatalf("sort=%s returned link %s on two pages", sort, it.ID)
					}
					seen[it.ID] = true
					order = append(order, it.ID)
				}
				if !page.HasMore {
					break
				}
				cursor = page.NextCursor
			}
			if len(seen) != total {
				t.Errorf("sort=%s paged over %d links, want %d: pagination dropped rows",
					sort, len(seen), total)
			}

			// The paged sequence must equal the unpaged one. Covering every row
			// is not enough — a cursor can be complete and still out of order.
			resp := f.do(http.MethodGet, "/api/v1/links?limit=100&sort="+sort, nil)
			var all struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			f.decode(resp, &all)
			if len(all.Items) != len(order) {
				t.Fatalf("sort=%s single page returned %d links, paged returned %d",
					sort, len(all.Items), len(order))
			}
			for i := range order {
				if order[i] != all.Items[i].ID {
					t.Fatalf("sort=%s paged order diverges at position %d: %s paged, %s unpaged",
						sort, i, order[i], all.Items[i].ID)
				}
			}
		})
	}

	// A cursor names a position in one ordering. Reusing it under another sort
	// is refused rather than silently reinterpreted against the wrong column.
	resp := f.do(http.MethodGet, "/api/v1/links?limit=7&sort=newest", nil)
	var first struct {
		NextCursor string `json:"next_cursor"`
	}
	f.decode(resp, &first)
	resp = f.do(http.MethodGet,
		"/api/v1/links?limit=7&sort=clicks&cursor="+url.QueryEscape(first.NextCursor), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a newest cursor replayed under sort=clicks returned %d, want 422",
			resp.StatusCode)
	}
	f.decode(resp, nil)
}

// include_total must describe the same set as the items beside it. CountLinks
// had no tag branch at all, so a tag-filtered page reported the workspace's
// whole link count and a client rendering "8 of 100" was off by the filter.
func TestIncludeTotalRespectsTheTagFilter(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	for i := range 9 {
		tag := "campaign"
		if i >= 3 {
			tag = "other"
		}
		resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
			"url": "https://example.com/tagged" + itoa(i), "tags": []string{tag},
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("seed %d returned %d", i, resp.StatusCode)
		}
		f.decode(resp, nil)
	}

	resp := f.do(http.MethodGet, "/api/v1/links?include_total=true", nil)
	var tags struct {
		Items []struct {
			Tags []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"tags"`
		} `json:"items"`
	}
	f.decode(resp, &tags)
	var campaignID string
	for _, it := range tags.Items {
		for _, tg := range it.Tags {
			if tg.Name == "campaign" {
				campaignID = tg.ID
			}
		}
	}
	if campaignID == "" {
		t.Fatal("no link came back carrying the campaign tag")
	}

	resp = f.do(http.MethodGet, "/api/v1/links?include_total=true&tag="+campaignID, nil)
	var filtered struct {
		Items []struct{ ID string } `json:"items"`
		Total *int64                `json:"total"`
	}
	f.decode(resp, &filtered)
	if len(filtered.Items) != 3 {
		t.Fatalf("tag filter returned %d links, want 3", len(filtered.Items))
	}
	if filtered.Total == nil || *filtered.Total != 3 {
		t.Errorf("total = %v under a tag filter returning 3 items, want 3", filtered.Total)
	}
}

// The two tag aggregates are paired positionally, so they have to be ordered
// identically. Names sorted by name and ids sorted by id, which are different
// orders whenever a link's tags were not created in alphabetical order — every
// tag then came back carrying another tag's name.
func TestTagIDsAndNamesPairCorrectly(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Created zebra-first so creation order (uuidv7, hence id order) is the
	// reverse of alphabetical order.
	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/z", "tags": []string{"zebra"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seeding zebra returned %d", resp.StatusCode)
	}
	f.decode(resp, nil)

	resp = f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/both", "tags": []string{"alpha", "zebra"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seeding both returned %d", resp.StatusCode)
	}
	var created struct {
		ID   string `json:"id"`
		Tags []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"tags"`
	}
	f.decode(resp, &created)

	// GetLinkTags joins properly, so the single-link read is the reference the
	// list has to match.
	want := map[string]string{}
	for _, tg := range created.Tags {
		want[tg.ID] = tg.Name
	}
	if len(want) != 2 {
		t.Fatalf("link was created with %d tags, want 2", len(want))
	}

	resp = f.do(http.MethodGet, "/api/v1/links", nil)
	var page struct {
		Items []struct {
			ID   string `json:"id"`
			Tags []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"tags"`
		} `json:"items"`
	}
	f.decode(resp, &page)

	for _, it := range page.Items {
		if it.ID != created.ID {
			continue
		}
		if len(it.Tags) != 2 {
			t.Fatalf("listed link carries %d tags, want 2", len(it.Tags))
		}
		for _, tg := range it.Tags {
			if name, ok := want[tg.ID]; !ok {
				t.Errorf("listed an unknown tag id %s", tg.ID)
			} else if name != tg.Name {
				t.Errorf("tag %s listed as %q, but it is %q: the id and name "+
					"arrays are paired positionally and disagree on order",
					tg.ID, tg.Name, name)
			}
		}
	}
}

func TestEmptyTagListSerializesAsArray(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{"url": "https://example.com"})
	defer func() { _ = resp.Body.Close() }()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	// [] rather than null: a client iterating the field should not have to
	// nil-check it. This is why emit_empty_slices is set in sqlc.yaml.
	if string(raw["tags"]) != "[]" {
		t.Errorf("tags = %s, want []", raw["tags"])
	}
}

func TestUnknownJSONFieldIsRejected(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// A misspelled field must not be silently dropped, or the caller believes
	// they set something they did not.
	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com", "titel": "typo",
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 for an unknown field", resp.StatusCode)
	}
}

func TestViewerCannotCreateLinks(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Demote to viewer and confirm the service, not the route, refuses.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE memberships SET role_id = (SELECT id FROM roles WHERE slug='viewer' AND organization_id IS NULL)`,
	); err != nil {
		t.Fatal(err)
	}

	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{"url": "https://example.com"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a viewer creating a link", resp.StatusCode)
	}

	// Reading is still allowed.
	resp2 := f.do(http.MethodGet, "/api/v1/links", nil)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("viewer list returned %d, want 200", resp2.StatusCode)
	}
}

func TestMeReportsPermissions(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	resp := f.do(http.MethodGet, "/api/v1/me", nil)
	var me struct {
		Email       string   `json:"email"`
		Role        string   `json:"role"`
		Permissions []string `json:"permissions"`
	}
	f.decode(resp, &me)

	if me.Role != "owner" {
		t.Errorf("role = %q, want owner", me.Role)
	}
	if len(me.Permissions) < 10 {
		t.Errorf("owner has %d permissions, want the full set", len(me.Permissions))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
