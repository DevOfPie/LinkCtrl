//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// apiFixture is a running server plus a cookie-jar client, so tests drive the
// real HTTP surface rather than calling services directly. Middleware, routing,
// JSON shape and status codes are all in scope.
type apiFixture struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	pool   *pgxpool.Pool
	keys   *auth.APIKeyService
}

func newAPI(t *testing.T) *apiFixture {
	t.Helper()
	pool := newDB(t)

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
	})

	// Not started: the usage tracker's ticker is not wanted in tests, and the
	// tests that care about last_used_at call FlushUsage directly rather than
	// sleeping through an interval.
	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg,
		Health: &httpx.Health{DB: pool},
		Auth:   authSvc,
		Keys:   keySvc,
		Links:  linkSvc,
		Stats:  analytics.NewReader(pool),
	}))
	t.Cleanup(srv.Close)

	jar, _ := newCookieJar()
	return &apiFixture{
		t:      t,
		server: srv,
		client: &http.Client{Jar: jar},
		pool:   pool,
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

func TestPhase2FieldsAreRejectedNotIgnored(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Accepting these silently would look like the feature works while the
	// link is in fact unprotected — worse than refusing.
	cases := map[string]map[string]any{
		"password":   {"url": "https://example.com", "password": "hunter2hunter2"},
		"max_clicks": {"url": "https://example.com", "max_clicks": 5},
		"one_time":   {"url": "https://example.com", "one_time": true},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := f.do(http.MethodPost, "/api/v1/links", body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422 with a not_implemented code", resp.StatusCode)
			}
		})
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
