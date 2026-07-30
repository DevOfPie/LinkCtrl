package httpx

import (
	"net/http"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// Deps are the collaborators the router needs. An explicit struct so adding a
// dependency is a visible change rather than a hidden global.
type Deps struct {
	Config   config.Config
	Health   *Health
	Auth     *auth.Service
	Keys     *auth.APIKeyService
	Links    *link.Service
	Redirect *RedirectHandler
	Stats    *analytics.Reader
	Web      *Web
	// Metrics is optional. Nil disables instrumentation entirely rather than
	// registering into a global registry, so two servers in one test process
	// cannot collide.
	Metrics *observability.Metrics

	// Limits are the rate limits. The zero value enforces none, so a test that
	// does not care about throttling does not have to opt out of it.
	Limits Limiters

	// Authenticator overrides how session cookies are resolved. Production
	// leaves it nil and the auth service is used. The test that proves the
	// redirect path performs no session lookup substitutes a tripwire here.
	Authenticator Authenticator
}

// authenticator returns the session resolver, preferring an explicit override.
func (d Deps) authenticator() Authenticator {
	if d.Authenticator != nil {
		return d.Authenticator
	}
	if d.Auth != nil {
		return d.Auth
	}
	return nil
}

// APIPrefix is the versioned API root.
const APIPrefix = "/api/v1"

// NewRouter builds the application handler.
//
// The structure is two handler trees, not one, and that split is the point.
//
// The application tree carries session lookup, security headers and the rest.
// The redirect tree carries almost nothing: a request for /{alias} must not
// pay for a session query, a CSRF check or template machinery, because the
// budget for the entire response is 20ms and a session lookup alone is a
// database round trip.
//
// Only RealIP is shared, because analytics needs the client address and
// resolving it is a header read.
//
// Every top-level path registered here must also appear in
// internal/alias/reserved.txt, or a user could create an alias that shadows
// it. TestReservedListCoversRegisteredRoutes enforces that.
func NewRouter(d Deps) http.Handler {
	// --- application tree (authenticated, full middleware) ----------------
	app := http.NewServeMux()

	if d.Auth != nil {
		authAPI := &AuthAPI{Auth: d.Auth, Config: d.Config}
		// Credential endpoints carry the login limit rather than the API one.
		// Per-account lockout already exists and is not enough on its own: it
		// answers "many guesses at one account", while this answers "many guesses
		// from one address", which is what credential stuffing across a leaked
		// list looks like.
		guard := RateLimit(d.Limits.Login, "login", d.Metrics, nil)
		app.Handle("POST "+APIPrefix+"/auth/setup", guard(http.HandlerFunc(authAPI.Setup)))
		app.Handle("POST "+APIPrefix+"/auth/register", guard(http.HandlerFunc(authAPI.Register)))
		app.Handle("POST "+APIPrefix+"/auth/login", guard(http.HandlerFunc(authAPI.Login)))
		app.HandleFunc("POST "+APIPrefix+"/auth/logout", authAPI.Logout)
		// Changing a password needs the current one, so this endpoint verifies a
		// credential too — and the lockout does not cover it.
		app.Handle("POST "+APIPrefix+"/auth/password",
			guard(RequireAuth(http.HandlerFunc(authAPI.ChangePassword))))
	}

	if d.Links != nil {
		api := &LinkAPI{Links: d.Links}
		protected := map[string]http.HandlerFunc{
			"GET " + APIPrefix + "/me":                  api.Me,
			"GET " + APIPrefix + "/links":               api.List,
			"POST " + APIPrefix + "/links":              api.Create,
			"GET " + APIPrefix + "/links/{id}":          api.Get,
			"PATCH " + APIPrefix + "/links/{id}":        api.Update,
			"DELETE " + APIPrefix + "/links/{id}":       api.Delete,
			"POST " + APIPrefix + "/links/{id}/archive": api.Archive,
			"POST " + APIPrefix + "/links/{id}/restore": api.Restore,
			"GET " + APIPrefix + "/tags":                api.ListTags,
			"DELETE " + APIPrefix + "/tags/{id}":        api.DeleteTag,
		}
		for pattern, h := range protected {
			app.Handle(pattern, RequireAuth(h))
		}
	}

	if d.Keys != nil {
		keys := &KeyAPI{Keys: d.Keys}
		for pattern, h := range map[string]http.HandlerFunc{
			"POST " + APIPrefix + "/api-keys":        keys.Create,
			"GET " + APIPrefix + "/api-keys":         keys.List,
			"DELETE " + APIPrefix + "/api-keys/{id}": keys.Revoke,
		} {
			app.Handle(pattern, RequireAuth(h))
		}
	}

	if d.Stats != nil {
		stats := &StatsAPI{Reader: d.Stats}
		for pattern, h := range map[string]http.HandlerFunc{
			"GET " + APIPrefix + "/links/{id}/stats":  stats.LinkStats,
			"GET " + APIPrefix + "/links/{id}/clicks": stats.LinkClicks,
			"GET " + APIPrefix + "/stats/overview":    stats.Overview,
		} {
			app.Handle(pattern, RequireAuth(h))
		}
	}

	// The API reference. The spec endpoints need nothing but the embedded
	// document, so they are served whenever docs are enabled; the Swagger UI
	// page also needs the asset pipeline, so it additionally requires Web.
	if d.Config.DocsEnabled {
		docs := &DocsHandlers{}
		app.HandleFunc("GET "+APIPrefix+"/openapi.json", docs.SpecJSON)
		app.HandleFunc("GET "+APIPrefix+"/openapi.yaml", docs.SpecYAML)
		if d.Web != nil {
			docs.UI = d.Web.UI
			app.HandleFunc("GET /docs", docs.Page)
		}
	}

	if d.Web != nil {
		web := d.Web

		// The same login limit as the API, answering with a page instead of a
		// problem document. Sharing the limiter is the point: an attacker must not
		// be able to double their budget by alternating between the two surfaces.
		guard := RateLimit(d.Limits.Login, "login", d.Metrics, web.tooManyRequests)

		// Public: sign-in, first-run setup, and the root redirect.
		app.HandleFunc("GET /{$}", web.Root)
		app.HandleFunc("GET /login", web.LoginPage)
		app.Handle("POST /login", guard(http.HandlerFunc(web.LoginSubmit)))
		app.HandleFunc("POST /logout", web.Logout)
		app.HandleFunc("GET /setup", web.SetupPage)
		app.Handle("POST /setup", guard(http.HandlerFunc(web.SetupSubmit)))
		app.Handle("POST /account/password",
			guard(web.RequireWebAuth(http.HandlerFunc(web.PasswordChange))))

		// Everything else redirects anonymous visitors to the login form,
		// where the API would return a problem document.
		for pattern, fn := range map[string]http.HandlerFunc{
			"GET /dashboard":           web.Dashboard,
			"GET /links":               web.LinksPage,
			"POST /links":              web.LinkCreate,
			"GET /links/{id}":          web.LinkDetail,
			"POST /links/{id}":         web.LinkUpdate,
			"POST /links/{id}/archive": web.LinkArchive,
			"POST /links/{id}/restore": web.LinkRestore,
			"POST /links/{id}/delete":  web.LinkDelete,
			"GET /keys":                web.KeysPage,
			"POST /keys":               web.KeyCreate,
			"POST /keys/{id}/revoke":   web.KeyRevoke,
			"GET /account":             web.AccountPage,
		} {
			app.Handle(pattern, web.RequireWebAuth(fn))
		}
	}

	var appHandler http.Handler = app
	if a := d.authenticator(); a != nil {
		appHandler = Session(a, d.Config.SecureCookies)(appHandler)
	}
	// Outside Session, so it runs first: a request carrying both a bearer token
	// and a cookie is authenticated by the explicit credential.
	if d.Keys != nil {
		appHandler = BearerAuth(d.Keys)(appHandler)
	}
	// CSRF protection for everything cookie-authenticated. The stdlib check
	// reads Sec-Fetch-Site, falling back to comparing Origin against Host, so
	// a cross-site form post is refused before any handler runs. Non-browser
	// clients send neither header and pass untouched; safe methods are exempt.
	// BaseURL is added as a trusted origin for deployments where a proxy makes
	// the Host header disagree with the public origin.
	csrf := http.NewCrossOriginProtection()
	if err := csrf.AddTrustedOrigin(d.Config.BaseURL); err == nil {
		appHandler = csrf.Handler(appHandler)
	} else {
		// An unparseable BaseURL cannot reach here — config validation refuses
		// it — but if it somehow does, protect without the extra origin rather
		// than not at all.
		appHandler = http.NewCrossOriginProtection().Handler(appHandler)
	}
	appHandler = SecurityHeaders(d.Config)(appHandler)
	// Outermost in the application chain: the deadline covers the session lookup
	// and the CSRF check as well as the handler, and the timing header measures
	// what the client actually waited for rather than what the handler alone did.
	appHandler = ServerTiming(d.Config.HTTP.ServerTiming)(appHandler)
	appHandler = RequestTimeout(d.Config.HTTP.RequestTimeout)(appHandler)

	// --- root tree --------------------------------------------------------
	root := http.NewServeMux()

	// Operational endpoints. No session middleware: a readiness probe should
	// not perform a session lookup, and these must answer while the database
	// is down.
	if d.Health != nil {
		root.HandleFunc("GET /healthz", d.Health.Live)
		root.HandleFunc("GET /readyz", d.Health.Ready)
	}

	// The API subtree. Registered as a prefix so every method and path under
	// it reaches the application tree; more specific patterns still win over
	// the single-segment alias pattern below.
	//
	// The API limit wraps here rather than inside the application chain, so a
	// throttled call costs a map lookup instead of a session query and a CSRF
	// check. The trade is that its response does not carry the dashboard's
	// security headers, which is why the refusal sets nosniff itself — the rest
	// of that policy is about HTML, and this is a problem document.
	//
	// Dashboard page loads are deliberately not counted against API_RATE_PER_MIN:
	// the name says API, and a person clicking around a server-rendered UI would
	// otherwise consume the budget their own scripts need.
	root.Handle(APIPrefix+"/", RateLimit(d.Limits.API, "api", d.Metrics, nil)(appHandler))

	if d.Web != nil {
		// Dashboard prefixes, mounted one by one rather than at "/", because
		// "/" belongs to the redirect catch-all. Every entry here must appear
		// in internal/alias/reserved.txt; the reserved-list test enforces it
		// via RegisteredTopLevelPaths.
		for _, p := range []string{
			"/{$}", "/login", "/logout", "/setup", "/dashboard", "/docs",
			"/links", "/links/", "/keys", "/keys/", "/account", "/account/",
		} {
			root.Handle(p, appHandler)
		}

		// Static assets bypass the session middleware: they are public bytes,
		// and a stylesheet request must not cost a session lookup.
		root.Handle("/static/", d.Web.UI.StaticHandler("/static/"))
	} else {
		root.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("LinkCtrl\n"))
		})
	}

	// The catch-all.
	//
	// Registered without a method on purpose. "HEAD /{alias}" is rejected by
	// ServeMux as ambiguous against "GET /healthz" — it matches fewer methods
	// but a more general path, and Go refuses to guess which should win. A
	// method-less pattern is unambiguously the more general of the two, so the
	// specific routes take precedence and the handler filters methods itself.
	//
	// Precedence is structural, but the reserved-word list is what stops a
	// user creating an alias that collides with a route added later.
	if d.Redirect != nil {
		root.Handle("/{alias}", methodFilter(d.Redirect, http.MethodGet, http.MethodHead))
	}

	// RealIP wraps both trees: analytics and rate limiting need the client
	// address, and resolving it costs a header read rather than a query.
	//
	// Metrics wrap RealIP in turn, so the recorded duration is everything the
	// server does — the outside view. The redirect path measures itself more
	// precisely inside its own handler, where the SLO is defined.
	return d.Metrics.HTTPMiddleware(RealIP(d.Config.TrustedProxies)(root))
}

// RegisteredTopLevelPaths lists the first path segment of every route the
// router registers, for the test that guards against an alias shadowing a
// real route.
func RegisteredTopLevelPaths() []string {
	return []string{
		"healthz", "readyz", "api", "docs",
		"login", "logout", "setup", "dashboard", "links", "keys", "account", "static",
	}
}

// methodFilter restricts a handler to the given methods, answering anything
// else with 405 and a correct Allow header.
//
// Needed because the alias catch-all cannot carry a method in its pattern; see
// the note where it is registered.
func methodFilter(next http.Handler, allowed ...string) http.Handler {
	allow := strings.Join(allowed, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, m := range allowed {
			if r.Method == m {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Allow", allow)
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
}
