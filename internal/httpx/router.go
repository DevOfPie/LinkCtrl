package httpx

import (
	"net/http"
	"slices"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/account"
	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/recovery"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
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
	// Recovery repairs a forgotten password (M51). Nil leaves the two endpoints
	// and the two public pages unregistered, which is what the parity test
	// against openapi.yaml compares itself to — and which is the state F141
	// describes: no route back into an account whose password was lost.
	Recovery *recovery.Service
	// Accounts ends an account's life and erases what ending it leaves behind
	// (M52). Nil leaves the endpoint and the dashboard's delete section
	// unregistered, which is what the parity test against openapi.yaml compares
	// itself to — and which is the state F44 describes: no account deletion of
	// any kind, for anybody.
	Accounts *account.Service
	// Disputes serves the blocked-attempt appeal path and the review queue. Nil
	// leaves both the endpoints and the dashboard page unregistered, which is
	// what the parity test against openapi.yaml compares itself to — and which
	// also takes the "ask for a review" button off the links form, so a refusal
	// never offers a door that is not there.
	Disputes *dispute.Service
	// Instance serves the instance-level principal's roster (D98). Nil leaves
	// the endpoints and the reviewer section of the dispute page unregistered,
	// which is what the parity test against openapi.yaml compares itself to —
	// and which leaves whoever the principal already is holding what they hold,
	// because the grants are rows rather than a running service.
	Instance *instance.Service
	Web      *Web
	// Hosts is the verified custom-hostname set (M40). Nil leaves custom domains
	// unrouted entirely — every Host header is answered exactly as it was before
	// this milestone — which is what the CLI and the tests that predate it get.
	//
	// It is the *only* thing that decides whether an alias resolves on a name
	// this operator did not configure, and it is populated by one query that
	// filters on `verified_at`.
	Hosts *redirect.HostCache
	// DomainRoot serves a verified custom hostname's own root. Nil leaves that
	// root a 404, which is where every custom hostname starts.
	DomainRoot *DomainRootRedirect
	// TLSAsk answers Caddy's on-demand TLS question. Nil leaves the endpoint
	// unregistered, and then an operator's on-demand TLS block has nothing to
	// ask — which is the correct failure, because issuing without the ask would
	// obtain certificates for names nobody has verified.
	TLSAsk *TLSAsk

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

// appMux is the application ServeMux plus a record of every pattern registered
// on it.
//
// The record exists because net/http exposes no way to enumerate a ServeMux's
// patterns, and two things downstream need that list. The root mux cannot mount
// the application tree at "/" — that belongs to the alias catch-all — so it
// mounts it path by path, and the reserved-word list has to name every
// top-level path a route occupies. Both are derived from this slice, so neither
// can fall behind a route somebody added.
//
// Recording rather than declaring is the whole point, and is D97. A hand-written
// mount list sitting beside the registrations reads as if it were checked and is
// not: it can only ever be compared against itself, which is how eleven of M42's
// and M43's routes shipped registered, reserved, linked from the nav, documented
// — and unreachable on every deployment shape (F85).
type appMux struct {
	mux      *http.ServeMux
	patterns []string
}

func newAppMux() *appMux { return &appMux{mux: http.NewServeMux()} }

func (m *appMux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.mux.Handle(pattern, h)
}

func (m *appMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(h))
}

// registerAppRoutes registers every application route — API and dashboard — on
// the application mux.
//
// Split out of NewRouter so the route set can be enumerated without building a
// whole router: the reserved-list guard registers into a throwaway appMux and
// reads what came back, rather than reading a list written beside these calls.
//
// Every top-level path registered here must also appear in
// internal/alias/reserved.txt, or a user could create an alias that shadows
// it. TestReservedListCoversRegisteredRoutes enforces that.
func registerAppRoutes(d Deps, app *appMux) {
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

		// Account recovery (M51). Unauthenticated, necessarily — the caller has
		// lost the only credential they had — and under the same `guard` as
		// login and registration, so recovery and credential guessing draw on
		// one budget. That sharing is why the milestone adds no limiter of its
		// own: a per-route bucket would let an attacker burn one without
		// touching the other.
		if d.Recovery != nil {
			rec := &RecoveryAPI{Recovery: d.Recovery}
			app.Handle("POST "+APIPrefix+"/auth/forgot", guard(http.HandlerFunc(rec.Forgot)))
			app.Handle("POST "+APIPrefix+"/auth/reset", guard(http.HandlerFunc(rec.Reset)))
		}

		// Account deletion (M52). Under the same `guard` as the credential
		// endpoints above, because the confirmation is the account's own
		// password and this is therefore a third surface on which one can be
		// guessed — sharing the bucket is what stops an attacker doubling their
		// budget by alternating between it and /login.
		//
		// **Not under `signedIn`'s web equivalent, and RequireAuth is the whole
		// gate.** Handing over every organization on the way out is exactly how
		// somebody arrives at deletion, so the caller belonging to nothing (D36)
		// is the ordinary case here rather than the exception.
		if d.Accounts != nil {
			acct := &AccountAPI{Accounts: d.Accounts, Config: d.Config}
			app.Handle("DELETE "+APIPrefix+"/account",
				guard(RequireAuth(http.HandlerFunc(acct.Delete))))
		}

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
			// QR codes (M41), nested under the link because a code is a picture
			// of that link's own short URL. No permission of their own either:
			// seeing the code is links.read and styling it is links.update — see
			// internal/link/qr.go and decisions.md, D75.
			//
			// The `.svg` and `.png` siblings are the picture and are the only
			// non-JSON responses this API has besides the spec document. Paths
			// rather than Accept negotiation, because an <img> and a download
			// both send an Accept header nobody chose. `.png` is M49, and it is
			// the one endpoint here that rasterises — internal/qr's MaxSize is
			// the bound that lets it.
			//
			// PUT rather than PATCH: an omitted style field means its default,
			// which is what makes "back to plain black on white" an empty object.
			//
			// **The five above stayed the link's *default* code (M50).** A link
			// may now carry several, and the choice m50.md required be made and
			// recorded was between growing these an identifier and leaving them
			// as the shorthand. They are the shorthand: the default code is what
			// every already-printed picture resolves to, so a client calling
			// `GET /links/{id}/qr` today goes on getting the same code tomorrow,
			// which is exactly what the contract test exists to hold.
			//
			// The `/qr/codes` collection below is where several are addressed. Its
			// members are keyed by slug rather than by id because the slug is what
			// is printed on the code and therefore the identity somebody has in
			// hand, and its `.svg`/`.png` siblings are one path segment deeper for
			// a mechanical reason: ServeMux wildcards match a whole segment, so
			// `{slug}.svg` is not a pattern that exists.
			"GET " + APIPrefix + "/links/{id}/qr":                        api.GetQR,
			"GET " + APIPrefix + "/links/{id}/qr.svg":                    api.GetQRSVG,
			"GET " + APIPrefix + "/links/{id}/qr.png":                    api.GetQRPNG,
			"PUT " + APIPrefix + "/links/{id}/qr":                        api.SetQR,
			"DELETE " + APIPrefix + "/links/{id}/qr":                     api.DeleteQR,
			"GET " + APIPrefix + "/links/{id}/qr/codes":                  api.ListQRCodes,
			"POST " + APIPrefix + "/links/{id}/qr/codes":                 api.CreateQRCode,
			"GET " + APIPrefix + "/links/{id}/qr/codes/{slug}":           api.GetQRCode,
			"PUT " + APIPrefix + "/links/{id}/qr/codes/{slug}":           api.SetQRCode,
			"DELETE " + APIPrefix + "/links/{id}/qr/codes/{slug}":        api.DeleteQRCode,
			"GET " + APIPrefix + "/links/{id}/qr/codes/{slug}/image.svg": api.GetQRCodeSVG,
			"GET " + APIPrefix + "/links/{id}/qr/codes/{slug}/image.png": api.GetQRCodePNG,
			"GET " + APIPrefix + "/folders":                              api.ListFolders,
			"POST " + APIPrefix + "/folders":                             api.CreateFolder,
			"PATCH " + APIPrefix + "/folders/{folderID}":                 api.UpdateFolder,
			"DELETE " + APIPrefix + "/folders/{folderID}":                api.DeleteFolder,
			"POST " + APIPrefix + "/folders/{folderID}/move":             api.MoveFolder,
			// Campaigns (M41). A sibling collection rather than a subresource of
			// a link, exactly as folders are: a campaign exists whether or not
			// anything carries it, and which links do is a question the links
			// list answers with `?campaign=`. Guarded by the link permissions
			// (D75), so no seed migration and no delegability call.
			"GET " + APIPrefix + "/campaigns":                 api.ListCampaigns,
			"POST " + APIPrefix + "/campaigns":                api.CreateCampaign,
			"GET " + APIPrefix + "/campaigns/{campaignID}":    api.GetCampaign,
			"PATCH " + APIPrefix + "/campaigns/{campaignID}":  api.UpdateCampaign,
			"DELETE " + APIPrefix + "/campaigns/{campaignID}": api.DeleteCampaign,
			// Webhooks (M42). A sibling collection like campaigns and folders,
			// because a webhook belongs to the workspace and hears about every
			// link in it — nesting it under one link would misdescribe what it
			// subscribes to.
			//
			// Unlike campaigns and QR codes, these carry permissions of their
			// own rather than reusing `links.*` (D75's reasoning does not reach
			// here): a webhook is an instruction to make this server connect
			// somewhere, which is a different power from editing where a
			// visitor's browser is sent. See internal/link/webhook.go.
			//
			// Rotation is a POST because it mints a credential and returns it in
			// the response body; a GET that did that would be one a browser
			// could be made to fetch.
			"GET " + APIPrefix + "/webhooks":                        api.ListWebhooks,
			"POST " + APIPrefix + "/webhooks":                       api.CreateWebhook,
			"GET " + APIPrefix + "/webhooks/{webhookID}":            api.GetWebhook,
			"PATCH " + APIPrefix + "/webhooks/{webhookID}":          api.UpdateWebhook,
			"DELETE " + APIPrefix + "/webhooks/{webhookID}":         api.DeleteWebhook,
			"POST " + APIPrefix + "/webhooks/{webhookID}/rotate":    api.RotateWebhookSecret,
			"GET " + APIPrefix + "/webhooks/{webhookID}/deliveries": api.ListWebhookDeliveries,
			// Automation rules (M43). A sibling collection for the reason
			// webhooks are one: a rule belongs to the workspace and watches
			// every link in it.
			//
			// **No run-now endpoint, deliberately.** Evaluation happens on the
			// leader-elected scheduler and nowhere else, and an endpoint that
			// ran it would put trigger matching, notification writes and link
			// archiving on the request path of whoever called it. See
			// internal/automation.
			"GET " + APIPrefix + "/automation":                   api.ListAutomationRules,
			"POST " + APIPrefix + "/automation":                  api.CreateAutomationRule,
			"GET " + APIPrefix + "/automation/{automationID}":    api.GetAutomationRule,
			"PATCH " + APIPrefix + "/automation/{automationID}":  api.UpdateAutomationRule,
			"DELETE " + APIPrefix + "/automation/{automationID}": api.DeleteAutomationRule,
			"GET " + APIPrefix + "/tags":                         api.ListTags,
			"DELETE " + APIPrefix + "/tags/{id}":                 api.DeleteTag,
			"GET " + APIPrefix + "/domain":                       api.GetDomain,
			"PATCH " + APIPrefix + "/domain":                     api.UpdateDomain,
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
			// The gate (M40). Verification is a POST because passing it starts
			// serving an alias namespace on a public hostname; the root redirect
			// is a PUT because clearing it is sending the empty string, and on a
			// PATCH that would mean "unchanged".
			"POST " + APIPrefix + "/domains/{domainID}/verify":       api.VerifyDomain,
			"PUT " + APIPrefix + "/domains/{domainID}/root-redirect": api.SetDomainRootRedirect,
			// The reputation-feed disclosure (M32). Read-only, and there is no
			// second operation here by design: the page it mirrors accepts no
			// POST (D40), and an API that could write would be the settings
			// surface D38 removed wearing a bearer token.
			"GET " + APIPrefix + "/feeds": (&FeedAPI{Links: d.Links}).Get,
		}
		for pattern, h := range protected {
			app.Handle(pattern, RequireAuth(h))
		}

		// The upload surface (M50.5), registered apart from the map above
		// because it carries a second limit.
		//
		// **A bucket of its own, and it is on the write rather than on both.**
		// Everything else under `/api/v1` is a JSON body capped at 256 KiB;
		// this one accepts up to qr.MaxLogoUploadBytes and hands them to an
		// image decoder, so what a request costs is set by its content rather
		// than by its shape and `API_RATE_PER_MIN` is a number nobody chose for
		// it. Both limits apply — the API limiter still wraps the whole tree at
		// the mount — and `UPLOAD_RATE_PER_MIN` is the narrower of the two.
		//
		// The clear is not limited beyond the API's own bound: it accepts no
		// body, decodes nothing, and throttling it would only make it harder to
		// remove a logo somebody regrets.
		//
		// **Four routes, two capabilities.** The `/qr/logo` pair is the default
		// code's, on exactly the shape D133 gave `/qr.svg` and `/qr.png`: the
		// shorthand is how a code with no slug is addressed, and the default
		// code's identity *is* the absence of one (D130). The owner overruled
		// D136 on 2026-08-07 to add it — without it a link nobody had added a
		// second code to, which is nearly every link, could carry no logo at all.
		upload := RateLimit(d.Limits.Upload, "upload", d.Metrics, nil)
		app.Handle("PUT "+APIPrefix+"/links/{id}/qr/logo",
			upload(RequireAuth(http.HandlerFunc(api.SetQRLogo))))
		app.Handle("DELETE "+APIPrefix+"/links/{id}/qr/logo",
			RequireAuth(http.HandlerFunc(api.DeleteQRLogo)))
		app.Handle("PUT "+APIPrefix+"/links/{id}/qr/codes/{slug}/logo",
			upload(RequireAuth(http.HandlerFunc(api.SetQRCodeLogo))))
		app.Handle("DELETE "+APIPrefix+"/links/{id}/qr/codes/{slug}/logo",
			RequireAuth(http.HandlerFunc(api.DeleteQRCodeLogo)))
	}

	if d.Keys != nil {
		keys := &KeyAPI{Keys: d.Keys}
		for pattern, h := range map[string]http.HandlerFunc{
			"POST " + APIPrefix + "/api-keys":        keys.Create,
			"GET " + APIPrefix + "/api-keys":         keys.List,
			"POST " + APIPrefix + "/api-keys/rotate": keys.Rotate,
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
		// The instance-wide log sits under the principal's own prefix rather
		// than beside /audit, because what it is scoped to is the thing that
		// distinguishes it and the permission it needs is the principal's.
		app.Handle("GET "+APIPrefix+"/instance/audit",
			RequireAuth(http.HandlerFunc(a.ListInstance)))
	}

	if d.Instance != nil {
		in := &InstanceAPI{Instance: d.Instance}
		for pattern, h := range map[string]http.HandlerFunc{
			"GET " + APIPrefix + "/instance/reviewers":         in.Reviewers,
			"POST " + APIPrefix + "/instance/reviewers":        in.GrantReviewer,
			"DELETE " + APIPrefix + "/instance/reviewers/{id}": in.RevokeReviewer,
		} {
			app.Handle(pattern, RequireAuth(h))
		}
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
			// Marking one unread is a DELETE of the read state rather than a
			// second verb on the same noun: `read_at` is a column with a value
			// or without one, and removing it is what this does (M48).
			"DELETE " + APIPrefix + "/notifications/{id}/read": n.MarkUnread,
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
		// Account deletion (M52), on the same limiter as the password change
		// beside it and for the same reason: both verify the account's own
		// password, so both are surfaces one can be guessed on.
		//
		// Under `signedIn` like its sibling, which means an account belonging to
		// nothing (D36) reaches it through the API rather than through this
		// page. That is the existing shape of `GET /account` rather than a
		// choice this route makes — every dashboard page but two requires an
		// organization — and changing it would be a decision about the
		// dashboard, not about deletion.
		if web.Accounts != nil {
			app.Handle("POST /account/delete", guard(signedIn(web.AccountDelete)))
		}
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

		// Account recovery (M51). Public for the reason redemption and signup
		// are — somebody who has lost their password has no session — and under
		// the same login limiter, so a reset sweep and a credential sweep draw
		// on one budget rather than two.
		//
		// Registered whether or not a relay is configured, so a mail-free
		// instance answers the refusal these handlers write rather than the
		// alias catch-all's 404. "This instance cannot send mail" and "there is
		// no such page" are different answers and only one of them is true —
		// the same reason the signup pages are registered on a closed instance.
		if web.Recovery != nil {
			app.HandleFunc("GET /forgot", web.ForgotPage)
			app.Handle("POST /forgot", guard(http.HandlerFunc(web.ForgotSubmit)))
			app.HandleFunc("GET /reset/{token}", web.ResetPage)
			app.Handle("POST /reset/{token}", guard(http.HandlerFunc(web.ResetSubmit)))
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
			// Campaigns (M41). The same three forms and the same no-JavaScript
			// shape as the folder page, minus the move: a campaign has no
			// parent, so there is nowhere to move one to. The QR form below is
			// on the link's own page rather than here, because a code belongs to
			// a link and not to a campaign.
			"GET /campaigns":                      web.CampaignsPage,
			"POST /campaigns":                     web.CampaignCreate,
			"POST /campaigns/{campaignID}":        web.CampaignUpdate,
			"POST /campaigns/{campaignID}/delete": web.CampaignDelete,
			// The QR panel's own route (M48), and the POST that has always
			// written the style. The GET is what makes the panel a panel: its
			// contents render as an ordinary page when opened directly, and the
			// popover on the link page is the same block drawn over the surface
			// it belongs to.
			"GET /links/{id}/qr":              web.LinkQRPage,
			"POST /links/{id}/qr":             web.LinkQRStyle,
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
			// Two more forms, on the same no-JavaScript pattern (M40): checking
			// the DNS record, and pointing the hostname's own root somewhere.
			// Both POST because both change something.
			"POST /domains/{domainID}/verify":        web.DomainVerify,
			"POST /domains/{domainID}/root-redirect": web.DomainRootRedirect,
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
			"POST /links/{id}/sign":    web.LinkSign,
			"POST /links/{id}/archive": web.LinkArchive,
			"POST /links/{id}/restore": web.LinkRestore,
			"POST /links/{id}/delete":  web.LinkDelete,
			// Webhooks (M42). Five forms and no JavaScript, on the folder page's
			// shape. The toggle and the rotation are their own actions rather
			// than fields on the edit form: switching a misbehaving receiver off
			// is the first thing somebody reaches for, and rotating a leaked
			// secret is the second, and neither should require opening an editor.
			"GET /webhooks":                     web.WebhooksPage,
			"POST /webhooks":                    web.WebhookCreate,
			"POST /webhooks/{webhookID}":        web.WebhookUpdate,
			"POST /webhooks/{webhookID}/toggle": web.WebhookToggle,
			"POST /webhooks/{webhookID}/rotate": web.WebhookRotate,
			"POST /webhooks/{webhookID}/delete": web.WebhookDelete,
			// Automation rules (M43). Four forms and no JavaScript, on the
			// webhooks page's shape. The toggle is its own action for the reason
			// every other toggle in this router is: pausing a standing
			// instruction that is misbehaving must not require opening an
			// editor.
			"GET /automation":                        web.AutomationPage,
			"POST /automation":                       web.AutomationCreate,
			"POST /automation/{automationID}":        web.AutomationUpdate,
			"POST /automation/{automationID}/toggle": web.AutomationToggle,
			"POST /automation/{automationID}/delete": web.AutomationDelete,
			"GET /keys":                              web.KeysPage,
			"GET /notifications":                     web.NotificationsPage,
			"POST /notifications/read":               web.NotificationReadAll,
			"POST /notifications/{id}/read":          web.NotificationRead,
			// Where a notification leads, and the undo for the read it performs
			// on the way (M48). Both POST: opening one changes state, and
			// unreading one is the correction for having done so by accident.
			"POST /notifications/{id}/open":   web.NotificationOpen,
			"POST /notifications/{id}/unread": web.NotificationUnread,
			"POST /keys":                      web.KeyCreate,
			"POST /keys/{id}/revoke":          web.KeyRevoke,
			"GET /account":                    web.AccountPage,
			// Read-only, and registered GET-only on purpose: a POST to /feeds
			// must be refused by the mux rather than by a handler somebody
			// could add one to. D40, and TestTheDisclosurePageAcceptsNoWrite.
			"GET /feeds":              web.FeedsPage,
			"POST /workspace/switch":  web.WorkspaceSwitch,
			"POST /workspace/default": web.WorkspaceDefault,
		} {
			app.Handle(pattern, signedIn(fn))
		}

		// The dashboard's own upload (M50.5), registered apart from the map
		// because it carries the same second limit the API's does.
		//
		// **The same limiter object, not a second one with the same number.** The
		// reasoning is the login guard's, one bucket above: a budget an attacker
		// can double by alternating between the API and the dashboard is not the
		// budget an operator configured. What differs is the refusal — a page
		// rather than a problem document, because a browser is what posts here.
		//
		// Removing a logo posts to `POST /links/{id}/qr` under its own button
		// name, the way reset and remove already do: it carries no body worth
		// limiting, and one action attribute per surface is what keeps a refusal
		// coming back to the form it was made from.
		app.Handle("POST /links/{id}/qr/logo",
			RateLimit(d.Limits.Upload, "upload", d.Metrics, web.tooManyRequests)(
				signedIn(web.LinkQRLogo)))

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

			// The reviewer roster shares the queue's path because it is the
			// same page, and it is registered separately because it needs a
			// dependency the queue does not: without it the section is not
			// drawn, and a route with no handler behind it would be a form
			// posting into a 404.
			if web.Instance != nil {
				for pattern, fn := range map[string]http.HandlerFunc{
					// The roster's own route (M48), the panel pattern's second
					// caller. Registered beside the writes rather than with the
					// queue for the reason the writes are: without web.Instance
					// the section is not drawn, and a page with no service
					// behind it would render a form posting into a 404.
					"GET /disputes/reviewers":              web.DisputeReviewersPage,
					"POST /disputes/reviewers":             web.DisputeReviewerGrant,
					"POST /disputes/reviewers/{id}/revoke": web.DisputeReviewerRevoke,
				} {
					app.Handle(pattern, signedIn(fn))
				}
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
}

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
// alwaysNosniff sets X-Content-Type-Options on every response a mux produces,
// including the ones no handler in this repository writes.
//
// `ServeMux` cleans an escaped path before dispatching and answers the resulting
// redirect **itself**: `GET /ld1//deep` is a 307 to `/ld1/deep` with a
// `text/html` body of its own making, and no handler here is involved, so
// nothing this project writes could set a header on it. That is F64, and it is
// pre-existing rather than M33's — `cleanPath` applies whatever patterns are
// registered — but the deep-link route is what made those cleaned paths land
// somewhere real instead of 404ing a moment later.
//
// Only nosniff, deliberately. It is the one with bite on an HTML body, it is
// already set by every handler on this tree and by the application tree's own
// middleware, so applying it everywhere changes no response this project writes.
// `X-Robots-Tag` and `Cache-Control: no-store` are **error-page** policy rather
// than tree policy — `Location` deliberately sets a different `Cache-Control`
// and no robots header, because a short link is meant to be shared — so setting
// those here would change successful redirects to fix a 307.
func alwaysNosniff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func NewRouter(d Deps) http.Handler {
	// --- application tree (authenticated, full middleware) ----------------
	app := newAppMux()
	registerAppRoutes(d, app)

	// Tell the metrics classifier what the dashboard actually is, from the mux
	// that was just handed the routes rather than from a second list somebody
	// maintains. The first copy of that list fell eleven routes behind and every
	// one of them was counted as a short link (F16).
	observability.SetWebPaths(app.mounts())

	var appHandler http.Handler = app.mux
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
		if d.Health != nil {
			mux.HandleFunc("GET /healthz", d.Health.Live)
			mux.HandleFunc("GET /readyz", d.Health.Ready)
		}
		// Caddy's on-demand TLS ask (M40, decision D3). Registered beside the
		// probes and for the same reason: it is asked during a TLS handshake, by
		// a proxy that does not know or send the operator's chosen hostname, and
		// it must answer before any application request exists.
		if d.TLSAsk != nil {
			mux.Handle("GET "+TLSAskPath, d.TLSAsk)
			mux.Handle("HEAD "+TLSAskPath, d.TLSAsk)
		}
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
			// Mounted from the patterns the application mux was actually
			// handed, not from a list written beside them. There is no second
			// list to fall behind: registering a handler is what produces its
			// mount, so a route cannot be registered and left unreachable.
			//
			// This replaced a hand-written slice that had done exactly that —
			// see appMux, and F85. The reserved-list guard now reads the same
			// registrations, so neither direction is checked against itself.
			for _, p := range app.mounts() {
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

	// A verified custom hostname's tree (M40): operational endpoints, its own
	// root, and aliases. No dashboard, no API and no static assets — a
	// customer's hostname serves links and nothing else, so a session cookie is
	// never offered a second origin and the management surface has exactly one
	// address.
	registerCustom := func(root *http.ServeMux) {
		registerOps(root)
		if d.DomainRoot != nil {
			root.Handle("GET /{$}", d.DomainRoot)
			root.Handle("HEAD /{$}", d.DomainRoot)
		}
		registerRedirect(root)
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
			link:     alwaysNosniff(linkMux),
			ops:      opsMux,
		}
	} else {
		mux := http.NewServeMux()
		registerOps(mux)
		registerApp(mux)
		registerRedirect(mux)
		root = alwaysNosniff(mux)
	}

	// Custom domains sit in front of whichever of those two this deployment is,
	// and change neither.
	//
	// A Host header that names a verified hostname is served by the tree above;
	// anything else — unknown, registered but unverified, just unverified on
	// another replica — falls through to exactly the handler it would have
	// reached before this milestone existed. That is what keeps "an unverified
	// or unknown host stays ops-only 404" true on a split-host instance without
	// changing what a single-host instance answers to a Host header it has never
	// been configured with.
	if d.Hosts != nil {
		customMux := http.NewServeMux()
		registerCustom(customMux)
		root = customHostRouter{hosts: d.Hosts, custom: alwaysNosniff(customMux), next: root}
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

// patternPath strips the optional method from a ServeMux pattern, leaving the
// path. "GET /links/{id}" becomes "/links/{id}"; "/static/" is unchanged.
func patternPath(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return strings.TrimSpace(pattern[i+1:])
	}
	return pattern
}

// mounts returns the root-mux patterns that make every route registered on the
// application mux reachable, and nothing wider.
//
// The rule is one line per registration: a single-segment path mounts itself, a
// deeper one mounts its first segment as a subtree. So "GET /webhooks" produces
// "/webhooks" and "POST /webhooks/{webhookID}/rotate" produces "/webhooks/",
// while "POST /theme" — which has no deeper route — produces "/theme" alone and
// leaves "/theme/anything" to the alias catch-all, exactly as before.
//
// The API subtree is excluded because registerApp mounts it itself, wrapped in
// the API rate limiter; mounting "/api/" here as well would hand anything under
// /api that is not /api/v1 to the application tree without that limiter.
//
// Sorted, because most registrations happen by ranging over a map literal and
// Go randomizes that order. A mount list that differed run to run would make
// the reserved-list guard's subtest names differ too.
func (m *appMux) mounts() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, pattern := range m.patterns {
		path := patternPath(pattern)
		switch {
		case path == "/{$}":
			// The root exact-match pattern. It has no segment, and "/{alias}"
			// does not match "/", so it has to be mounted verbatim.
			add("/{$}")
		case strings.HasPrefix(path, APIPrefix+"/"):
			continue
		default:
			rest := strings.TrimPrefix(path, "/")
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				add("/" + rest[:i] + "/")
			} else {
				add("/" + rest)
			}
		}
	}
	slices.Sort(out)
	return out
}

// infrastructurePatterns are the routes registered on the root mux directly
// rather than reached through the application mux: health endpoints, Caddy's
// TLS ask, the API prefix and the static tree. These are fixed and registered
// individually, so unlike the application set they are listed rather than
// derived.
var infrastructurePatterns = []string{
	"/healthz", "/readyz", TLSAskPath, APIPrefix + "/", "/static/",
}

// TLSAskPath is where Caddy's on-demand TLS `ask` is answered.
//
// A top-level path, so it is in internal/alias/reserved.txt like every other
// route: an alias called `tls-check` would otherwise shadow the endpoint that
// decides which hostnames get certificates, and
// TestReservedListCoversRegisteredRoutes is what enforces that it cannot.
const TLSAskPath = "/tls-check"

// topLevelSegments reduces root-mux patterns to the first path segment each one
// occupies — the unit internal/alias/reserved.txt is written in, because an
// alias is a single segment and that is all it can shadow.
//
// Its caller is the reserved-list guard, which feeds it the mounts derived from
// a real registration pass plus infrastructurePatterns. That is what makes the
// guard a check rather than a tautology: before F85 the same list was both the
// thing being protected and the record of what needed protecting.
func topLevelSegments(patterns []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range patterns {
		seg := strings.Trim(patternPath(p), "/")
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
