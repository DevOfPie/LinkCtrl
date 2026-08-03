package httpx

import (
	"net/http"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
	"github.com/DevOfPie/LinkCtrl/internal/team"
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
	// RootRedirect serves the link host's root. Only consulted on a split-host
	// deployment; nil leaves that root a 404, which is what it was before the
	// setting existed.
	RootRedirect *RootRedirect
	Stats        *analytics.Reader
	// Audit serves the audit log. Nil leaves the endpoint unregistered, which
	// is what the parity test against openapi.yaml compares itself to.
	Audit *audit.Service
	// Notify serves the per-user inbox, and backs the nav's unread count.
	Notify *notify.Service
	// Invites serves the invitation lifecycle. Nil leaves both the endpoints
	// and the dashboard page unregistered, which is what the parity test
	// against openapi.yaml compares itself to.
	Invites *invite.Service
	// Team serves member management, workspace lifecycle and the organization
	// lifecycle. Nil leaves its endpoints and its three dashboard pages
	// unregistered, which is what the parity test against openapi.yaml compares
	// itself to.
	Team *team.Service
	// Signup owns whether the instance accepts new accounts. Nil leaves the
	// public signup pages unregistered and every registration refused, which is
	// the direction a missing dependency has to fail in.
	Signup *signup.Service
	// Disputes serves the blocked-attempt appeal path and the review queue. Nil
	// leaves both the endpoints and the dashboard page unregistered, which is
	// what the parity test against openapi.yaml compares itself to — and which
	// also takes the "ask for a review" button off the links form, so a refusal
	// never offers a door that is not there.
	Disputes *dispute.Service
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
		authAPI := &AuthAPI{Auth: d.Auth, Signup: d.Signup, Config: d.Config}
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

		// The switcher. On the auth service because which workspace a request
		// acts in is identity, not a feature of one.
		ws := &WorkspaceAPI{Auth: d.Auth}
		for pattern, h := range map[string]http.HandlerFunc{
			"GET " + APIPrefix + "/workspaces":              ws.List,
			"POST " + APIPrefix + "/workspaces/{id}/switch": ws.Switch,
			"PUT " + APIPrefix + "/workspaces/default":      ws.SetDefault,
		} {
			app.Handle(pattern, RequireAuth(h))
		}
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
			// Minting a signed URL (M35). No permission of its own: a signature
			// is what makes a gated link followable, so issuing one is
			// links.update — see internal/link/gates.go.
			"POST " + APIPrefix + "/links/{id}/sign": api.Sign,
			// Routing rules (M34), nested under the link they belong to. No
			// permission of their own: a rule is where a link points, and that is
			// links.read and links.update — see internal/link/routing.go.
			"GET " + APIPrefix + "/links/{id}/rules":             api.ListRules,
			"POST " + APIPrefix + "/links/{id}/rules":            api.CreateRule,
			"PATCH " + APIPrefix + "/links/{id}/rules/{ruleID}":  api.UpdateRule,
			"DELETE " + APIPrefix + "/links/{id}/rules/{ruleID}": api.DeleteRule,
			// Split testing (M36), nested under the link for the same reason and
			// guarded by the same two permissions: an arm is where a link points,
			// expressed as a share rather than as a condition.
			"GET " + APIPrefix + "/links/{id}/split":                api.GetSplit,
			"POST " + APIPrefix + "/links/{id}/split":               api.CreateVariant,
			"PATCH " + APIPrefix + "/links/{id}/split/{variantID}":  api.UpdateVariant,
			"DELETE " + APIPrefix + "/links/{id}/split/{variantID}": api.DeleteVariant,
			// Folders (M38). A sibling collection rather than a subresource of a
			// link: a folder exists whether or not anything is in it, and which
			// links it holds is a question the links list answers with
			// `?folder=`. No permission of their own either — a folder is where a
			// link lives, and that is links.read, links.create, links.update and
			// links.delete; see internal/link/folder.go and decisions.md, D67.
			//
			// The move is its own POST rather than a field on the PATCH, because
			// `parent_id: null` has to mean "the top level" and a PATCH field's
			// null means "unchanged" everywhere else on this API.
			"GET " + APIPrefix + "/folders":                  api.ListFolders,
			"POST " + APIPrefix + "/folders":                 api.CreateFolder,
			"PATCH " + APIPrefix + "/folders/{folderID}":     api.UpdateFolder,
			"DELETE " + APIPrefix + "/folders/{folderID}":    api.DeleteFolder,
			"POST " + APIPrefix + "/folders/{folderID}/move": api.MoveFolder,
			"GET " + APIPrefix + "/tags":                     api.ListTags,
			"DELETE " + APIPrefix + "/tags/{id}":             api.DeleteTag,
			"GET " + APIPrefix + "/domain":                   api.GetDomain,
			"PATCH " + APIPrefix + "/domain":                 api.UpdateDomain,
			// Registered hostnames (M39). A different resource from the
			// singular /domain above, which is the instance default's settings
			// and predates there being more than one domain.
			//
			// No permission of its own: domains.write already exists (00800)
			// and M39 turns it into an ownership check rather than adding a
			// second slug — a workspace admin administers their own hostnames
			// and gets a 403 on anybody else's. See internal/link/domains.go
			// and decisions.md, D68 and D69.
			//
			// Nothing registered here is served. The host router still refuses
			// an unrecognized Host with the operational 404; verification and
			// serving are M40's.
			"GET " + APIPrefix + "/domains":               api.ListDomains,
			"POST " + APIPrefix + "/domains":              api.CreateDomain,
			"PATCH " + APIPrefix + "/domains/{domainID}":  api.UpdateRegisteredDomain,
			"DELETE " + APIPrefix + "/domains/{domainID}": api.DeleteRegisteredDomain,
			// The reputation-feed disclosure (M32). Read-only, and there is no
			// second operation here by design: the page it mirrors accepts no
			// POST (D40), and an API that could write would be the settings
			// surface D38 removed wearing a bearer token.
			"GET " + APIPrefix + "/feeds": (&FeedAPI{Links: d.Links}).Get,
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

	if d.Audit != nil {
		a := &AuditAPI{Audit: d.Audit}
		app.Handle("GET "+APIPrefix+"/audit", RequireAuth(http.HandlerFunc(a.List)))
	}

	if d.Invites != nil {
		inv := &InvitationAPI{Invites: d.Invites}
		for pattern, h := range map[string]http.HandlerFunc{
			"POST " + APIPrefix + "/invitations":        inv.Create,
			"GET " + APIPrefix + "/invitations":         inv.List,
			"DELETE " + APIPrefix + "/invitations/{id}": inv.Revoke,
		} {
			app.Handle(pattern, RequireAuth(h))
		}
		// Redemption is the one endpoint here that must work with no credential
		// at all — it is how somebody acquires their first one. It verifies a
		// password, so it carries the login limit rather than the API one, and
		// shares the limiter with /auth/login so alternating between the two
		// does not double an attacker's budget.
		app.Handle("POST "+APIPrefix+"/invitations/redeem",
			RateLimit(d.Limits.Login, "login", d.Metrics, nil)(http.HandlerFunc(inv.Redeem)))
	}

	if d.Team != nil {
		tm := &TeamAPI{Team: d.Team}
		for pattern, h := range map[string]http.HandlerFunc{
			"GET " + APIPrefix + "/members":         tm.ListMembers,
			"POST " + APIPrefix + "/members":        tm.GrantMember,
			"PATCH " + APIPrefix + "/members/{id}":  tm.ChangeMemberRole,
			"DELETE " + APIPrefix + "/members/{id}": tm.RemoveMember,
			// Creating and reshaping workspaces sits under the same prefix the
			// switcher already owns, because it is the same object. The
			// switcher's own routes are unchanged; nothing here is a method or
			// path either of them already claims.
			"POST " + APIPrefix + "/workspaces":        tm.CreateWorkspace,
			"PATCH " + APIPrefix + "/workspaces/{id}":  tm.RenameWorkspace,
			"DELETE " + APIPrefix + "/workspaces/{id}": tm.DeleteWorkspace,
			// Creating an organization and tearing one down sit on the same
			// collection. The delete carries an id even though it can only ever
			// name the organization the caller is acting in: a path parameter
			// that has to match is a confirmation, and an irreversible operation
			// with no target in its URL is one a client can fire by accident.
			"POST " + APIPrefix + "/organizations":        tm.CreateOrganization,
			"DELETE " + APIPrefix + "/organizations/{id}": tm.DeleteOrganization,
		} {
			app.Handle(pattern, RequireAuth(h))
		}
	}

	if d.Disputes != nil {
		dp := &DisputeAPI{Disputes: d.Disputes}
		for pattern, h := range map[string]http.HandlerFunc{
			"POST " + APIPrefix + "/disputes":             dp.File,
			"GET " + APIPrefix + "/disputes":              dp.List,
			"POST " + APIPrefix + "/disputes/{id}/allow":  dp.Allow,
			"POST " + APIPrefix + "/disputes/{id}/uphold": dp.Uphold,
		} {
			app.Handle(pattern, RequireAuth(h))
		}
	}

	if d.Notify != nil {
		n := &NotificationAPI{Notify: d.Notify}
		for pattern, h := range map[string]http.HandlerFunc{
			"GET " + APIPrefix + "/notifications":            n.List,
			"GET " + APIPrefix + "/notifications/unread":     n.Unread,
			"POST " + APIPrefix + "/notifications/read":      n.ReadAll,
			"POST " + APIPrefix + "/notifications/{id}/read": n.Read,
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

		// What every dashboard route needs: a session, and an organization to
		// spend it in. The second is D36's — an account whose only organization
		// was deleted keeps its account and belongs to nothing, and every page
		// below assumes a workspace it can render. They are composed here rather
		// than per route so a page added later cannot forget the second half; the
		// two routes that must work *without* an organization are registered
		// outside this helper, where their absence from it is visible.
		signedIn := func(fn http.HandlerFunc) http.Handler {
			return web.RequireWebAuth(web.RequireOrganization(fn))
		}

		// Public: sign-in, first-run setup, and the root redirect.
		app.HandleFunc("GET /{$}", web.Root)
		app.HandleFunc("GET /login", web.LoginPage)
		app.Handle("POST /login", guard(http.HandlerFunc(web.LoginSubmit)))
		app.HandleFunc("POST /logout", web.Logout)
		app.HandleFunc("GET /setup", web.SetupPage)
		// Unauthenticated on purpose: the preference is per-browser, not per
		// account, so it has to be settable from the login page too.
		app.HandleFunc("POST /theme", web.ThemeSet)
		app.Handle("POST /setup", guard(http.HandlerFunc(web.SetupSubmit)))
		app.Handle("POST /account/password", guard(signedIn(web.PasswordChange)))
		app.Handle("POST /account/domain", signedIn(web.DomainUpdate))
		app.Handle("POST /account/bots", signedIn(web.BotBlockingUpdate))

		// Redeeming an invitation is public, because the person doing it may
		// have no account yet — and on a default instance, where the mailer is
		// off, a copied link is the only way this page is ever reached. The
		// POST verifies a password, so it carries the login limit.
		if web.Invites != nil {
			app.HandleFunc("GET /invite/{token}", web.InvitePage)
			app.Handle("POST /invite/{token}", guard(http.HandlerFunc(web.InviteAccept)))
		}

		// Self-serve signup, and the link that finishes one. Both are public by
		// definition — the person using them has no account yet — and both carry
		// the login limit rather than the API one: the first creates a password
		// and the second turns a token into an account, so neither may be
		// attempted at machine speed. Sharing the limiter with /login is
		// deliberate, so alternating between the surfaces does not double an
		// attacker's budget.
		//
		// Registered whether or not sign-ups are open, so that a closed instance
		// answers the refusal these handlers write rather than the alias
		// catch-all's 404: "there is no sign-up here" and "there is no such
		// link" are different answers, and only one of them is true.
		if web.Signup != nil {
			app.HandleFunc("GET /signup", web.SignupPage)
			app.Handle("POST /signup", guard(http.HandlerFunc(web.SignupSubmit)))
			app.HandleFunc("GET /verify/{token}", web.VerifyPage)
			app.Handle("POST /verify/{token}", guard(http.HandlerFunc(web.VerifySubmit)))
		}

		// Everything else redirects anonymous visitors to the login form,
		// where the API would return a problem document.
		for pattern, fn := range map[string]http.HandlerFunc{
			"GET /dashboard":   web.Dashboard,
			"GET /links":       web.LinksPage,
			"POST /links":      web.LinkCreate,
			"GET /links/{id}":  web.LinkDetail,
			"POST /links/{id}": web.LinkUpdate,
			// Folders (M38). Four POSTs and no JavaScript: creating, renaming,
			// moving and deleting are each a form that submits on its own, so the
			// page works with scripting off. htmx swaps the tree in place when it
			// is on, which is the enhancement rather than the mechanism — and
			// dragging is deliberately absent, because a drag target is
			// unreachable by keyboard and unreachable without script.
			"GET /folders":                    web.FoldersPage,
			"POST /folders":                   web.FolderCreate,
			"POST /folders/{folderID}":        web.FolderRename,
			"POST /folders/{folderID}/move":   web.FolderMove,
			"POST /folders/{folderID}/delete": web.FolderDelete,
			// Registered hostnames (M39). Three POSTs and no JavaScript, like
			// the folder page: registering, renaming and removing are each a
			// form that submits on its own. The page exists to say what the API
			// says — that a registered hostname is stored unverified and that
			// nothing is served on it until M40.
			"GET /domains":                    web.DomainsPage,
			"POST /domains":                   web.DomainCreate,
			"POST /domains/{domainID}":        web.DomainRename,
			"POST /domains/{domainID}/delete": web.DomainDelete,
			// Routing rules (M34). Three actions rather than one form: adding a
			// rule and switching one off are different enough operations that
			// making the second go through the first would mean opening an editor
			// to pause a campaign.
			"POST /links/{id}/rules":                 web.RuleCreate,
			"POST /links/{id}/rules/{ruleID}/toggle": web.RuleToggle,
			"POST /links/{id}/rules/{ruleID}/delete": web.RuleDelete,
			// Split testing (M36). The same three actions for the same reason:
			// parking an arm mid-test is what somebody reaches for when one of
			// them is misbehaving, and it must not require opening an editor.
			"POST /links/{id}/split":                    web.VariantCreate,
			"POST /links/{id}/split/{variantID}/toggle": web.VariantToggle,
			"POST /links/{id}/split/{variantID}/delete": web.VariantDelete,
			// Minting a signed URL (M35). A POST because it can create the
			// workspace's signing secret, and because a capability must not be
			// something a link somebody clicks can produce.
			"POST /links/{id}/sign":         web.LinkSign,
			"POST /links/{id}/archive":      web.LinkArchive,
			"POST /links/{id}/restore":      web.LinkRestore,
			"POST /links/{id}/delete":       web.LinkDelete,
			"GET /keys":                     web.KeysPage,
			"GET /notifications":            web.NotificationsPage,
			"POST /notifications/read":      web.NotificationReadAll,
			"POST /notifications/{id}/read": web.NotificationRead,
			"POST /keys":                    web.KeyCreate,
			"POST /keys/{id}/revoke":        web.KeyRevoke,
			"GET /account":                  web.AccountPage,
			// Read-only, and registered GET-only on purpose: a POST to /feeds
			// must be refused by the mux rather than by a handler somebody
			// could add one to. D40, and TestTheDisclosurePageAcceptsNoWrite.
			"GET /feeds":              web.FeedsPage,
			"POST /workspace/switch":  web.WorkspaceSwitch,
			"POST /workspace/default": web.WorkspaceDefault,
		} {
			app.Handle(pattern, signedIn(fn))
		}

		if web.Invites != nil {
			for pattern, fn := range map[string]http.HandlerFunc{
				"GET /invites":              web.InvitesPage,
				"POST /invites":             web.InviteCreate,
				"POST /invites/{id}/revoke": web.InviteRevoke,
			} {
				app.Handle(pattern, signedIn(fn))
			}
		}

		if web.Disputes != nil {
			for pattern, fn := range map[string]http.HandlerFunc{
				"GET /disputes":              web.DisputesPage,
				"POST /disputes":             web.DisputeFile,
				"POST /disputes/{id}/allow":  web.DisputeAllow,
				"POST /disputes/{id}/uphold": web.DisputeUphold,
			} {
				app.Handle(pattern, signedIn(fn))
			}
		}

		if web.Team != nil {
			for pattern, fn := range map[string]http.HandlerFunc{
				"GET /members":                    web.MembersPage,
				"POST /members":                   web.MemberGrant,
				"POST /members/{id}/role":         web.MemberRole,
				"POST /members/{id}/remove":       web.MemberRemove,
				"GET /workspaces":                 web.WorkspacesPage,
				"POST /workspaces":                web.WorkspaceCreate,
				"POST /workspaces/{id}/rename":    web.WorkspaceRename,
				"POST /workspaces/{id}/delete":    web.WorkspaceDelete,
				"POST /organizations/{id}/delete": web.OrganizationDelete,
			} {
				app.Handle(pattern, signedIn(fn))
			}

			// The two routes an account that belongs to nothing must still
			// reach, and the only ones registered without RequireOrganization
			// (D36). Redirecting these to themselves would be the loop the whole
			// state exists to avoid; the page turns a non-orphan away on its own,
			// and the POST authorizes in the service like every other write.
			app.Handle("GET /organizations/new", web.RequireWebAuth(http.HandlerFunc(web.OrganizationNewPage)))
			app.Handle("POST /organizations", web.RequireWebAuth(http.HandlerFunc(web.OrganizationCreate)))
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
	// The dashboard's own origin is added as trusted, for deployments where a
	// proxy makes the Host header disagree with the public origin. It is the app
	// origin specifically: the link host never receives an unsafe method, and
	// trusting it here would widen the set of origins allowed to post to the
	// dashboard for no gain.
	csrf := http.NewCrossOriginProtection()
	if err := csrf.AddTrustedOrigin(d.Config.AppOrigin()); err == nil {
		appHandler = csrf.Handler(appHandler)
	} else {
		// An unparseable origin cannot reach here — config validation refuses
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
	//
	// One mux when the dashboard and short links share a hostname, which is the
	// default and the only deployment 0.1.0 had. Two when they do not, and then
	// each host answers only its own paths.

	// Operational endpoints. No session middleware: a readiness probe should
	// not perform a session lookup, and these must answer while the database
	// is down.
	//
	// Registered on every host, including hostnames this instance was never
	// configured with. Probes come from load balancers, orchestrators and the
	// container runtime, none of which send the operator's chosen name — the
	// image's own healthcheck asks 127.0.0.1.
	registerOps := func(mux *http.ServeMux) {
		if d.Health == nil {
			return
		}
		mux.HandleFunc("GET /healthz", d.Health.Live)
		mux.HandleFunc("GET /readyz", d.Health.Ready)
	}

	registerApp := func(root *http.ServeMux) {
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
			// Mounted from the same slice the reserved-list guard reads, so a
			// new dashboard route cannot be registered without the guard seeing
			// it. Two lists that had to agree by hand is what the guard existed
			// to prevent in the first place.
			for _, p := range dashboardPatterns {
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
	// user creating an alias that collides with a route added later. That stays
	// true on a split-host deployment, where the collision is impossible: an
	// instance can be merged back onto one host, and an alias created as
	// "login" during the split would break the dashboard on the day it is.
	//
	// Two patterns, not one, and they do not overlap: "/{alias}" matches a path
	// of exactly one segment, "/{alias}/{rest...}" matches everything with a
	// separator after the alias. ServeMux would reject an ambiguous pair, so the
	// fact that both register is itself the proof they are disjoint.
	//
	// The multi-segment pattern is the most general thing in the tree, which is
	// what makes it safe: every registered route is a strict subset of it —
	// "/api/v1/", "/static/", "/links/", "/invite/" — so each of them still wins
	// on ServeMux's specificity rule. Only a route that is *also* an arbitrary
	// two-segment wildcard could be shadowed, and there is no such route.
	//
	// It carries the same methodFilter for the same reason: written with a
	// method it would be ambiguous against the specific ops routes, and
	// method-less it is unambiguously the more general pattern.
	// **POST on the single-segment pattern is D53, signed off 2026-08-02, and it
	// is the only amendment to the redirect tree's tripwires this phase makes.**
	//
	// Before M35 both lines below read
	// `methodFilter(d.Redirect, http.MethodGet, http.MethodHead)`, so a POST to a
	// link host answered 405 at the mux. A password link has to accept a form,
	// and the form posts to the URL it was served from — which is what lets the
	// challenge page be fixed bytes with no template engine and no knowledge of
	// the link it belongs to.
	//
	// The permission is exactly one method on exactly one pattern, and the rest
	// of the redirect tree's rules stand unamended: no session lookup, no CSRF
	// middleware, no template rendering, and no cookie set anywhere. The POST
	// issues nothing — it verifies argon2id against Postgres and answers the 302
	// itself — which is why there is no CSRF token and why the absence of one is
	// a conclusion rather than an omission. If an unlock is ever made to persist
	// across clicks, something *is* handed to the browser and D53 is revisited
	// rather than inherited.
	//
	// The multi-segment pattern keeps GET and HEAD only. A password form is
	// served at the alias itself, so nothing beneath it needs to accept a write,
	// and widening it would be permission nobody asked for. The handler enforces
	// the other half of the boundary: an alias with no password answers 405 to a
	// POST exactly as the method filter used to.
	registerRedirect := func(root *http.ServeMux) {
		if d.Redirect != nil {
			root.Handle("/{alias}",
				methodFilter(d.Redirect, http.MethodGet, http.MethodHead, http.MethodPost))
			root.Handle("/{alias}/{rest...}",
				methodFilter(d.Redirect, http.MethodGet, http.MethodHead))
		}
	}

	// The root of the link host. Registered only on the split-host path,
	// because "/{alias}" does not match "/" and on a single host that root
	// already belongs to the dashboard — registering both would be a duplicate
	// pattern and a panic at startup, which is the right way to find out.
	registerRoot := func(root *http.ServeMux) {
		if d.RootRedirect != nil {
			root.Handle("GET /{$}", d.RootRedirect)
			root.Handle("HEAD /{$}", d.RootRedirect)
		}
	}

	var root http.Handler
	if d.Config.SplitHosts() {
		appMux := http.NewServeMux()
		registerOps(appMux)
		registerApp(appMux)

		linkMux := http.NewServeMux()
		registerOps(linkMux)
		registerRoot(linkMux)
		registerRedirect(linkMux)

		opsMux := http.NewServeMux()
		registerOps(opsMux)

		root = hostRouter{
			appHost:  config.CanonicalHost(d.Config.AppBaseURLParsed().Host),
			linkHost: config.CanonicalHost(d.Config.LinkBaseURLParsed().Host),
			app:      appMux,
			link:     linkMux,
			ops:      opsMux,
		}
	} else {
		mux := http.NewServeMux()
		registerOps(mux)
		registerApp(mux)
		registerRedirect(mux)
		root = mux
	}

	// RealIP wraps both trees: analytics and rate limiting need the client
	// address, and resolving it costs a header read rather than a query.
	//
	// Metrics wrap RealIP in turn, so the recorded duration is everything the
	// server does — the outside view. The redirect path measures itself more
	// precisely inside its own handler, where the SLO is defined.
	return d.Metrics.HTTPMiddleware(RealIP(d.Config.TrustedProxies)(root))
}

// hostRouter dispatches on the request's Host when the dashboard and short
// links are served on different hostnames.
//
// A request for the wrong host is answered 404, never redirected to the right
// one. A cross-host redirect reachable through the alias namespace is an open
// redirector for anybody who can create a link, and the reserved-word list is
// no defence against that — it constrains what an alias may be called, not
// where a redirect may point.
type hostRouter struct {
	appHost, linkHost string
	app, link, ops    http.Handler
}

func (h hostRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch config.CanonicalHost(r.Host) {
	case h.appHost:
		h.app.ServeHTTP(w, r)
	case h.linkHost:
		h.link.ServeHTTP(w, r)
	default:
		// An unrecognized host gets the operational endpoints and nothing
		// else. Serving links here would let any name pointed at this address
		// publish them, which is precisely the decision Phase 2's custom
		// domains have to make deliberately, with verification behind it.
		h.ops.ServeHTTP(w, r)
	}
}

// dashboardPatterns are the dashboard routes mounted on the root mux, one by
// one rather than at "/", because "/" belongs to the redirect catch-all.
//
// A package-level slice rather than a literal inside NewRouter so that the
// reserved-list guard reads the same values the router registers. Every entry
// must also appear in internal/alias/reserved.txt, or a user could create an
// alias that shadows it; TestReservedListCoversRegisteredRoutes enforces that.
var dashboardPatterns = []string{
	"/{$}", "/login", "/logout", "/setup", "/dashboard", "/docs",
	"/links", "/links/", "/folders", "/folders/", "/keys", "/keys/", "/account", "/account/",
	"/domains", "/domains/",
	"/notifications", "/notifications/", "/theme", "/workspace/", "/feeds",
	"/invites", "/invites/", "/invite/", "/disputes", "/disputes/",
	"/members", "/members/", "/workspaces", "/workspaces/",
	"/organizations", "/organizations/",
	"/signup", "/verify/",
}

// infrastructurePatterns are the routes registered outside dashboardPatterns:
// health endpoints, the API prefix and the static tree. These are fixed and
// registered individually, so unlike the dashboard set they are listed rather
// than iterated.
var infrastructurePatterns = []string{"/healthz", "/readyz", APIPrefix + "/", "/static/"}

// RegisteredTopLevelPaths lists the first path segment of every route the
// router registers, for the test that guards against an alias shadowing a real
// route.
//
// Derived from the slices the router actually mounts rather than hand-written
// beside them. It is still not a walk of the live mux — net/http exposes no way
// to enumerate a ServeMux's patterns — but adding a dashboard route now updates
// this automatically, which is where routes are actually added.
func RegisteredTopLevelPaths() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range append(append([]string{}, dashboardPatterns...), infrastructurePatterns...) {
		seg := strings.Trim(p, "/")
		if i := strings.IndexByte(seg, '/'); i >= 0 {
			seg = seg[:i]
		}
		// "/{$}" is the root exact-match pattern; it has no segment to reserve.
		if seg == "" || seg == "{$}" || seen[seg] {
			continue
		}
		seen[seg] = true
		out = append(out, seg)
	}
	return out
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
