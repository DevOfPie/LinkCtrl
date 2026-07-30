package httpx

import (
	"net/http"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Deps are the collaborators the router needs. An explicit struct so adding a
// dependency is a visible change rather than a hidden global.
type Deps struct {
	Config   config.Config
	Health   *Health
	Auth     *auth.Service
	Links    *link.Service
	Redirect *RedirectHandler

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
		app.HandleFunc("POST "+APIPrefix+"/auth/setup", authAPI.Setup)
		app.HandleFunc("POST "+APIPrefix+"/auth/register", authAPI.Register)
		app.HandleFunc("POST "+APIPrefix+"/auth/login", authAPI.Login)
		app.HandleFunc("POST "+APIPrefix+"/auth/logout", authAPI.Logout)
		app.Handle("POST "+APIPrefix+"/auth/password",
			RequireAuth(http.HandlerFunc(authAPI.ChangePassword)))
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

	var appHandler http.Handler = app
	if a := d.authenticator(); a != nil {
		appHandler = Session(a, d.Config.SecureCookies)(appHandler)
	}
	appHandler = SecurityHeaders(d.Config)(appHandler)

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
	root.Handle(APIPrefix+"/", appHandler)

	root.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("LinkCtrl\n"))
	})

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
	return RealIP(d.Config.TrustedProxies)(root)
}

// RegisteredTopLevelPaths lists the first path segment of every route the
// router registers, for the test that guards against an alias shadowing a
// real route.
func RegisteredTopLevelPaths() []string {
	return []string{"healthz", "readyz", "api"}
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
