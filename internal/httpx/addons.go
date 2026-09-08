package httpx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// This file is the application tree's half of the routes limb (M64): the one
// pattern add-on pages arrive on, and the rule for turning what a module
// answered into a response.
//
// # One pattern, and dispatch inside it
//
// `/addons/{addon}/{rest...}` and its bare form, registered once. Not a pattern
// per add-on, and that is a decision rather than a shortcut: the route set is
// then a property of this build rather than of whichever modules happened to be
// in a directory at boot, so the reserved-word guard, the mount list and the
// metrics classifier all see the same thing on every instance. An add-on that is
// not installed, or that did not declare `routes.own_prefix`, is a 404 from the
// handler — the same answer a mistyped path gets, because which add-ons an
// instance runs is not something an anonymous visitor is owed. It is also free:
// that 404 spends nothing, which is D309 and the paragraph below.
//
// # Public, and why
//
// D261. These routes are not behind `signedIn`: an authentication add-on's whole
// purpose is to start a flow for somebody who has no session yet, so requiring
// one would make the M65 hook unreachable through the surface M64 built for it.
// What an add-on learns about who is signed in is a separate grant
// (`session.context`) and a separate record with no credential in it. Public is
// not unbounded: D261's second half — no limiter here — was overturned by D305
// once M65 gave this prefix a way to mint, and **every route an add-on serves is
// charged against the login budget** in internal/httpx/router.go, whatever that
// add-on declared.
//
// **A request that reaches no add-on is not one of those routes** (D309). It is
// refused on shape — by [Web.addonRouteExists], which the limiter asks before it
// charges and this handler asks before it reads a body — so a scanner walking
// `/addons/nosuch/wp-login.php` cannot spend the budget somebody else needs to
// sign in. The precedent is the 404-probe limiter, which has charged a miss and
// never a hit since M13.
//
// # The add-on does not choose HTML
//
// The whole of the CSP answer. A module's body is text; the host either serves it
// as one of two non-executable media types or wraps it in the dashboard's own
// page template, where html/template escapes it. `internal/httpx/middleware.go`'s
// csp constant is untouched — there is nothing here for it to permit.

// AddonRouter is what this package needs from the add-on host. An interface, so
// the tests that assert what reaches a browser can hand over a module's answer
// without a wasm runtime, and so this package does not have to construct one.
type AddonRouter interface {
	Route(ctx context.Context, name string, in addon.RequestIn) (addon.Response, error)
	// ServesRoutes answers whether a module of this name would be handed a
	// request at all — the same test Route makes before it does anything, asked
	// without doing any of it. It is what turns *this path names no add-on* into
	// an answer available before the limiter charges and before a body is read.
	//
	// The two must agree, and the way they are kept agreeing is that the host
	// answers this from the same loaded set and the same two conditions Route
	// selects on. A false yes here is a 404 charged against the sign-in budget,
	// which is the defect this method exists to remove; a false no is a live
	// add-on answering 404, which the wiring test would catch.
	ServesRoutes(name string) bool
}

// AddonPagePattern is the route add-on pages arrive on, and AddonBarePattern is
// the same prefix with nothing after it.
//
// Two patterns because `{rest...}` does not match an empty remainder: without
// the second, `/addons/oidc` would fall through to the alias catch-all and
// answer "no such link" for a path that plainly names an add-on.
const (
	AddonPagePattern = addon.RoutePrefix + "{addon}/{rest...}"
	AddonBarePattern = addon.RoutePrefix + "{addon}"
)

// maxAddonRequestBody caps what is read from a request before it is handed to a
// module.
//
// The same bound as an HTML form's (maxFormBytes), and it is the coarse gate
// rather than the deciding one. What decides is the *record*: internal/addon
// refuses a request whose encoded record does not fit one ABI value, and a body
// is only part of that record — the envelope is beside it, a body that is not
// UTF-8 is base64 first, and both are inside the JSON encoding, where a control
// character costs six bytes. So the effective ceiling on a body is a function of
// its bytes and not a number that can be written here, and this constant's job is
// only to stop the host reading a body that could not be part of any record it
// will accept — the record bound is the same 64 KiB, and the envelope is always
// beside the body inside it.
//
// Read through MaxBytesReader so an oversized body is refused rather than
// truncated into something a module would parse as complete — and refused with
// the same 413 the record bound answers, because both are the same fact about the
// same request.
const maxAddonRequestBody = 64 * 1024

// addonPageData is what pages/addon.html renders.
type addonPageData struct {
	shell
	// Addon is the add-on's name, which is also the segment of the URL it was
	// reached through. It is the only thing this page says about the add-on
	// itself: how an installed add-on is *presented* is M68's, and inventing a
	// heading here would be that decision made in the wrong milestone.
	Addon string
	// Body is what the module answered, as text. Rendered through
	// html/template's escaping like every other value on every other page, which
	// is what makes a script tag in it a script tag on the screen.
	Body string
}

// AddonPage serves one request to one add-on.
//
// **The shape is refused before anything is spent.** A path under this prefix
// naming no add-on this instance serves is a 404 answered here, and the limiter
// in front of the handler asked the same question and let it through uncharged —
// see [Web.addonRouteExists] and D309.
func (h *Web) AddonPage(w http.ResponseWriter, r *http.Request) {
	if !h.addonRouteExists(r) {
		h.errorPage(w, r, http.StatusNotFound, "Not found", addonNotFound)
		return
	}
	name := r.PathValue("addon")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAddonRequestBody))
	if err != nil {
		h.errorPage(w, r, http.StatusRequestEntityTooLarge, "Too large",
			"That request carries more than an add-on may be handed.")
		return
	}

	in := addon.RequestIn{
		Method: r.Method,
		// The two facts the host needs about the request and the guest never sees.
		// They go no further than internal/addon's own state: no ABI record carries
		// either, and the address becomes a /24 or /48 prefix inside internal/auth
		// before it reaches a column. They are here because a session minted through
		// `session_mint` must record where the sign-in came from exactly as one
		// minted by the sign-in form does — a session row with no `ip_prefix` and no
		// `user_agent` is a session an operator cannot recognise in the list on their
		// own account page.
		ClientIP:  ClientIPFrom(r.Context()),
		UserAgent: r.UserAgent(),
		// Whoever is signed in, or nil. **The identity and not a record built from
		// it**: internal/addon owns the mapping onto the SessionContext record a
		// module sees, and owns it because the record's whole claim — nothing in it
		// is a credential — is a property of that mapping. It is also the actor a
		// link is written for, and an actor derived from a record a manifest can
		// cause to be blanked would be an actor a manifest could erase.
		Identity: IdentityFrom(r.Context()),
		// Everything after the add-on's own segment, always rooted — the leading
		// slash is added here so a module never has to branch on whether it was
		// reached at its root.
		//
		// **The escapes are decoded, and a module cannot undo that.** PathValue
		// answers the remainder with its percent-escapes resolved, so
		// `/addons/x/a%2fb` arrives as `/a/b` and is indistinguishable from
		// `/addons/x/a/b`; measured, along with `%2e%2e%2f%2e%2e%2f` arriving as
		// `/../../dashboard` and `%00` arriving as a NUL byte. What a module may
		// conclude from this string is therefore: that ServeMux matched it inside
		// this add-on's own prefix, and nothing else. It is not a filesystem path,
		// its segments are not a security boundary, and a module dispatching on
		// them must compare whole segments rather than search for a substring. The
		// host reads none of it — the prefix was decided by the pattern, before
		// this value existed — so a traversal in it reaches nothing of LinkCtrl's.
		// A module that wants the untouched form has to be given one, and it is
		// not: adding r.URL.RawPath here would hand a guest a second spelling of
		// the same request to disagree with itself about.
		Path:           "/" + strings.TrimPrefix(r.PathValue("rest"), "/"),
		Query:          r.URL.RawQuery,
		ContentType:    r.Header.Get("Content-Type"),
		AcceptLanguage: r.Header.Get("Accept-Language"),
		Body:           body,
		// Every cookie the browser sent, including this product's session cookie.
		// The host is what filters them down to the prefixes the add-on's manifest
		// declared, because the host is what knows the manifest — see
		// addon.RequestIn.
		Cookies: r.Cookies(),
	}

	resp, err := h.Addons.Route(r.Context(), name, in)
	if err != nil {
		h.addonFailed(w, r, name, err)
		return
	}
	h.writeAddonResponse(w, r, name, resp)
}

const addonNotFound = "No add-on serves this address on this instance."

// addonRouteExists reports whether this request names an add-on that would be
// handed it, and it is the only place that question is answered (D309).
//
// It decides two things at once, deliberately: the 404 above, and whether the
// login limiter in internal/httpx/router.go may charge the request. They have to
// be one function, because the defect this repairs was the two disagreeing —
// every miss under `/addons/` spent the budget a person needs to sign in, so two
// GETs to `/addons/nosuch/wp-login.php` and `/addons/nosuch/xmlrpc.php` from an
// ordinary scanner denied `POST /login` on an instance with `Login = 2`, and with
// `TRUSTED_PROXIES` unset denied it for everybody at once.
//
// **The rule D305 set is untouched**: every add-on route is charged, whatever the
// add-on behind it declares, because protection keyed on a grant in a manifest is
// protection the next grant can move out of reach. What this says is only that a
// path reaching no add-on is not an add-on route. It is the shape the 404-probe
// limiter has had since M13 — a miss is charged and a hit is not — with the
// direction reversed, because there the miss is the abuse and here the miss is
// the thing nobody's sign-in should pay for.
//
// Cheap on purpose: a map-free walk of the loaded set, no body read, no instance,
// no lock. It runs on every request under the prefix including the ones being
// limited, so anything expensive here would be the flood it is defending against.
func (h *Web) addonRouteExists(r *http.Request) bool {
	if h.Addons == nil {
		return false
	}
	name := r.PathValue("addon")
	if name == "" {
		return false
	}
	return h.Addons.ServesRoutes(name)
}

// writeAddonResponse turns a module's answer into an HTTP response.
func (h *Web) writeAddonResponse(w http.ResponseWriter, r *http.Request,
	name string, resp addon.Response) {
	// The cookies first, because a redirect writes its header immediately below
	// and a Set-Cookie added after that is a Set-Cookie nobody receives.
	//
	// **This loop writes the host's jar and never a module's own list** (F289).
	// What a module named is inside `resp.Jar`'s value by the time it arrives
	// here, and `resp.SetCookie` is empty — so the number of Set-Cookie headers
	// an add-on's response carries is at most two, whatever the module answered
	// and however many times it is visited, and a browser's cookie store cannot
	// be filled by an add-on until it evicts this product's own session cookie.
	// internal/addon owns that packing because it owns the manifest; what is left
	// here is the scoping, which was always the host's.
	//
	// **Before the minted branch, and that is a repair.** The two orders differ
	// only when a second factor is owed — that branch answers the request itself
	// and returns — and writing the session first meant an add-on's own cookies
	// were dropped for TOTP accounts and only for TOTP accounts. What a module
	// wrote its jar for is its flow's state: a callback that clears the `state`
	// cookie it set at the start left it set, on exactly the accounts whose
	// sign-in takes another step. A module cannot see which accounts those are,
	// so it could not have compensated. Nothing about a jar depends on whether a
	// session was minted, so nothing about writing it does either.
	for _, c := range resp.Jar {
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is config-driven, like every other cookie this product sets; HttpOnly and SameSite are hardcoded two lines below and an add-on cannot opt out of either
			Name:  c.Name,
			Value: c.Value,
			// The path is the add-on's own prefix, not a choice the module makes: a
			// cookie an add-on could scope for itself is one it could scope to the
			// whole origin, and then two add-ons are in each other's namespace on
			// every request. The attributes below are the host's for the same reason
			// — an add-on cannot opt out of HttpOnly or of SameSite.
			Path:     addon.RoutePrefix + name + "/",
			MaxAge:   c.MaxAge,
			Secure:   h.Config.SecureCookies,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	// The session the host minted while the module was answering, if it minted one
	// (M65). **Before the redirect**, because a redirect writes its header
	// immediately below and a Set-Cookie added after that is a Set-Cookie nobody
	// receives — and because a second factor replaces the module's own answer
	// outright.
	//
	// Nothing a module wrote reaches this branch. `resp.Minted` is written only by
	// the `session_mint` host function, after internal/auth agreed that an account
	// exists for the assertion, is active, is not locked out, and either has no
	// second factor or has one this operator said the provider satisfied.
	if resp.Minted != nil && h.mintedSession(w, r, name, resp.Minted, resp.Location) {
		return
	}

	if resp.Location != "" {
		// 302, always. The status was checked when the module wrote it, so a
		// permanent redirect was refused there rather than downgraded here — the
		// inherited rule is that this product never answers one, and this route is
		// on the application tree where a browser caches aggressively.
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, resp.Location, http.StatusFound)
		return
	}

	// A status the host can write. internal/addon defaults an unset one to 200
	// when it checks the record, so this is belt against a caller that did not go
	// through that check — and belt worth having, because http.ResponseWriter
	// panics on a zero status rather than answering anything.
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}

	if resp.ContentType == addon.ContentTypeWrapped {
		// The host draws the page. The module's body is a value in it, so
		// html/template decides the escaping context and a module that answered
		// with markup gets the characters of markup.
		h.render(w, r, resp.Status, "addon", addonPageData{
			shell: h.shell(r, name, ""),
			Addon: name,
			Body:  resp.Body,
		})
		return
	}

	// One of the two media types an add-on may name for itself, neither of which
	// a browser executes. nosniff is set by the application tree's own middleware
	// on every response, so neither can be sniffed into something that is.
	w.Header().Set("Content-Type", resp.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.Status)
	if _, err := io.WriteString(w, resp.Body); err != nil {
		observability.LoggerFrom(r.Context()).Debug("writing an add-on's response failed",
			slog.String("addon", name), slog.Any("error", err))
	}
}

// mintedSession writes what an add-on's assertion produced, and reports whether
// it has answered the request itself.
//
// Two outcomes, and only one of them lets the module's own response through:
//
//   - **A session**: the cookie is written here — by the same constructor the
//     sign-in form uses, with the same attributes and the same lifetime — and the
//     module's response is then served as it would have been. That is what lets an
//     add-on redirect somebody to wherever its flow was going while the browser
//     picks up a session on the way.
//   - **A second factor still owed**: the host takes the request over. The pending
//     credential is the host's, it lives for minutes, and there is no shape in
//     which a module holds one — so the module's `location` becomes the `next` of
//     the host's own prompt rather than where the browser goes now. `safeNext`
//     bounds that: an add-on's location may legitimately be an external URL (an
//     authorization endpoint is one), and an external URL is not something the
//     sign-in flow may be pointed at, so anything not local becomes the dashboard.
//
// It returns true only in the second case, which is the one where the module's
// answer has been replaced.
func (h *Web) mintedSession(w http.ResponseWriter, r *http.Request,
	name string, minted *addon.Minted, location string) bool {
	if minted.SecondFactorRequired {
		observability.LoggerFrom(r.Context()).Info(
			"an add-on's assertion was accepted and the account still owes a second factor",
			slog.String("addon", name))
		seeOther(w, r, "/login/code?t="+url.QueryEscape(minted.PendingToken.Reveal())+
			"&next="+url.QueryEscape(safeNext(location)))
		return true
	}
	maxAge := int(h.Config.Auth.SessionAbsoluteTTL.Seconds())
	http.SetCookie(w, NewSessionCookie(minted.Token.Reveal(), h.Config.SecureCookies, maxAge))
	return false
}

// addonFailed maps a routing failure to a page.
//
// Nothing the module said reaches the reader. A wasm trap names the guest's own
// symbols and a refused response names the field that was wrong; both are for
// the operator's log, which the host has already written them to.
func (h *Web) addonFailed(w http.ResponseWriter, r *http.Request, name string, err error) {
	switch {
	case errors.Is(err, addon.ErrNoRoute):
		h.errorPage(w, r, http.StatusNotFound, "Not found", addonNotFound)
	case errors.Is(err, addon.ErrRequestTooLarge):
		// The client's own doing, so the same answer MaxBytesReader's refusal gets
		// and no error line: an operator reading the log must not be told an add-on
		// failed because somebody posted a large form to it.
		h.errorPage(w, r, http.StatusRequestEntityTooLarge, "Too large",
			"That request carries more than an add-on may be handed.")
	case errors.Is(err, addon.ErrBusy):
		// The concurrency bound, reached. A person, so a page and not a problem
		// document, and the same wording the rate limiter uses for the same reason:
		// waiting is the whole of the advice.
		h.errorPage(w, r, http.StatusServiceUnavailable, "Busy",
			"This add-on is handling as many requests as it can. Try again in a moment.")
	default:
		observability.LoggerFrom(r.Context()).Error("an add-on could not answer",
			slog.String("addon", name), slog.Any("error", err))
		h.errorPage(w, r, http.StatusBadGateway, "Add-on failed",
			"The add-on serving this page did not answer. The error is in the server log.")
	}
}
