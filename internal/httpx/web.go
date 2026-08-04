package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
	"github.com/DevOfPie/LinkCtrl/internal/team"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// Web serves the HTML dashboard.
//
// Every handler here is a thin skin over the same service calls the JSON API
// makes. That is the mechanism behind the "every UI feature has API support"
// success criterion: the two surfaces cannot diverge because there is nothing
// in either of them to diverge — validation, authorization and behaviour all
// live one layer down.
type Web struct {
	UI     *ui.Renderer
	Config config.Config
	Auth   *auth.Service
	Keys   *auth.APIKeyService
	Links  *link.Service
	Stats  *analytics.Reader
	Notify *notify.Service
	// Invites serves both halves of the invitation surface: the administrator's
	// page and the public redemption form.
	Invites *invite.Service
	// Team serves member management, workspace lifecycle and the organization
	// lifecycle. Nil leaves its three pages unregistered — including the one an
	// account belonging to nothing is held on, which is why an instance wired
	// without it can never reach that state either.
	Team *team.Service
	// Signup owns whether this instance accepts new accounts, and the public
	// form. Nil leaves the signup and verification pages unregistered, and every
	// registration refused.
	Signup *signup.Service
	// Disputes serves the review queue and the appeal a refused creator files.
	// Nil leaves the page unregistered and takes the "ask for a review" button
	// off the link form, because a refusal must not offer a door that is not
	// there.
	Disputes *dispute.Service
	// Instance backs the reviewer roster on the dispute queue (D98). Nil leaves
	// the section undrawn and its two routes unregistered, which is the state a
	// deployment without the queue is already in.
	Instance *instance.Service
}

// shell is what the layout template needs on every page.
type shell struct {
	Title    string
	Nav      string
	Identity *auth.Identity
	// Unread is the notification badge. Zero renders no badge, which is also
	// what a failed count renders — see below.
	Unread int64
	// UnreadPreview is what the bell shows when it is opened: the newest unread
	// notifications, cut at notify.PreviewLimit. It arrives with Unread from the
	// same call, so the bell costs the badge's query and not a second one.
	//
	// Empty is the ordinary state of a new account, and the bell renders an
	// empty state rather than an empty box for it.
	UnreadPreview []notify.Notification
	// Theme is the explicit override, or "" to follow prefers-color-scheme.
	// Rendered as an attribute on <html> by the layout, so the first response
	// is already in the right theme and there is no correcting script — the
	// flash of the wrong theme is unrepresentable rather than suppressed.
	Theme string
	// Path is where the appearance control should return to after its POST. It
	// is on every shell rather than on the two pages that render the control,
	// because a third render site should not also need a handler change.
	Path string
	// Workspaces is every workspace the signed-in user may act in. On the shell
	// because the switcher belongs to the page chrome: a person changes
	// workspace from wherever they happen to be, not by navigating to a page
	// about workspaces first.
	//
	// The layout draws nothing when there is one of them, which is every
	// instance today. It is still loaded, at the cost of one indexed query per
	// render — the same trade the unread badge makes — because a switcher that
	// only appears after a page refresh is worse than the query.
	Workspaces []auth.Workspace
	// HasOrganization is false for an account that belongs to nothing, which
	// D36 made a state a signed-in person can legitimately be in. The header
	// draws its destinations from it: every one of them leads somewhere that
	// needs an organization, so offering them would be offering a redirect back
	// to the page the reader is already on.
	//
	// Not an authorization check. What such an account may do is decided by its
	// empty permission set, in the services, like everybody else's.
	HasOrganization bool
}

func (h *Web) shell(r *http.Request, title, nav string) shell {
	s := shell{
		Title:           title,
		Nav:             nav,
		Identity:        IdentityFrom(r.Context()),
		Theme:           themeFrom(r),
		Path:            r.URL.Path,
		HasOrganization: IdentityFrom(r.Context()).HasOrganization(),
	}
	// One notification query per page render, served by the partial index the
	// table ships with, answering the badge count and the bell's preview
	// together. An error is swallowed to zero rather than propagated: this is a
	// badge, and failing a page an operator asked for because a decoration could
	// not be computed is the wrong trade.
	if h.Notify != nil && s.Identity != nil {
		if n, items, err := h.Notify.UnreadPreview(r.Context(), s.Identity, notify.PreviewLimit); err == nil {
			s.Unread, s.UnreadPreview = n, items
		}
	}
	// Same trade for the switcher: a page whose content the reader asked for
	// must not fail because the chrome could not be drawn. An empty list renders
	// no switcher, which is what a single-membership account gets anyway.
	if h.Auth != nil && s.Identity != nil {
		if ws, err := h.Auth.Workspaces(r.Context(), s.Identity); err == nil {
			s.Workspaces = ws
		}
	}
	return s
}

// maxFormBytes caps HTML form bodies. Far above any real form, far below
// anything that could tie up memory.
const maxFormBytes = 64 * 1024

func parseForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	return r.ParseForm()
}

// isHTMX reports whether the request came from an hx-* attribute rather than a
// full navigation.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// seeOther sends the browser somewhere else, in whichever dialect it speaks.
// (Named for its status code; "redirect" is taken by the package that serves
// short links.)
//
// An htmx request follows HTTP redirects transparently and swaps the *target
// page* into the fragment it was updating — a login page rendered inside a
// table is the classic symptom. HX-Redirect makes htmx perform a full
// navigation instead. 303 for everyone else, so a redirected POST becomes a
// GET and refresh does not offer to resubmit the form.
func seeOther(w http.ResponseWriter, r *http.Request, to string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusOK)
		return
	}
	// gosec's taint analysis cannot see through safeNext, which is the only path
	// by which a caller-supplied destination reaches here — it rejects anything
	// that is not a local path, including the "//evil.com" form that beats a naive
	// "starts with /" check. Every other call site passes a literal.
	http.Redirect(w, r, to, http.StatusSeeOther) //nolint:gosec // G710: destinations are literals or pass safeNext
}

// render writes a page, downgrading a template failure to a plain 500.
//
// Pages are personal, so no-store: the back button after signing out must not
// replay the dashboard out of the browser cache.
func (h *Web) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	w.Header().Set("Cache-Control", "no-store")
	if err := h.UI.Render(w, status, page, data); err != nil {
		observability.LoggerFrom(r.Context()).Error("render failed",
			slog.String("page", page), slog.Any("error", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (h *Web) renderPartial(w http.ResponseWriter, r *http.Request, page, block string, data any) {
	w.Header().Set("Cache-Control", "no-store")
	if err := h.UI.RenderPartial(w, http.StatusOK, page, block, data); err != nil {
		observability.LoggerFrom(r.Context()).Error("render partial failed",
			slog.String("page", page), slog.String("block", block), slog.Any("error", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// errorPageData is what pages/error.html renders.
type errorPageData struct {
	shell
	Code    int
	Heading string
	Message string
}

func (h *Web) errorPage(w http.ResponseWriter, r *http.Request, code int, heading, message string) {
	h.render(w, r, code, "error", errorPageData{
		shell:   h.shell(r, heading, ""),
		Code:    code,
		Heading: heading,
		Message: message,
	})
}

// tooManyRequests is the dashboard's counterpart of writeTooManyRequests: a
// page rather than a problem document, because the client is a person.
//
// Rendered rather than redirected back to the form, so the browser does not
// resubmit whatever was refused when the reader reloads.
func (h *Web) tooManyRequests(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusTooManyRequests, "Too many attempts",
		"Too many attempts from your address. Wait a moment and try again.")
}

// webError maps a service error to an HTML response, the counterpart of
// WriteError for pages. Validation errors do not come through here — handlers
// re-render their form with the field messages instead.
func (h *Web) webError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		h.errorPage(w, r, http.StatusNotFound, "Not found",
			"This page or link does not exist, or it belongs to a different workspace.")
	case errors.Is(err, domain.ErrForbidden):
		h.errorPage(w, r, http.StatusForbidden, "Not allowed", err.Error())
	case errors.Is(err, domain.ErrConflict):
		// A refusal with a reason the reader can act on — "delete the links
		// first", "make somebody else an owner first". Pages that can put it
		// beside the list it is about do so; this is the answer for the ones
		// that cannot.
		h.errorPage(w, r, http.StatusConflict, "Not allowed yet", conflictMessage(err))
	case errors.Is(err, domain.ErrUnauthorized),
		errors.Is(err, auth.ErrSessionNotFound),
		errors.Is(err, auth.ErrSessionExpired),
		errors.Is(err, auth.ErrSessionRevoked):
		seeOther(w, r, "/login")
	default:
		observability.LoggerFrom(r.Context()).Error("unhandled web error",
			slog.Any("error", err), slog.String("path", r.URL.Path))
		h.errorPage(w, r, http.StatusInternalServerError, "Something went wrong",
			"The error has been logged. Try again, and check the server logs if it persists.")
	}
}

// fieldErrors flattens validation errors for the templates: one message per
// field, plus a banner message for anything without a field.
func fieldErrors(err error) (map[string]string, string) {
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		return map[string]string{}, ""
	}
	fields := make(map[string]string, len(ve))
	var general []string
	for _, fe := range ve {
		if fe.Field == "" || fe.Field == "body" {
			general = append(general, fe.Message)
			continue
		}
		// First message per field wins; a form can highlight one thing at a
		// time anyway.
		if _, ok := fields[fe.Field]; !ok {
			fields[fe.Field] = fe.Message
		}
	}
	return fields, strings.Join(general, " ")
}

// conflictMessage is the readable half of a domain.ErrConflict, or "" for any
// other error.
//
// Conflicts from the team service are written as `%w: <instruction>`, and the
// instruction is the whole value of them — "delete the links first" tells
// somebody what to do, where "conflict: delete the links first" reads like a
// fault. Anything that is not a conflict returns empty so callers fall through
// to their ordinary error path rather than showing a bare sentinel.
func conflictMessage(err error) string {
	if !errors.Is(err, domain.ErrConflict) {
		return ""
	}
	if _, rest, ok := strings.Cut(err.Error(), ": "); ok && rest != "" {
		return rest
	}
	return err.Error()
}

// RequireWebAuth is RequireAuth for pages: anonymous requests are sent to the
// login form with a way back, not handed a JSON problem they cannot read.
func (h *Web) RequireWebAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IdentityFrom(r.Context()) == nil {
			to := "/login"
			if r.Method == http.MethodGet && r.URL.Path != "/" {
				to += "?next=" + safeNextParam(r.URL.RequestURI())
			}
			seeOther(w, r, to)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// safeNext validates a post-login destination. Only a local path is accepted:
// anything else would make the login form an open redirect, and "//evil.com"
// is the classic way a naive "starts with /" check gets beaten.
func safeNext(raw string) string {
	if raw == "" || raw[0] != '/' ||
		strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n") {
		return "/dashboard"
	}
	return raw
}

// safeNextParam applies the same rule before the value goes into a query
// string, returning it escaped.
func safeNextParam(raw string) string {
	return strings.ReplaceAll(safeNext(raw), "&", "%26")
}

// --- root, login, setup, logout ---------------------------------------------

// Root sends / wherever makes sense for the visitor.
func (h *Web) Root(w http.ResponseWriter, r *http.Request) {
	if IdentityFrom(r.Context()) != nil {
		seeOther(w, r, "/dashboard")
		return
	}
	seeOther(w, r, "/login")
}

type loginPageData struct {
	shell
	Email  string
	Next   string
	Error  string
	Notice string
	// SignupOpen draws the "create an account" link. A link that leads to a
	// refusal is worse than no link, and on an instance with no mailer `open`
	// is not open — so this is the effective mode and not the configured one.
	SignupOpen bool
}

func (h *Web) LoginPage(w http.ResponseWriter, r *http.Request) {
	if IdentityFrom(r.Context()) != nil {
		seeOther(w, r, "/dashboard")
		return
	}
	// A fresh instance has nobody to sign in; take the operator to setup
	// instead of a form that can only fail.
	if needs, err := h.Auth.NeedsSetup(r.Context()); err == nil && needs {
		seeOther(w, r, "/setup")
		return
	}

	data := loginPageData{shell: h.shell(r, "Sign in", ""), SignupOpen: h.signupOpen()}
	if r.URL.Query().Get("next") != "" {
		data.Next = safeNext(r.URL.Query().Get("next"))
	}
	if r.URL.Query().Get("signedout") == "1" {
		data.Notice = "You have been signed out."
	}
	// Where a completed verification lands. The account exists and the address
	// is proven; all that is left is the password they chose at the form.
	if r.URL.Query().Get("verified") == "1" {
		data.Notice = "Your address is confirmed and your account is ready. Sign in below."
	}
	h.render(w, r, http.StatusOK, "login", data)
}

func (h *Web) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	res, err := h.Auth.Login(r.Context(), auth.LoginInput{
		Email:     r.PostFormValue("email"),
		Password:  r.PostFormValue("password"),
		IP:        ClientIPFrom(r.Context()),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		data := loginPageData{
			shell:      h.shell(r, "Sign in", ""),
			Email:      r.PostFormValue("email"),
			Next:       safeNext(r.PostFormValue("next")),
			SignupOpen: h.signupOpen(),
		}
		switch {
		case isCredentialFailure(err):
			// One message for every credential failure, same as the API, and
			// the same words in the same order — SignInRefusedDetail is shared
			// rather than restated, because a form that said "the account is
			// locked briefly" while the API said "the email or password is
			// incorrect" is the second half of finding F92. Whoever wanted to
			// know whether an address was registered only had to ask the surface
			// that would tell them.
			data.Error = SignInRefusedDetail
		default:
			h.webError(w, r, err)
			return
		}
		h.render(w, r, http.StatusUnauthorized, "login", data)
		return
	}

	maxAge := int(h.Config.Auth.SessionAbsoluteTTL.Seconds())
	http.SetCookie(w, NewSessionCookie(res.Token, h.Config.SecureCookies, maxAge))
	seeOther(w, r, safeNext(r.PostFormValue("next")))
}

func (h *Web) Logout(w http.ResponseWriter, r *http.Request) {
	if id := IdentityFrom(r.Context()); id != nil && !id.IsAPIKey() {
		if err := h.Auth.Logout(r.Context(), id.SessionID); err != nil {
			h.webError(w, r, err)
			return
		}
	}
	http.SetCookie(w, ClearSessionCookie(h.Config.SecureCookies))
	seeOther(w, r, "/login?signedout=1")
}

type setupPageData struct {
	shell
	Name  string
	Email string
	Error string
}

func (h *Web) SetupPage(w http.ResponseWriter, r *http.Request) {
	needs, err := h.Auth.NeedsSetup(r.Context())
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if !needs {
		// Same shape as the API: once claimed, the page is gone, and saying
		// "gone" reveals nothing an attacker can use.
		seeOther(w, r, "/login")
		return
	}
	h.render(w, r, http.StatusOK, "setup", setupPageData{shell: h.shell(r, "Set up", "")})
}

func (h *Web) SetupSubmit(w http.ResponseWriter, r *http.Request) {
	needs, err := h.Auth.NeedsSetup(r.Context())
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if !needs {
		seeOther(w, r, "/login")
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	email := r.PostFormValue("email")
	password := r.PostFormValue("password")

	fail := func(msg string) {
		h.render(w, r, http.StatusUnprocessableEntity, "setup", setupPageData{
			shell: h.shell(r, "Set up", ""),
			Name:  r.PostFormValue("name"),
			Email: email,
			Error: msg,
		})
	}

	if len(password) < auth.MinPasswordLength {
		fail("The password must be at least 12 characters.")
		return
	}

	if _, err := h.Auth.Register(r.Context(), auth.RegisterInput{
		Email: email, Name: r.PostFormValue("name"), Password: password, IsFirstUser: true,
	}); err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidEmail):
			fail("That does not look like an email address.")
		case errors.Is(err, auth.ErrEmailTaken):
			fail("That email address is already registered.")
		default:
			h.webError(w, r, err)
		}
		return
	}

	// Sign the new owner straight in: the credentials were in hand this one
	// time, and bouncing to a login form to retype them helps nobody.
	res, err := h.Auth.Login(r.Context(), auth.LoginInput{
		Email: email, Password: password,
		IP: ClientIPFrom(r.Context()), UserAgent: r.UserAgent(),
	})
	if err != nil {
		seeOther(w, r, "/login")
		return
	}
	maxAge := int(h.Config.Auth.SessionAbsoluteTTL.Seconds())
	http.SetCookie(w, NewSessionCookie(res.Token, h.Config.SecureCookies, maxAge))
	seeOther(w, r, "/dashboard")
}
