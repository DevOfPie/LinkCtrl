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
}

// domainSection fills in the link-domain panel, or leaves it hidden.
func (h *Web) domainSection(r *http.Request, data *accountPageData) {
	if h.Links == nil || !h.Config.SplitHosts() {
		return
	}
	settings, err := h.Links.DomainSettings(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		// A reader who cannot see it simply does not get the panel. This is one
		// operator-chosen URL, not a failure worth replacing the page over.
		return
	}
	data.ShowDomain = true
	data.CanEditDomain = IdentityFrom(r.Context()).Can(link.PermDomainsWrite)
	data.LinkHost = h.Config.LinkOrigin()
	data.RootRedirectURL = settings.RootRedirectURL
}

func (h *Web) AccountPage(w http.ResponseWriter, r *http.Request) {
	data := accountPageData{
		shell:       h.shell(r, "Account", "account"),
		FieldErrors: map[string]string{},
	}
	if r.URL.Query().Get("changed") == "1" {
		data.Notice = "Password changed. Every other session has been signed out."
	}
	if r.URL.Query().Get("domain") == "1" {
		data.Notice = "Link domain updated."
	}
	h.domainSection(r, &data)
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
		data := accountPageData{
			shell:       h.shell(r, "Account", "account"),
			FieldErrors: map[string]string{},
		}
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
		data := accountPageData{
			shell:       h.shell(r, "Account", "account"),
			FieldErrors: map[string]string{},
		}
		h.domainSection(r, &data)
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
