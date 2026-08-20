package httpx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

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
// instance runs is not something an anonymous visitor is owed.
//
// # Public, and why
//
// D261. These routes are not behind `signedIn`: an authentication add-on's whole
// purpose is to start a flow for somebody who has no session yet, so requiring
// one would make the M65 hook unreachable through the surface M64 built for it.
// What an add-on learns about who is signed in is a separate grant
// (`session.context`) and a separate record with no credential in it, and what
// bounds the cost of an anonymous request is the host's own concurrency bound.
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
	Route(ctx context.Context, name string, in addon.RequestIn,
		sess addon.SessionContext) (addon.Response, error)
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
func (h *Web) AddonPage(w http.ResponseWriter, r *http.Request) {
	if h.Addons == nil {
		h.errorPage(w, r, http.StatusNotFound, "Not found", addonNotFound)
		return
	}
	name := r.PathValue("addon")
	if name == "" {
		h.errorPage(w, r, http.StatusNotFound, "Not found", addonNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAddonRequestBody))
	if err != nil {
		h.errorPage(w, r, http.StatusRequestEntityTooLarge, "Too large",
			"That request carries more than an add-on may be handed.")
		return
	}

	in := addon.RequestIn{
		Method: r.Method,
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

	resp, err := h.Addons.Route(r.Context(), name, in, addonSession(r))
	if err != nil {
		h.addonFailed(w, r, name, err)
		return
	}
	h.writeAddonResponse(w, r, name, resp)
}

const addonNotFound = "No add-on serves this address on this instance."

// addonSession is the SessionContext record for whoever is signed in, and the
// zero value for nobody.
//
// Nobody is the ordinary case rather than an error: these routes are public, so
// a module drawing a sign-in page reads `signed_in: false` and gets on with it.
// Nothing here is a credential — no cookie, no token, no session identifier —
// which is D232 applied to the read half and is asserted at the ABI's surface.
func addonSession(r *http.Request) addon.SessionContext {
	id := IdentityFrom(r.Context())
	if id == nil {
		return addon.SessionContext{}
	}
	out := addon.SessionContext{
		SignedIn:    true,
		UserID:      id.UserID.String(),
		Email:       id.Email,
		DisplayName: id.Name,
		Role:        id.Role,
	}
	// The zero UUID crosses as an empty string rather than as a run of zeroes: a
	// module comparing what it was handed against "" is doing the obvious thing,
	// and a module that stored 00000000-0000-0000-0000-000000000000 as a tenant
	// would have stored a tenant that does not exist.
	if id.WorkspaceID != uuid.Nil {
		out.WorkspaceID = id.WorkspaceID.String()
	}
	if id.OrgID != uuid.Nil {
		out.OrganizationID = id.OrgID.String()
	}
	return out
}

// writeAddonResponse turns a module's answer into an HTTP response.
func (h *Web) writeAddonResponse(w http.ResponseWriter, r *http.Request,
	name string, resp addon.Response) {
	// The cookies first, because a redirect writes its header immediately below
	// and a Set-Cookie added after that is a Set-Cookie nobody receives.
	for _, c := range resp.SetCookie {
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
