//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/ratelimit"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// The gates (M35): password, signature, one-time and max-click.
//
// **Every fixture here runs with Redis switched off**, which is not a
// convenience. The inherited rule is that the cache is optional, and the gates
// are the first thing in this product that would be *wrong* rather than merely
// slower if that were untrue: a click budget that lived in Redis would re-open
// every spent link the moment the cache was flushed. Running the whole file
// against Postgres alone is what makes "correct with Redis disabled entirely"
// something the build checks rather than something a comment claims.
//
// The tripwire has its own fixture at the bottom of this file, for the reason
// it is in hosts_test.go: M35 is the milestone that let POST reach this tree, so
// this is where a session lookup would first appear. It cannot share the fixture
// above — a tripwire that fails on any session lookup also fails the sign-in the
// API needs — so it seeds through the link service exactly as hosts_test.go
// does, and for the same reason.

type gateFixture struct {
	*apiFixture
	resolver *redirect.Resolver
	gates    *gate.Service
	domainID uuid.UUID
	links    *link.Service
	limiter  *ratelimit.Limiter
}

// newGates builds the redirect tree with the gate service attached.
//
// passwordLimit of 0 disables the password limiter, which is what every test
// here wants except the one that is about the limiter: a limit in the way would
// make a test that submits several passwords flaky for a reason it is not about.
func newGates(t *testing.T, passwordLimit int) *gateFixture {
	t.Helper()
	return newGatesWithStatus(t, passwordLimit, http.StatusFound)
}

// newGatesWithStatus is newGates on an instance configured for some other
// redirect status.
//
// It exists for one test, and the test is F81: `REDIRECT_DEFAULT_STATUS` admits
// 307, and until this reopening the password gate answered whatever it was set
// to. Every other fixture in this suite pins 302, which is why nothing here saw
// it.
func newGatesWithStatus(t *testing.T, passwordLimit, status int) *gateFixture {
	t.Helper()
	pool := newDB(t)

	cfg := config.Config{
		AppEnv: config.Development, BaseURL: "http://links.test", SecureCookies: false,
	}
	cfg.Auth.SignupMode = config.SignupOpen
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour
	cfg.Redirect.DefaultStatus = status

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
	})
	// Nil Redis client. See the file comment: this is the milestone's own claim,
	// not a shortcut.
	resolver := redirect.NewResolver(pool, nil, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	gateSvc := gate.NewService(pool, gate.Config{Hasher: authSvc.Hasher()})
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.BaseURL, Cache: resolver,
		Hasher: authSvc.Hasher(), Gates: gateSvc,
	})

	dom, err := dbgen.New(pool).ResolveDefaultDomain(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	limiter := ratelimit.New(passwordLimit, ratelimit.Options{})

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg,
		Health: &httpx.Health{DB: pool},
		Auth:   authSvc,
		Links:  linkSvc,
		Redirect: &httpx.RedirectHandler{
			Resolver: resolver, DomainID: dom.ID, Status: status,
			Gates: gateSvc, PasswordLimiter: limiter,
		},
	}))
	t.Cleanup(srv.Close)

	jar, _ := newCookieJar()
	f := &apiFixture{t: t, server: srv, client: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, pool: pool, auth: authSvc}

	return &gateFixture{
		apiFixture: f, resolver: resolver, gates: gateSvc,
		domainID: dom.ID, links: linkSvc, limiter: limiter,
	}
}

// createGated makes a link and returns its alias and id.
func (f *gateFixture) createGated(body map[string]any) (alias string, id uuid.UUID) {
	f.t.Helper()
	resp := f.do(http.MethodPost, "/api/v1/links", body)
	if resp.StatusCode != http.StatusCreated {
		defer func() { _ = resp.Body.Close() }()
		var problem map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&problem)
		f.t.Fatalf("create link returned %d: %v", resp.StatusCode, problem)
	}
	var created struct {
		Alias string
		ID    uuid.UUID
	}
	f.decode(resp, &created)
	return created.Alias, created.ID
}

func (f *gateFixture) visit(path string) *http.Response {
	f.t.Helper()
	return f.raw(http.MethodGet, path, nil)
}

func (f *gateFixture) submit(path string, form url.Values) *http.Response {
	f.t.Helper()
	return f.raw(http.MethodPost, path, form)
}

// raw drives the redirect tree without the JSON content type the API helper
// sets, because a password form is form-encoded and a browser sends no cookies
// this tree would read anyway.
func (f *gateFixture) raw(method, path string, form url.Values) *http.Response {
	f.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.server.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// A session cookie is sent deliberately on every one of these, so the
	// no-cookie assertions below are made against a request a browser would
	// actually send rather than against a bare one.
	req.AddCookie(&http.Cookie{Name: auth.CookieName(false), Value: "a-real-looking-token"})
	resp, err := (&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}).Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// --- the fields are settable ------------------------------------------------

// TestGateFieldsAreAcceptedAndReported replaces the 422 contract M35 removed.
//
// The old test asserted `not_implemented` for `password`, `max_clicks` and
// `one_time`. Those refusals were the honest answer while the feature did not
// exist; asserting them now would be asserting that it still does not.
func TestGateFieldsAreAcceptedAndReported(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/gated", "password": "a-link-password-here",
		"max_clicks": 5, "one_time": false, "require_signature": true,
	})
	var created struct {
		ID               uuid.UUID
		HasPassword      bool   `json:"has_password"`
		MaxClicks        *int64 `json:"max_clicks"`
		OneTime          bool   `json:"one_time"`
		RequireSignature bool   `json:"require_signature"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with gates returned %d, want 201", resp.StatusCode)
	}
	f.decode(resp, &created)

	if !created.HasPassword {
		t.Error("has_password is false on a link created with one")
	}
	if created.MaxClicks == nil || *created.MaxClicks != 5 {
		t.Errorf("max_clicks = %v, want 5", created.MaxClicks)
	}
	if !created.RequireSignature {
		t.Error("require_signature is false on a link created with it")
	}

	// The password must not come back, in any field, under any name. This is
	// asserted over the raw body rather than over a struct, because a struct
	// only catches the fields somebody remembered to declare.
	resp = f.do(http.MethodGet, "/api/v1/links/"+created.ID.String(), nil)
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 64*1024)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	for _, forbidden := range []string{"a-link-password-here", "$argon2id$"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("GET /links/{id} response contains %q; a link's password and its "+
				"hash must never leave the server\n%s", forbidden, body)
		}
	}
}

// TestClickLimitBelowOneIsRefused pins the one value the column would accept and
// the product must not.
func TestClickLimitBelowOneIsRefused(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/nobody", "max_clicks": 0,
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("max_clicks=0 returned %d, want 422; a link nobody may follow is "+
			"indistinguishable from one somebody meant to delete", resp.StatusCode)
	}
}

// --- password ---------------------------------------------------------------

func TestPasswordLinkChallengeAndVerification(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	const password = "the-link-password"
	alias, _ := f.createGated(map[string]any{
		"url": "https://example.com/secret", "password": password,
	})

	// A visit is a challenge, not a redirect.
	resp := f.visit("/" + alias)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /%s = %d, want 200 with the challenge page", alias, resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("the challenge carried Location: %q; the destination must not leak "+
			"to somebody who has not answered", loc)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("X-Robots-Tag"); got == "" {
		t.Error("the challenge page is indexable")
	}

	// The wrong password re-serves the challenge and still discloses nothing.
	resp = f.submit("/"+alias, url.Values{"password": {"not-the-password"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wrong password = %d, want 200 with the retry page", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("a wrong password produced Location: %q", loc)
	}

	// The right one answers the redirect itself — as a 303, which is the one
	// status that mandates a GET and therefore the one that keeps the password
	// body from being re-sent to the destination (D94, amending D53's 302).
	resp = f.submit("/"+alias, url.Values{"password": {password}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("correct password = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/secret" {
		t.Errorf("Location = %q, want the destination", got)
	}
}

// TestPasswordVerificationIssuesNothing is D53's condition, as a test.
//
// The CSRF waiver rests entirely on this POST handing nothing to the browser. A
// later change that set a cookie — an "unlock" that survived the request — would
// void the reasoning the amendment was signed off on, and this is what notices.
func TestPasswordVerificationIssuesNothing(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	const password = "the-link-password"
	alias, _ := f.createGated(map[string]any{
		"url": "https://example.com/secret", "password": password,
	})

	for _, resp := range []*http.Response{
		f.visit("/" + alias),
		f.submit("/"+alias, url.Values{"password": {"wrong"}}),
		f.submit("/"+alias, url.Values{"password": {password}}),
	} {
		if cookies := resp.Cookies(); len(cookies) > 0 {
			t.Fatalf("the redirect tree set %d cookie(s) on a password link: %v.\n"+
				"D53 waived CSRF protection for this POST *because* it issues nothing. "+
				"Anything handed to the browser here voids that reasoning and reopens "+
				"the decision — it is not a change to make quietly.", len(cookies), cookies)
		}
	}

	// And the unlock really does not persist: the next visit asks again.
	if resp := f.visit("/" + alias); resp.StatusCode != http.StatusOK {
		t.Errorf("a second visit after a correct password = %d, want 200 (the "+
			"challenge again). Nothing is remembered, by design.", resp.StatusCode)
	}
}

// TestPasswordSubmitPerformsNoSessionLookup is the tripwire half of D53.
//
// The amendment widened `methodFilter` and nothing else. Everything the
// tripwires assert stands: the link host has no session middleware, so a POST
// carrying a session cookie must not resolve one. Seeded through the link
// service rather than over the API, because the tripwire fails the test on *any*
// session lookup and signing in is one.
func TestPasswordSubmitPerformsNoSessionLookup(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()

	cfg := config.Config{AppEnv: config.Development, BaseURL: "http://links.test"}
	cfg.Auth.SignupMode = config.SignupOpen

	authSvc := auth.NewService(pool, auth.ServiceConfig{Params: fastParams})
	resolver := redirect.NewResolver(pool, nil, redirect.Options{
		TTL: time.Hour, NegativeTTL: time.Minute,
	})
	gateSvc := gate.NewService(pool, gate.Config{Hasher: authSvc.Hasher()})
	linkSvc := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: cfg.BaseURL, Cache: resolver,
		Hasher: authSvc.Hasher(), Gates: gateSvc,
	})
	dom, err := dbgen.New(pool).ResolveDefaultDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}

	owner, err := authSvc.Register(ctx, auth.RegisterInput{
		Email: "tripwire@example.com", Password: "a-sufficiently-long-password",
		IsFirstUser: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	const password = "the-link-password"
	created, err := linkSvc.Create(ctx, owner, link.CreateInput{
		URL: "https://example.com/secret", Alias: "tripwired", Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}

	tripwire := &tripwireAuthenticator{t: t}
	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config:        cfg,
		Health:        &httpx.Health{DB: pool},
		Authenticator: tripwire,
		Redirect: &httpx.RedirectHandler{
			Resolver: resolver, DomainID: dom.ID, Status: http.StatusFound,
			Gates: gateSvc,
		},
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, tc := range []struct {
		name, method string
		form         url.Values
		want         int
	}{
		{"challenge", http.MethodGet, nil, http.StatusOK},
		{"wrong password", http.MethodPost, url.Values{"password": {"nope"}}, http.StatusOK},
		// 303 rather than 302 since D94; the tripwire is about session lookups and
		// does not care which redirect status, but a stale number here would read
		// as an assertion that the status is 302.
		{"correct password", http.MethodPost, url.Values{"password": {password}}, http.StatusSeeOther},
	} {
		var body *strings.Reader
		if tc.form != nil {
			body = strings.NewReader(tc.form.Encode())
		} else {
			body = strings.NewReader("")
		}
		req, rerr := http.NewRequestWithContext(ctx, tc.method,
			srv.URL+"/"+created.Alias, body)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if tc.form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.AddCookie(&http.Cookie{Name: auth.CookieName(false), Value: "a-real-looking-token"})
		resp, derr := client.Do(req)
		if derr != nil {
			t.Fatal(derr)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
		if tripwire.called.Load() {
			t.Fatalf("the %s request performed a session lookup.\n"+
				"D53 authorised one thing: POST on the single-segment redirect "+
				"pattern. The link host still has no session middleware, and this "+
				"is what proves it stayed that way.", tc.name)
		}
	}
}

// TestPostToAnAliasWithoutAPasswordIsRefused is the other half of D53's
// boundary: the mux lets POST through, and everything that is not a password
// link answers exactly what it answered before.
func TestPostToAnAliasWithoutAPasswordIsRefused(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	alias, _ := f.createGated(map[string]any{"url": "https://example.com/open"})

	resp := f.submit("/"+alias, url.Values{"password": {"anything"}})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to an ordinary alias = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want \"GET, HEAD\"", got)
	}

	// And the multi-segment pattern never gained POST at all, so this one is
	// refused by the mux rather than by the handler.
	resp = f.submit("/"+alias+"/deep", url.Values{"password": {"anything"}})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST beneath an alias = %d, want 405; D53 widened the "+
			"single-segment pattern only", resp.StatusCode)
	}
}

// TestCachedSnapshotCarriesNoPasswordHash is the bullet m35.md states as a rule
// rather than as a comment.
func TestCachedSnapshotCarriesNoPasswordHash(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	const password = "the-link-password"
	alias, _ := f.createGated(map[string]any{
		"url": "https://example.com/secret", "password": password,
	})

	// Resolve it, so the snapshot is the one the redirect path actually built.
	res, err := f.resolver.Resolve(context.Background(), f.domainID, alias)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Snapshot.HasPassword {
		t.Fatal("the snapshot does not know the link has a password")
	}

	// The payload as it would be written to Redis. Marshalled here rather than
	// read back out of a cache, because the claim is about what the value
	// *encodes to* — an instance with Redis off must not be the only one where
	// this holds.
	encoded, err := json.Marshal(res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT password_hash FROM links WHERE alias = $1`, alias).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{hash, "$argon2id$", password} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("the cached snapshot carries %q.\nIt is written to Redis on every "+
				"cache miss, so a hash in here is an offline cracking target for every "+
				"password link on the instance.\npayload: %s", forbidden, encoded)
		}
	}
}

// --- one-time and max-click --------------------------------------------------

func TestOneTimeLinkIsFollowedOnce(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	alias, _ := f.createGated(map[string]any{
		"url": "https://example.com/once", "one_time": true,
	})

	if resp := f.visit("/" + alias); resp.StatusCode != http.StatusFound {
		t.Fatalf("the first visit = %d, want 302", resp.StatusCode)
	}
	resp := f.visit("/" + alias)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("the second visit = %d, want 410. The counter is in Postgres "+
			"precisely so this holds with the cache switched off.", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("a spent link still carried Location: %q", loc)
	}
}

func TestMaxClickLinkStopsAtItsCeiling(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	const limit = 3
	alias, id := f.createGated(map[string]any{
		"url": "https://example.com/limited", "max_clicks": limit,
	})

	for i := range limit {
		if resp := f.visit("/" + alias); resp.StatusCode != http.StatusFound {
			t.Fatalf("visit %d = %d, want 302", i+1, resp.StatusCode)
		}
	}
	if resp := f.visit("/" + alias); resp.StatusCode != http.StatusGone {
		t.Fatalf("visit %d = %d, want 410", limit+1, resp.StatusCode)
	}

	// links.click_count is untouched by any of this, which is the bullet that
	// says the approximate counter stays approximate. It is written by the
	// analytics pipeline, which is not running in this fixture, so it must still
	// read zero after four visits.
	var clickCount int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT click_count FROM links WHERE id = $1`, id).Scan(&clickCount); err != nil {
		t.Fatal(err)
	}
	if clickCount != 0 {
		t.Errorf("links.click_count = %d after gating four visits; the gate must "+
			"read its own durable counter and leave this column alone", clickCount)
	}

	// And the durable one counted every allowed click and no more. The refused
	// fourth visit must not have incremented it, or a raised ceiling would open
	// fewer clicks than it says.
	consumed, _, err := f.gates.Budget(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != limit {
		t.Errorf("link_click_budget.consumed = %d, want %d", consumed, limit)
	}
}

// TestHeadDoesNotSpendAClick keeps a link checker from destroying a one-time
// link by asking whether it is alive.
func TestHeadDoesNotSpendAClick(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	alias, _ := f.createGated(map[string]any{
		"url": "https://example.com/once", "one_time": true,
	})

	if resp := f.raw(http.MethodHead, "/"+alias, nil); resp.StatusCode != http.StatusFound {
		t.Fatalf("HEAD = %d, want 302", resp.StatusCode)
	}
	if resp := f.visit("/" + alias); resp.StatusCode != http.StatusFound {
		t.Fatalf("the first real visit after a HEAD = %d, want 302; a HEAD must not "+
			"spend the one click this link has", resp.StatusCode)
	}
}

// TestHeadOnASpentLinkIsRefused is the other half of the sentence above, and
// the half that was missing (F78).
//
// *Never spends a click* and *never checks whether there is one* are different
// claims, and the code delivered the second while the milestone promised the
// first. The consequence is not a nicety: an exhausted one-time link answered
// 302 with its destination in `Location` to every HEAD, forever, and the click
// recorder skips HEAD too, so the disclosure left nothing behind to notice.
//
// Repeated three times deliberately. A budget that HEAD consumed would also make
// the first of these 410 — by destroying the link — so one request cannot tell
// the fix from the outcome D53 refuses.
func TestHeadOnASpentLinkIsRefused(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	alias, id := f.createGated(map[string]any{
		"url": "https://example.com/secret-destination", "one_time": true,
	})

	if resp := f.visit("/" + alias); resp.StatusCode != http.StatusFound {
		t.Fatalf("the first visit = %d, want 302", resp.StatusCode)
	}
	if resp := f.visit("/" + alias); resp.StatusCode != http.StatusGone {
		t.Fatalf("the second visit = %d, want 410", resp.StatusCode)
	}

	for i := range 3 {
		resp := f.raw(http.MethodHead, "/"+alias, nil)
		if resp.StatusCode != http.StatusGone {
			t.Fatalf("HEAD %d on a spent link = %d, want 410.\n"+
				"A GET has already been told this link is gone; answering a HEAD "+
				"anything else publishes the destination of a link nobody may "+
				"follow, to whoever asks with the cheaper method.", i+1, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Fatalf("HEAD %d carried Location: %q", i+1, loc)
		}
	}

	// And it is still the non-consuming read D53 bought. Three HEADs against a
	// budget of one would read as four consumed if any of them wrote.
	consumed, _, err := f.gates.Budget(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != 1 {
		t.Errorf("consumed = %d after one GET and three HEADs, want 1; HEAD must "+
			"answer from the counter without adding to it", consumed)
	}
}

// TestACorrectPasswordAnswersASeeOther is F81.
//
// On an instance configured for 307 the password POST used to be answered with
// 307, and RFC 9110 §15.4.8 forbids a user agent changing the method — so the
// browser re-sent `password=<secret>` as a POST body to the link's destination,
// a third-party host the operator does not control. 303 is the one redirect
// status that mandates a GET.
func TestACorrectPasswordAnswersASeeOther(t *testing.T) {
	f := newGatesWithStatus(t, 0, http.StatusTemporaryRedirect)
	f.setupOwner()

	// The instance really is configured for a method-preserving status, or the
	// rest of this proves nothing.
	open, _ := f.createGated(map[string]any{"url": "https://example.com/open"})
	if resp := f.visit("/" + open); resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("an ordinary link = %d, want 307; this fixture is meant to be the "+
			"307 instance", resp.StatusCode)
	}

	const password = "the-link-password"
	alias, _ := f.createGated(map[string]any{
		"url": "https://example.com/secret", "password": password,
	})

	resp := f.submit("/"+alias, url.Values{"password": {password}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a correct password on a 307 instance = %d, want 303.\n"+
			"307 preserves the method, so the browser re-POSTs the password body "+
			"to the destination — a plaintext credential handed to whoever runs it.",
			resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/secret" {
		t.Errorf("Location = %q, want the destination", got)
	}
}

// TestOneTimeLinkUnderRealConcurrency is the test m35.md names as mandatory.
//
// **Real concurrency, not a sequential loop.** The sequential version passes
// against a read-then-write implementation, which is exactly the implementation
// that hands the destination to two people at once under load. Every request
// below starts from the same barrier so they contend on the counter rather than
// queueing behind each other.
func TestOneTimeLinkUnderRealConcurrency(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	alias, id := f.createGated(map[string]any{
		"url": "https://example.com/once", "one_time": true,
	})

	// Warm the snapshot, so the racers contend on the budget and not on the
	// resolver's singleflight.
	if _, err := f.resolver.Resolve(context.Background(), f.domainID, alias); err != nil {
		t.Fatal(err)
	}

	const racers = 32
	var start sync.WaitGroup
	var done sync.WaitGroup
	var redirects, gone, other atomic.Int64
	start.Add(1)
	done.Add(racers)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for range racers {
		go func() {
			defer done.Done()
			start.Wait()
			req, err := http.NewRequestWithContext(context.Background(),
				http.MethodGet, f.server.URL+"/"+alias, nil)
			if err != nil {
				other.Add(1)
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				other.Add(1)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			switch resp.StatusCode {
			case http.StatusFound:
				redirects.Add(1)
			case http.StatusGone:
				gone.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	start.Done()
	done.Wait()

	if got := redirects.Load(); got != 1 {
		t.Errorf("%d of %d concurrent requests were redirected, want exactly 1.\n"+
			"A one-time link that hands its destination to more than one visitor is "+
			"not one-time, and this is the failure a sequential loop cannot see.",
			got, racers)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("%d requests answered neither 302 nor 410", got)
	}
	if got := gone.Load(); got != racers-1 {
		t.Errorf("%d requests got 410, want %d", got, racers-1)
	}

	consumed, exhaustedAt, err := f.gates.Budget(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != 1 {
		t.Errorf("consumed = %d after the race, want 1", consumed)
	}
	if exhaustedAt == nil {
		t.Error("exhausted_at was not stamped when the budget ran out")
	}
}

// --- signed URLs -------------------------------------------------------------

func TestSignedURLIsRequiredVerifiedAndExpires(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	alias, id := f.createGated(map[string]any{
		"url": "https://example.com/embargoed", "require_signature": true,
	})

	// Unsigned is refused.
	if resp := f.visit("/" + alias); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned = %d, want 403", resp.StatusCode)
	}

	// Minted through the API, because a client that could not produce one would
	// make the feature unreachable.
	resp := f.do(http.MethodPost, "/api/v1/links/"+id.String()+"/sign",
		map[string]any{"ttl_seconds": 3600})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("sign returned %d, want 201", resp.StatusCode)
	}
	var signed struct {
		URL       string
		ExpiresAt time.Time `json:"expires_at"`
	}
	f.decode(resp, &signed)

	parsed, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("sig") == "" || parsed.Query().Get("exp") == "" {
		t.Fatalf("the signed URL carries no sig/exp: %s", signed.URL)
	}
	query := "?" + parsed.RawQuery

	if got := f.visit("/" + alias + query); got.StatusCode != http.StatusFound {
		t.Fatalf("a valid signature = %d, want 302", got.StatusCode)
	}

	// A tampered signature, and a tampered expiry. The expiry is inside the MAC,
	// so extending it must not work — that is the whole reason it is signed
	// rather than merely accompanied by a signature.
	tampered := parsed.Query()
	tampered.Set("sig", strings.Repeat("A", len(tampered.Get("sig"))))
	if got := f.visit("/" + alias + "?" + tampered.Encode()); got.StatusCode != http.StatusForbidden {
		t.Errorf("a forged signature = %d, want 403", got.StatusCode)
	}

	extended := parsed.Query()
	extended.Set("exp", "99999999999")
	if got := f.visit("/" + alias + "?" + extended.Encode()); got.StatusCode != http.StatusForbidden {
		t.Errorf("an edited expiry = %d, want 403; the expiry is inside the MAC "+
			"precisely so whoever holds the URL cannot extend it", got.StatusCode)
	}

	// And an honestly-expired one. Minted directly against the workspace secret,
	// because the API refuses to issue a signature that is already dead.
	secret, err := f.gates.Secret(context.Background(), f.workspaceID())
	if err != nil {
		t.Fatal(err)
	}
	stale, err := gate.Sign(secret, f.domainID, alias, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.visit("/" + alias + "?" + stale.Encode()); got.StatusCode != http.StatusForbidden {
		t.Errorf("an expired signature = %d, want 403", got.StatusCode)
	}
}

// TestSignatureParametersAreNotForwarded keeps a workspace's capability from
// reaching whoever runs the destination.
func TestSignatureParametersAreNotForwarded(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()

	alias, id := f.createGated(map[string]any{
		"url": "https://example.com/embargoed", "require_signature": true,
		"forward_query": true,
	})

	resp := f.do(http.MethodPost, "/api/v1/links/"+id.String()+"/sign", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("sign returned %d", resp.StatusCode)
	}
	var signed struct{ URL string }
	f.decode(resp, &signed)
	parsed, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatal(err)
	}

	q := parsed.Query()
	q.Set("utm_source", "newsletter")
	got := f.visit("/" + alias + "?" + q.Encode())
	if got.StatusCode != http.StatusFound {
		t.Fatalf("signed visit = %d, want 302", got.StatusCode)
	}
	loc := got.Header.Get("Location")
	if !strings.Contains(loc, "utm_source=newsletter") {
		t.Errorf("Location = %q; the visitor's own parameters should still forward", loc)
	}
	for _, leaked := range []string{"sig=", "exp="} {
		if strings.Contains(loc, leaked) {
			t.Errorf("Location = %q leaks %q to the destination; whoever runs it could "+
				"then replay the signature until it expires", loc, leaked)
		}
	}
}

// workspaceID reads the owner's workspace, for the tests that need to sign
// something the API would refuse to sign.
func (f *gateFixture) workspaceID() uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT workspace_id FROM links LIMIT 1`).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// --- the namespace a gate is keyed on (F79, F80) -----------------------------
//
// **These run on the custom-domain fixture, and that is the whole point.** Every
// gate test above it wires `DomainID` from `ResolveDefaultDomain`, so the
// instance default and the link's own domain are the same uuid and two gates
// keyed on the wrong one of them read identically. A second hostname is the
// cheapest arrangement in which they differ at all.

// verifiedHost verifies a hostname and waits for this replica to know it.
//
// `verify` alone is not enough: the invalidator refreshes the host cache in a
// goroutine, deliberately, so that a subscriber's read loop never blocks on
// Postgres. Every test here asks the custom hostname for something immediately
// afterwards, and a background reload that had not landed answers 404 from the
// split-host router — a failure about scheduling, in a test about signatures.
// The existing domain tests reload synchronously for the same reason.
func verifiedHost(f *domainFixture, hostname string) *link.Domain {
	f.t.Helper()
	d := f.verify(f.register(hostname))
	if err := f.hosts.Reload(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
	return d
}

// gatedOn creates a link on a named domain, with gates.
func gatedOn(f *domainFixture, domainID *uuid.UUID, alias, url string, in link.CreateInput) *domain.Link {
	f.t.Helper()
	in.URL, in.Alias, in.DomainID = url, alias, domainID
	l, err := f.links.Create(f.t.Context(), f.owner, in)
	if err != nil {
		f.t.Fatalf("create %s: %v", alias, err)
	}
	return l
}

// postOn submits a form to one of the fixture's hostnames.
func postOn(f *domainFixture, host, path string, form url.Values) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost,
		f.server.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestASignedLinkWorksOnACustomHostname is F79 and F80 together, because they
// are the two halves of one round trip: the URL that is minted, and the request
// that is verified.
//
// Before this reopening the feature was **wholly non-functional** on a custom
// hostname. `Sign` built the URL on the instance's own host, and the verifier
// checked the MAC against the instance's own domain id while the signer had used
// the link's — so the legitimate holder of a signature was answered 403 by a link
// they had just been given a URL for, on a hostname that URL did not name.
func TestASignedLinkWorksOnACustomHostname(t *testing.T) {
	f := newDomains(t)
	d := verifiedHost(f, customHost)
	l := gatedOn(f, &d.ID, "embargo", "https://example.com/embargoed",
		link.CreateInput{RequireSignature: true})

	// The gate is on, or the 302 below would mean nothing.
	if resp := f.get(customHost, "/embargo"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an unsigned request = %d, want 403", resp.StatusCode)
	}

	signed, err := f.links.Sign(t.Context(), f.owner, l.ID, time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parsed, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != customHost {
		t.Errorf("the signed URL is %q, on host %q, want %q.\n"+
			"Alias uniqueness is (domain_id, alias) and the default domain is shared "+
			"across workspaces, so a signed URL on the wrong host does not merely "+
			"404 — where that alias exists there it resolves somebody else's link.",
			signed.URL, parsed.Host, customHost)
	}

	resp := f.get(customHost, "/embargo?"+parsed.RawQuery)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("a signature minted for this link = %d on the hostname it was minted "+
			"for, want 302.\nThe domain id is inside the MAC; the verifier has to use "+
			"the domain the request arrived on, not the one this process resolved at "+
			"boot.", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/embargoed" {
		t.Errorf("Location = %q, want the destination", got)
	}
}

// TestASignatureDoesNotCrossHostnames is the property the domain id is in the
// MAC to provide, asserted for the first time in the arrangement that can break
// it.
//
// Two links, the same alias, two hostnames, one workspace — which is exactly
// what per-domain alias uniqueness makes possible. A signature minted for one of
// them verified against the *other* while the verifier read a boot constant,
// because from its point of view the two requests were the same alias on the
// same domain.
func TestASignatureDoesNotCrossHostnames(t *testing.T) {
	f := newDomains(t)

	// The default-domain link first: once a workspace verifies a hostname, that
	// hostname becomes the default for its new links (D71), so creating this one
	// afterwards would quietly put both links on the same domain and the test
	// would pass without proving anything.
	onDefault := gatedOn(f, nil, "shared", "https://example.com/on-the-default-host",
		link.CreateInput{RequireSignature: true})
	d := verifiedHost(f, customHost)
	gatedOn(f, &d.ID, "shared", "https://example.com/on-the-custom-host",
		link.CreateInput{RequireSignature: true})

	signed, err := f.links.Sign(t.Context(), f.owner, onDefault.ID, time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parsed, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != linkHost {
		t.Fatalf("the signed URL is on %q, want the instance's own link host %q",
			parsed.Host, linkHost)
	}

	// Where it was minted, it works.
	resp := f.get(linkHost, "/shared?"+parsed.RawQuery)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the signature = %d on its own host, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://example.com/on-the-default-host" {
		t.Errorf("Location = %q, want the link it was minted for", got)
	}

	// And nowhere else.
	crossed := f.get(customHost, "/shared?"+parsed.RawQuery)
	if crossed.StatusCode != http.StatusForbidden {
		t.Errorf("a signature minted for %s/shared answered %d on %s, want 403.\n"+
			"They are two links in two namespaces; a capability issued for one of "+
			"them opening the other is what the domain id in the MAC exists to stop.",
			linkHost, crossed.StatusCode, customHost)
	}
	if loc := crossed.Header.Get("Location"); loc != "" {
		t.Errorf("the crossed request carried Location: %q — the other link's "+
			"destination, handed to somebody holding a signature for a different link", loc)
	}
}

// TestThePasswordBucketIsKeyedOnTheRequestsHostname is the second limb of F79.
//
// It fails safe rather than open — one shared bucket admits fewer guesses, not
// more — and it is fixed here anyway because it is the same defect reading the
// same boot constant, and a limb left behind is a comment that has quietly
// stopped being true.
func TestThePasswordBucketIsKeyedOnTheRequestsHostname(t *testing.T) {
	f := newDomains(t)
	d := verifiedHost(f, customHost)
	gatedOn(f, &d.ID, "locked", "https://example.com/secret",
		link.CreateInput{Password: "the-link-password"})

	if resp := postOn(f, customHost, "/locked", url.Values{"password": {"wrong"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("a wrong password = %d, want 200 with the retry page", resp.StatusCode)
	}

	// The fixture's ceiling is one guess, so exactly one bucket is now empty and
	// which one it is says which domain the limiter was keyed on.
	if ok, _ := f.limiter.AllowKey("pw:" + d.ID.String() + ":locked"); ok {
		t.Errorf("the guess left %s's own bucket full; it was charged somewhere else",
			customHost)
	}
	if ok, _ := f.limiter.AllowKey("pw:" + f.defaultDomain.String() + ":locked"); !ok {
		t.Errorf("the guess was charged to the *instance default's* bucket for the " +
			"alias `locked`.\nThe comment on this key says the same alias on two " +
			"domains is two links; keyed on the domain resolved at boot it was one " +
			"bucket shared by every hostname on the instance.")
	}
}

// --- brute force (D54) -------------------------------------------------------

// A link with more legitimate visitors than its limit does not throttle itself.
//
// Both limbs are spent before the form is parsed, deliberately — that ordering
// is what keeps timing from saying which limb refused — and the cost nobody had
// recorded is that the per-alias bucket is emptied by *success* as readily as by
// failure. Twenty people opening the same password link at the same moment
// exhaust it between them, with no attacker present at all (F115).
//
// A correct password now hands the alias token back. That touches neither D53
// nor D54: the per-alias keying that closes distributed guessing is unchanged,
// an attacker who does not have the password never reaches the refund, and what
// is returned was spent by somebody who proved they had it. The address limb
// stays spent, which this test asserts too — a correct password is still traffic
// from that address.
func TestCorrectPasswordsDoNotThrottleTheLinkTheyOpen(t *testing.T) {
	const limit = 6
	f := newGates(t, limit)
	f.setupOwner()

	alias, _ := f.createGated(map[string]any{
		"url": "https://example.com/secret", "password": "the-link-password",
	})

	// Measured on the **alias** bucket directly, and that is forced rather than
	// chosen. Every request in this test comes from one client, so the
	// per-address limb refuses long before the per-alias one does — in a real
	// deployment the twenty visitors F115 describes have twenty addresses
	// between them and only the alias bucket is shared. Counting tokens is the
	// only way to ask about one limb from a fixture that has a single address.
	tokensLeft := func() int {
		t.Helper()
		n := 0
		for {
			ok, _ := f.limiter.AllowKey(passwordAliasBucket(f.domainID, alias))
			if !ok {
				return n
			}
			n++
			if n > limit*4 {
				t.Fatal("the alias bucket never emptied; it is not the bucket this " +
					"test thinks it is")
			}
		}
	}

	const correct = 3
	for i := range correct {
		resp := f.submit("/"+alias, url.Values{"password": {"the-link-password"}})
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("visitor %d with the right password got %d, want 303", i+1, resp.StatusCode)
		}
	}

	if n := tokensLeft(); n != limit {
		t.Errorf("%d of %d alias tokens survived %d correct passwords, want all %d. "+
			"A link that spends its own bucket on the people it was made for "+
			"throttles its audience with no attacker present (F115)",
			n, limit, correct, limit)
	}
}

// A wrong guess is still charged, so the refund has not disarmed D54.
func TestAWrongPasswordStillSpendsTheAliasBucket(t *testing.T) {
	const limit = 6
	f := newGates(t, limit)
	f.setupOwner()

	alias, _ := f.createGated(map[string]any{
		"url": "https://example.com/secret", "password": "the-link-password",
	})

	const wrong = 3
	for range wrong {
		resp := f.submit("/"+alias, url.Values{"password": {"not-it"}})
		_ = resp.Body.Close()
	}

	n := 0
	for {
		ok, _ := f.limiter.AllowKey(passwordAliasBucket(f.domainID, alias))
		if !ok {
			break
		}
		n++
		if n > limit*4 {
			t.Fatal("the alias bucket never emptied")
		}
	}
	if n != limit-wrong {
		t.Errorf("%d alias tokens survived %d wrong guesses, want %d. Refunding a "+
			"correct password must not refund an incorrect one, or the per-alias "+
			"limb D53's waiver rests on stops existing", n, wrong, limit-wrong)
	}
}

// passwordAliasBucket spells the per-alias limiter key the gate uses. Kept here
// rather than imported so a test cannot pass by sharing a typo with the code it
// checks.
func passwordAliasBucket(domainID uuid.UUID, alias string) string {
	return "pw:" + domainID.String() + ":" + alias
}

// TestPasswordGuessesAreLimitedPerAliasAndPerAddress is D54.
//
// The per-alias limb is the one worth a test of its own: a per-address limit
// alone is defeated by driving guesses through many visitors' browsers, and that
// is the variant D53's CSRF waiver depends on being closed.
func TestPasswordGuessesAreLimitedPerAliasAndPerAddress(t *testing.T) {
	f := newGates(t, 4)
	f.setupOwner()

	alias, _ := f.createGated(map[string]any{
		"url": "https://example.com/secret", "password": "the-link-password",
	})

	// Each guess spends a token from both buckets, so four attempts empty them.
	var throttled bool
	for i := range 8 {
		resp := f.submit("/"+alias, url.Values{"password": {"wrong"}})
		if resp.StatusCode == http.StatusTooManyRequests {
			throttled = true
			if resp.Header.Get("Retry-After") == "" {
				t.Error("a throttled guess carried no Retry-After")
			}
			break
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("guess %d = %d, want 200 (retry) or 429", i+1, resp.StatusCode)
		}
	}
	if !throttled {
		t.Fatal("eight wrong passwords in a row were never throttled; without a limit " +
			"a link password is worth exactly as much as the wordlist somebody runs")
	}

	// The alias bucket is what closes distributed guessing. Draining it from one
	// address and then asking on a *fresh* limiter key for the address would
	// still be refused, because the alias limb is empty.
	if ok, _ := f.limiter.AllowKey("pw:" + f.domainID.String() + ":" + alias); ok {
		t.Error("the per-alias bucket still had tokens after the guesses above; " +
			"guesses spread across addresses would then never be limited at all")
	}
}

// --- Redis is optional -------------------------------------------------------

// TestGatesAreCorrectWithoutRedis is the inherited rule, asserted rather than
// implied by the rest of this file running that way.
//
// It uses the gate service directly, so the claim is about the mechanism and not
// about a fixture's wiring: the counter is consumed twice against a limit of one
// with no cache anywhere in the process.
func TestGatesAreCorrectWithoutRedis(t *testing.T) {
	pool := newDB(t)
	authSvc := auth.NewService(pool, auth.ServiceConfig{Params: fastParams})
	gates := gate.NewService(pool, gate.Config{Hasher: authSvc.Hasher()})

	ctx := context.Background()
	linkID, workspaceID := seedBareLink(t, pool, authSvc)

	ok, err := gates.Consume(ctx, linkID, workspaceID, 1)
	if err != nil || !ok {
		t.Fatalf("the first click was refused: ok=%v err=%v", ok, err)
	}
	ok, err = gates.Consume(ctx, linkID, workspaceID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("the second click of a one-time link was allowed with no Redis in " +
			"the process. The counter is in Postgres for exactly this reason: a " +
			"budget that lives in the cache re-opens every spent link when it is flushed.")
	}
}

// seedBareLink writes the minimum a click budget can hang off.
//
// The workspace comes from registering an owner, because a fresh database has
// none: workspaces are provisioned by registration, and a test that INSERTed one
// by hand would be counting against a shape the product never produces.
func seedBareLink(t *testing.T, pool *pgxpool.Pool, authSvc *auth.Service) (linkID, workspaceID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	owner, err := authSvc.Register(ctx, auth.RegisterInput{
		Email: "budget@example.com", Password: "a-sufficiently-long-password",
		IsFirstUser: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspaceID = owner.WorkspaceID

	var domainID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM domains LIMIT 1`).Scan(&domainID); err != nil {
		t.Fatal(err)
	}
	linkID = uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO links (id, workspace_id, domain_id, alias, primary_url, status, one_time)
		VALUES ($1, $2, $3, $4, 'https://example.com/once', 'active', true)`,
		linkID, workspaceID, domainID, "gt"+linkID.String()[:6]); err != nil {
		t.Fatal(err)
	}
	return linkID, workspaceID
}

// TestAGateQueryIsBoundedByItsOwnBudget is F96.
//
// Every query in `internal/gate` runs on the redirect path and none of them was
// bounded by anything. `RequestTimeout` wraps the application handler only; the
// redirect tree is mounted bare on purpose, because `http.TimeoutHandler` buffers
// the response and would break the `Location` write and swallow the pages this
// file asserts. There is no `statement_timeout` in this tree and the pool sets
// only connect and lifetime limits. So a gate query that could not proceed held a
// connection for as long as Postgres would let it, while requests queued behind
// it — the inverse of the two-pool separation the redirect path exists to have.
//
// Driven with a table lock rather than with a slow query, because the thing being
// asserted is that the *caller* gives up, and a lock is the one way to make a
// fast query take arbitrarily long without changing it.
func TestAGateQueryIsBoundedByItsOwnBudget(t *testing.T) {
	pool := newDB(t)
	const budget = 200 * time.Millisecond
	svc := gate.NewService(pool, gate.Config{DBTimeout: budget})

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(),
		`LOCK TABLE link_click_budget IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock the budget table: %v", err)
	}

	start := time.Now()
	_, err = svc.Consume(t.Context(), uuid.New(), uuid.New(), 1)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Consume succeeded against a locked table")
	}
	// Generous, because this asserts that something gave up rather than how
	// quickly: an unbounded call waits for the transaction above, which this test
	// only ends by returning.
	if elapsed > 10*budget {
		t.Errorf("Consume took %v against a budget of %v; the gate's queries are "+
			"not bounded by anything", elapsed, budget)
	}
}

// TestAClientGoingAwayDoesNotSpendAClick is the half of F96's bound that had to
// be got right rather than merely applied.
//
// The resolver detaches its query from the request with `context.WithoutCancel`,
// because a singleflight leader's result is shared with every waiter and one
// abandoned tab must not fail the rest. Nothing in the gate service is shared, and
// copying that shape onto `Consume` would mean a visitor who hit Stop had still
// spent a one-time link's only click — on a redirect nobody received.
func TestAClientGoingAwayDoesNotSpendAClick(t *testing.T) {
	f := newGates(t, 0)
	f.setupOwner()
	alias, id := f.createGated(map[string]any{
		"url": "https://example.com/once", "alias": "gonequick", "one_time": true,
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.gates.Consume(ctx, id, f.workspaceID(), 1); err == nil {
		t.Error("Consume spent a click for a caller whose context had already been " +
			"cancelled; the budget's bound is detached from the request")
	}

	// The proof is not the error, it is that the link is still followable.
	resp := f.visit("/" + alias)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the one-time link answered %d after a cancelled consume, want 302 — "+
			"its only click was spent by a request that went nowhere", resp.StatusCode)
	}
}
