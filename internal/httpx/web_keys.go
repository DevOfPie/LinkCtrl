package httpx

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// keyRow is APIKeyInfo plus the derived flags the template needs.
type keyRow struct {
	auth.APIKeyInfo
	Expired bool
	// Superseded is a key mid-grace: rotated, still verifying, on a deadline.
	// A separate flag from Expired because the state reads differently — this
	// one is a key somebody is in the middle of replacing, and the row says
	// when it stops.
	Superseded bool
}

type keysPageData struct {
	shell
	Keys         []keyRow
	ScopeOptions []string
	Created      *auth.CreatedAPIKey
	// CanCreateOrgWide decides whether the workspace choice is offered at all.
	// Somebody whose role reaches one workspace never sees the control, rather
	// than seeing it and being refused.
	CanCreateOrgWide bool
	Form             struct {
		Name string
		// Reach is the form's own value for the control, not a derived flag, so a
		// refused submission redraws what was chosen rather than what it decayed
		// into. Three options since M54 — "workspace", "organization",
		// "account" — where there used to be two.
		Reach string
	}
	FieldErrors map[string]string
	Notice      string
	Error       string
}

func (h *Web) loadKeysPage(w http.ResponseWriter, r *http.Request) (keysPageData, bool) {
	actor := IdentityFrom(r.Context())
	data := keysPageData{
		shell:       h.shell(r, "API keys", "keys"),
		FieldErrors: map[string]string{},
	}

	keys, err := h.Keys.List(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	now := time.Now()
	for _, k := range keys {
		data.Keys = append(data.Keys, keyRow{
			APIKeyInfo: k,
			Expired:    k.ExpiresAt != nil && !k.ExpiresAt.After(now),
			Superseded: k.RevokedAt == nil && k.GraceExpiresAt != nil && k.GraceExpiresAt.After(now),
		})
	}

	// An error here is not a reason to replace the page: the answer is a
	// capability, and failing closed means the choice is not offered.
	if may, err := h.Keys.MayCreateOrgWide(r.Context(), actor); err == nil {
		data.CanCreateOrgWide = may
	}

	// The scope choices are the actor's own permissions minus what a key may
	// never hold. Computed from the identity rather than hardcoded, so a role
	// change shows up here without a template edit.
	for _, p := range actor.Permissions() {
		if _, blocked := auth.NonDelegableScopes[p]; blocked {
			continue
		}
		data.ScopeOptions = append(data.ScopeOptions, p)
	}
	sort.Strings(data.ScopeOptions)

	if r.URL.Query().Get("revoked") == "1" {
		data.Notice = "Key revoked. Anything using it fails on its next request."
	}
	return data, true
}

func (h *Web) KeysPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadKeysPage(w, r)
	if !ok {
		return
	}
	h.render(w, r, http.StatusOK, "keys", data)
}

// KeyCreate mints a key and renders the page directly — no redirect.
//
// A redirect would drop the token, which exists only in this response; the
// alternative is stashing it in a flash cookie, which would put a live
// credential in a Set-Cookie header for nothing. The cost is that refreshing
// this response re-submits and mints a second key, which is visible in the
// list and revocable, and the browser warns before doing it.
func (h *Web) KeyCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	// Three reaches, two fields. "workspace" pins to the workspace being acted
	// in; "organization" and "account" are both unpinned there and differ one
	// tier up, which is exactly the shape the two columns have.
	reach := r.PostFormValue("scope_reach")
	in := auth.CreateAPIKeyInput{
		Name:    r.PostFormValue("name"),
		Scopes:  r.PostForm["scopes"],
		OrgWide: reach == "organization" || reach == "account",
	}
	if reach == "organization" {
		org := IdentityFrom(r.Context()).OrgID
		in.OrganizationID = &org
	}
	if raw := r.PostFormValue("expires_in"); raw != "" {
		var days int
		switch raw {
		case "30":
			days = 30
		case "90":
			days = 90
		case "365":
			days = 365
		}
		if days > 0 {
			at := time.Now().AddDate(0, 0, days)
			in.ExpiresAt = &at
		}
	}

	created, err := h.Keys.Create(r.Context(), IdentityFrom(r.Context()), in)
	if err != nil {
		fields, general := fieldErrors(err)
		if len(fields) == 0 && general == "" {
			h.webError(w, r, err)
			return
		}
		data, ok := h.loadKeysPage(w, r)
		if !ok {
			return
		}
		data.Form.Name = r.PostFormValue("name")
		data.Form.Reach = reach
		data.FieldErrors = fields
		data.Error = general
		h.render(w, r, http.StatusUnprocessableEntity, "keys", data)
		return
	}

	data, ok := h.loadKeysPage(w, r)
	if !ok {
		return
	}
	data.Created = created
	h.render(w, r, http.StatusOK, "keys", data)
}

func (h *Web) KeyRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := h.Keys.Revoke(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/keys?revoked=1")
}

// --- account -----------------------------------------------------------------

type accountPageData struct {
	shell
	FieldErrors map[string]string
	Notice      string
	Error       string

	// ShowDomain is false on a single-host deployment, where "/" is this very
	// dashboard and there is nothing to point elsewhere.
	ShowDomain      bool
	CanEditDomain   bool
	LinkHost        string
	RootRedirectURL string

	// The bot-blocking panel is a separate section from the root redirect, and
	// separately gated. ShowBots is true wherever the settings could be read at
	// all, single-host included: short links are served either way, and so is
	// the crawler traffic this refuses.
	ShowBots          bool
	BlockBots         bool
	BlockBotsEnforced bool
	BotHost           string

	// WorkspacePinned says whether any workspace carries the pin, which decides
	// whether the control shows *Last-Used* as the current choice. The template
	// cannot fold over .Workspaces to work it out, and a second flag is cheaper
	// than a template function that exists for one page.
	WorkspacePinned bool

	// ShowMFA draws the second-factor summary (M53). False on an instance wired
	// without the service, where the routes are not registered either. True with
	// MFA.Available false is a different state and the section says so: the
	// instance has no MFA_SECRET_KEY, which matters most to somebody already
	// enrolled.
	ShowMFA bool
	MFA     auth.MFAStatus

	// ShowDelete draws the account-deletion section (M52). False on an instance
	// wired without the service, where the route is not registered either.
	ShowDelete bool
	// DeleteConfirmation is the word the form requires, rendered into both the
	// prose and the validation message so the page and the handler cannot come
	// to disagree about what it is.
	DeleteConfirmation string
}

// domainSections fills in the two panels that describe the link domain.
//
// One read for both, because they come from one row. Two calls would be two
// identical queries on every account page render, and the second would be the
// kind of cost nobody notices until somebody counts.
//
// Their visibility is not shared, though, and that is the reason they are two
// panels rather than one. The root redirect is meaningless without split hosts —
// on a single-host deployment "/" is this very dashboard. Bot blocking is not:
// short links are served either way, and so is the crawler traffic it refuses.
// Folding them together would have hidden the whole feature from every
// single-host instance, which is the shape a default install has.
func (h *Web) domainSections(r *http.Request, data *accountPageData) {
	if h.Links == nil {
		return
	}
	actor := IdentityFrom(r.Context())
	settings, err := h.Links.DomainSettings(r.Context(), actor)
	if err != nil {
		// A reader who cannot see them simply does not get the panels. These are
		// two operator-chosen settings, not a failure worth replacing the page
		// over.
		return
	}
	data.CanEditDomain = actor.Can(link.PermDomainsWrite)

	if h.Config.SplitHosts() {
		data.ShowDomain = true
		data.LinkHost = h.Config.LinkOrigin()
		data.RootRedirectURL = settings.RootRedirectURL
	}

	data.ShowBots = true
	data.BotHost = settings.Hostname
	data.BlockBots = settings.BlockBots
	data.BlockBotsEnforced = settings.BlockBotsEnforced
}

// accountDeletionConfirmation is what has to be typed beside the password.
//
// A second, deliberate act. The password alone is a field a browser fills in
// from its store and a person confirms without reading; this one cannot be
// autofilled and cannot be produced by muscle memory, which is the only defence
// a page has against an irreversible button being pressed by momentum. The word
// is in the visible prose above the field, so it costs a reader nothing and
// costs somebody skimming exactly the pause it is for.
const accountDeletionConfirmation = "DELETE"

// accountPage assembles the page, so the handlers that re-render it after a
// failed form do not each have to remember which sections it has.
func (h *Web) accountPage(r *http.Request) accountPageData {
	data := accountPageData{
		shell:       h.shell(r, "Account", "account"),
		FieldErrors: map[string]string{},
		// The section is drawn only where the service exists, for the reason
		// every other optional section on this page is: an instance wired
		// without it must not show a button whose route is not registered.
		ShowMFA:            h.MFA != nil,
		ShowDelete:         h.Accounts != nil,
		DeleteConfirmation: accountDeletionConfirmation,
	}
	if h.MFA != nil {
		// A failed read leaves the zero status, which draws the "off" summary and
		// a link to the page that will report the real error. The same trade
		// domainSections makes: a panel is not worth replacing the page over.
		if st, err := h.MFA.Status(r.Context(), IdentityFrom(r.Context())); err == nil {
			data.MFA = st
		}
	}
	for _, ws := range data.Workspaces {
		if ws.Default {
			data.WorkspacePinned = true
			break
		}
	}
	return data
}

func (h *Web) AccountPage(w http.ResponseWriter, r *http.Request) {
	data := h.accountPage(r)
	if r.URL.Query().Get("changed") == "1" {
		data.Notice = "Password changed. Every other session has been signed out."
	}
	if r.URL.Query().Get("domain") == "1" {
		data.Notice = "Link domain updated."
	}
	if r.URL.Query().Get("workspace") == "1" {
		data.Notice = "Default workspace updated. It applies the next time you sign in."
	}
	if r.URL.Query().Get("bots") == "1" {
		data.Notice = "Bot blocking updated. Cached links were refreshed, so it applies now."
	}
	// Where a completed disable lands (M53). Said here rather than on the page
	// that performed it, because with the factor gone that page is an offer to
	// enrol again, and a success notice above an offer reads as an undo button.
	if r.URL.Query().Get("mfa") == "off" {
		data.Notice = "Two-factor authentication is off. The authenticator entry and " +
			"every recovery code have been removed."
	}
	h.domainSections(r, &data)
	h.render(w, r, http.StatusOK, "account", data)
}

func (h *Web) PasswordChange(w http.ResponseWriter, r *http.Request) {
	actor := IdentityFrom(r.Context())

	// Same rule as the JSON endpoint: a leaked API key must not be able to
	// rotate the password out from under its owner.
	if actor.IsAPIKey() {
		h.errorPage(w, r, http.StatusForbidden, "Not allowed",
			"Changing a password requires a signed-in session, not an API key.")
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	fail := func(field, msg string) {
		data := h.accountPage(r)
		if field == "" {
			data.Error = msg
		} else {
			data.FieldErrors[field] = msg
		}
		h.render(w, r, http.StatusUnprocessableEntity, "account", data)
	}

	if len(next) < auth.MinPasswordLength {
		fail("new_password", "The new password must be at least 12 characters.")
		return
	}
	if next != confirm {
		fail("confirm_password", "The passwords do not match.")
		return
	}

	if err := h.Auth.ChangePassword(r.Context(), actor.UserID, actor.SessionID, current, next); err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			fail("", "The current password is incorrect.")
			return
		}
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/account?changed=1")
}

// DomainUpdate handles the link-domain form.
func (h *Web) DomainUpdate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	_, err := h.Links.SetRootRedirect(r.Context(), IdentityFrom(r.Context()),
		r.PostFormValue("root_redirect_url"))
	if err != nil {
		data := h.accountPage(r)
		h.domainSections(r, &data)
		// Show what they typed, not what is stored, so a rejected value can be
		// corrected rather than retyped.
		data.RootRedirectURL = r.PostFormValue("root_redirect_url")

		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			for _, fe := range ve {
				data.FieldErrors[fe.Field] = fe.Message
			}
			h.render(w, r, http.StatusUnprocessableEntity, "account", data)
			return
		}
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/account?domain=1")
}

// BotBlockingUpdate handles the domain bot-blocking form.
//
// Both checkboxes are read from one submission, because the two settings are
// written together — enforcing without blocking is refused, and a form that
// sent them separately would produce that state in between.
func (h *Web) BotBlockingUpdate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	block := r.PostFormValue("block_bots") != ""
	enforced := r.PostFormValue("block_bots_enforced") != ""

	if _, err := h.Links.SetBotBlocking(r.Context(), IdentityFrom(r.Context()), block, enforced); err != nil {
		data := h.accountPage(r)
		h.domainSections(r, &data)
		// Show what they ticked, not what is stored: a refused combination is
		// corrected by changing one box, and re-rendering the stored pair would
		// hide which one they had just moved.
		data.BlockBots = block
		data.BlockBotsEnforced = enforced

		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			for _, fe := range ve {
				data.FieldErrors[fe.Field] = fe.Message
			}
			h.render(w, r, http.StatusUnprocessableEntity, "account", data)
			return
		}
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/account?bots=1")
}

// AccountDelete handles the dashboard's account-deletion form (M52).
//
// The same service call the JSON endpoint makes, with the browser's answer to
// each refusal instead of a problem document. Nothing is decided here: the API
// key check below is the one duplicate, and it is duplicated for the reason
// PasswordChange duplicates it — the person gets a sentence rather than a
// permission slug they never asked to hold.
//
// **Where it lands is the interesting part.** On success there is no signed-in
// surface left to render — the session row is gone with the account — so the
// cookie is expired here and the browser goes to /login, which says what
// happened and names the erasure lag. Redirecting to /account would have meant
// the auth middleware answering the redirect with another one.
func (h *Web) AccountDelete(w http.ResponseWriter, r *http.Request) {
	actor := IdentityFrom(r.Context())

	if actor.IsAPIKey() {
		h.errorPage(w, r, http.StatusForbidden, "Not allowed",
			"Deleting an account requires a signed-in session, not an API key.")
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	// Re-rendered rather than redirected, so a refusal keeps the reader in front
	// of the form that produced it. The status carries the distinction a redirect
	// would flatten: a mistyped confirmation is 422, and a refusal about the
	// state of the account is 409.
	fail := func(status int, field, msg string) {
		data := h.accountPage(r)
		h.domainSections(r, &data)
		if field == "" {
			data.Error = msg
		} else {
			data.FieldErrors[field] = msg
		}
		h.render(w, r, status, "account", data)
	}

	if r.PostFormValue("confirm") != accountDeletionConfirmation {
		fail(http.StatusUnprocessableEntity, "confirm",
			"Type "+accountDeletionConfirmation+" to confirm.")
		return
	}

	if err := h.Accounts.Delete(r.Context(), actor, r.PostFormValue("password")); err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			fail(http.StatusUnprocessableEntity, "delete_password",
				"That password is not correct.")
		case errors.Is(err, domain.ErrConflict):
			// Both conflicts — the instance principal, and an organization left
			// with no owner — carry a sentence naming what to do about it, and
			// SoleOwnerError names which organizations. Beside the form rather
			// than on an error page, because the remedy is on other pages this
			// reader can reach and an error page would send them back a step
			// first.
			fail(http.StatusConflict, "", conflictMessage(err))
		default:
			h.webError(w, r, err)
		}
		return
	}

	http.SetCookie(w, ClearSessionCookie(h.Config.SecureCookies))
	seeOther(w, r, "/login?deleted=1")
}
