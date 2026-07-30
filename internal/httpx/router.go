package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Deps are the collaborators the router needs. An explicit struct so adding a
// dependency is a visible change rather than a hidden global.
type Deps struct {
	Config config.Config
	Health *Health
	Auth   *auth.Service
	Links  *link.Service
}

// APIPrefix is the versioned API root.
const APIPrefix = "/api/v1"

// NewRouter builds the application handler.
//
// Three groups with different middleware, which is the shape the <20ms
// redirect budget depends on:
//
//  1. operational endpoints, no auth and no logging overhead
//  2. /api/v1/*, session-or-bearer auth, problem+json errors
//  3. the alias catch-all (M7), which skips session lookup, CSRF and template
//     rendering entirely
//
// Every top-level path registered here must also appear in
// internal/alias/reserved.txt, or a user could create an alias that shadows
// it. TestReservedListCoversRegisteredRoutes enforces that.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()

	// --- operational ---------------------------------------------------
	mux.HandleFunc("GET /healthz", d.Health.Live)
	mux.HandleFunc("GET /readyz", d.Health.Ready)

	// --- API -------------------------------------------------------------
	if d.Auth != nil {
		authAPI := &AuthAPI{Auth: d.Auth, Config: d.Config}
		mux.HandleFunc("POST "+APIPrefix+"/auth/setup", authAPI.Setup)
		mux.HandleFunc("POST "+APIPrefix+"/auth/register", authAPI.Register)
		mux.HandleFunc("POST "+APIPrefix+"/auth/login", authAPI.Login)
		mux.HandleFunc("POST "+APIPrefix+"/auth/logout", authAPI.Logout)
		mux.Handle("POST "+APIPrefix+"/auth/password",
			RequireAuth(http.HandlerFunc(authAPI.ChangePassword)))
	}

	if d.Links != nil {
		api := &LinkAPI{Links: d.Links}
		// Every one of these needs an identity; the service then decides
		// whether that identity holds the required permission.
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
			mux.Handle(pattern, RequireAuth(h))
		}
	}

	// --- root -------------------------------------------------------------
	// Replaced by the dashboard in M11; the alias catch-all lands in M7.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("LinkCtrl\n"))
	})

	// Middleware applies outermost-first. RealIP runs before Session so a
	// login records the resolved address rather than the proxy's.
	var h http.Handler = mux
	if d.Auth != nil {
		h = Session(d.Auth, d.Config.SecureCookies)(h)
	}
	h = RealIP(d.Config.TrustedProxies)(h)
	h = SecurityHeaders(d.Config)(h)
	return h
}

// RegisteredTopLevelPaths lists the first path segment of every route the
// router registers. Used by the test that guards against an alias shadowing a
// real route.
func RegisteredTopLevelPaths() []string {
	return []string{"healthz", "readyz", "api"}
}
