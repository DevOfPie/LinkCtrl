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

// keyRow is APIKeyInfo plus the one derived flag the template needs.
type keyRow struct {
	auth.APIKeyInfo
	Expired bool
}

type keysPageData struct {
	shell
	Keys         []keyRow
	ScopeOptions []string
	Created      *auth.CreatedAPIKey
	Form         struct{ Name string }
	FieldErrors  map[string]string
	Notice       string
	Error        string
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
		})
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

	in := auth.CreateAPIKeyInput{
		Name:   r.PostFormValue("name"),
		Scopes: r.PostForm["scopes"],
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

// accountPage assembles the page, so the handlers that re-render it after a
// failed form do not each have to remember which sections it has.
func (h *Web) accountPage(r *http.Request) accountPageData {
	data := accountPageData{
		shell:       h.shell(r, "Account", "account"),
		FieldErrors: map[string]string{},
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
