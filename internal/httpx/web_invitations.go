package httpx

import (
	"errors"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
)

// The invitation surface has two halves that share nothing but a service.
//
// /invites is the administrator's page: issue, list, revoke, behind
// members.write. /invite/{token} is public, because the whole point of it is to
// be opened by somebody who has no account yet — and on a default instance,
// where the mailer is off (D1), the copied link is the only way it is ever
// reached.

type invitesPageData struct {
	shell
	Invitations []invite.Invitation
	// RoleOptions is the roles this actor may invite at: their own rank and
	// below (D28). Read from the seeded rows rather than hardcoded, so a
	// demotion narrows the form without a template change.
	RoleOptions []invite.Role
	// Created is the invitation just issued. Rendered directly rather than
	// redirected to, because the link exists only in this response — the same
	// reason the keys page renders a new key instead of redirecting.
	Created     *invite.Created
	Form        struct{ Email, Role string }
	FieldErrors map[string]string
	Notice      string
	Error       string
	// MailConfigured decides which sentence the page tells the truth with:
	// "we emailed it, and here is the link" or "copy this link, nothing was
	// sent".
	MailConfigured bool
}

func (h *Web) loadInvitesPage(w http.ResponseWriter, r *http.Request) (invitesPageData, bool) {
	actor := IdentityFrom(r.Context())
	data := invitesPageData{
		shell:          h.shell(r, "Invitations", "invites"),
		FieldErrors:    map[string]string{},
		MailConfigured: h.Config.SMTP.Enabled(),
	}
	items, err := h.Invites.List(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	data.Invitations = items

	roles, err := h.Invites.Roles(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	data.RoleOptions = roles
	return data, true
}

// InvitesPage lists the organization's invitations and offers the form.
func (h *Web) InvitesPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadInvitesPage(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("revoked") == "1" {
		data.Notice = "Invitation revoked. Its link stops working immediately."
	}
	h.render(w, r, http.StatusOK, "invites", data)
}

// InviteCreate issues an invitation and renders the page with the link on it.
func (h *Web) InviteCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	created, err := h.Invites.Create(r.Context(), IdentityFrom(r.Context()), invite.CreateInput{
		Email: r.PostFormValue("email"),
		Role:  r.PostFormValue("role"),
	})
	if err != nil {
		fields, general := fieldErrors(err)
		if len(fields) == 0 && general == "" {
			h.webError(w, r, err)
			return
		}
		data, ok := h.loadInvitesPage(w, r)
		if !ok {
			return
		}
		data.Form.Email = r.PostFormValue("email")
		data.Form.Role = r.PostFormValue("role")
		data.FieldErrors = fields
		data.Error = general
		h.render(w, r, http.StatusUnprocessableEntity, "invites", data)
		return
	}

	data, ok := h.loadInvitesPage(w, r)
	if !ok {
		return
	}
	data.Created = created
	h.render(w, r, http.StatusOK, "invites", data)
}

// InviteRevoke ends an invitation.
func (h *Web) InviteRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := h.Invites.Revoke(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/invites?revoked=1")
}

// --- the public half ---------------------------------------------------------

type invitePageData struct {
	shell
	// Token travels on the form rather than only in the URL, so the POST target
	// is a fixed path and the token is not re-parsed out of it.
	Token string
	Offer *invite.Offer
	// Valid is false for every unredeemable invitation, and the page says the
	// same thing for all of them.
	Valid       bool
	Form        struct{ Email, Name string }
	FieldErrors map[string]string
	Error       string
	// NewAccounts says whether this instance may create an account here. False
	// under SIGNUP_MODE=closed (D7), where the form still exists — an existing
	// account can still join — but promises less.
	NewAccounts bool
}

func (h *Web) invitePage(r *http.Request, token string) invitePageData {
	return invitePageData{
		shell:       h.shell(r, "Invitation", ""),
		Token:       token,
		FieldErrors: map[string]string{},
		NewAccounts: h.Config.Auth.SignupMode != config.SignupClosed,
	}
}

// InvitePage renders the invitation somebody was sent.
//
// It shows which organization and which role, and deliberately not the address
// the invitation was issued to. Printing that would hand whoever picked the
// link up the one thing they need to redeem it, which is exactly what binding
// the invitation to an address is for (D27).
func (h *Web) InvitePage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	data := h.invitePage(r, token)

	offer, err := h.Invites.Offer(r.Context(), token)
	if err != nil {
		if !errors.Is(err, invite.ErrNotRedeemable) {
			h.webError(w, r, err)
			return
		}
		// 404 for every unredeemable invitation, matching the API. Expired,
		// revoked, spent and invented are one answer.
		h.render(w, r, http.StatusNotFound, "invite", data)
		return
	}
	data.Valid, data.Offer = true, offer
	h.render(w, r, http.StatusOK, "invite", data)
}

// InviteAccept redeems the invitation and signs the person in.
//
// Signing in here is the difference from the JSON endpoint, and it is the same
// trade the first-run setup form makes: the password was in hand exactly once,
// and bouncing somebody to a login form to retype what they just typed helps
// nobody.
func (h *Web) InviteAccept(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	email := r.PostFormValue("email")
	password := r.PostFormValue("password")

	_, err := h.Invites.Redeem(r.Context(), invite.RedeemInput{
		Token: token, Email: email, Name: r.PostFormValue("name"), Password: password,
	})
	if err != nil {
		data := h.invitePage(r, token)
		data.Form.Email = email
		data.Form.Name = r.PostFormValue("name")

		if fields, general := fieldErrors(err); len(fields) > 0 || general != "" {
			// A malformed address or a short password. Both are about what was
			// typed and say nothing about any account, so the form keeps its
			// shape and points at the field.
			if offer, oerr := h.Invites.Offer(r.Context(), token); oerr == nil {
				data.Valid, data.Offer = true, offer
			}
			data.FieldErrors = fields
			data.Error = general
			h.render(w, r, http.StatusUnprocessableEntity, "invite", data)
			return
		}
		if !errors.Is(err, invite.ErrNotRedeemable) {
			h.webError(w, r, err)
			return
		}
		// The generic refusal. The invitation may well still be live — a wrong
		// password lands here — so the form is re-rendered rather than replaced
		// by the dead-invitation page, with the one message every failure gets.
		if offer, oerr := h.Invites.Offer(r.Context(), token); oerr == nil {
			data.Valid, data.Offer = true, offer
		}
		data.Error = "That did not work. Check the address the invitation was sent " +
			"to and the password for that account, then try again."
		h.render(w, r, http.StatusUnprocessableEntity, "invite", data)
		return
	}

	// The membership exists; the session is a convenience on top of it. A
	// failure to sign in therefore goes to the login form rather than undoing
	// anything.
	res, lerr := h.Auth.Login(r.Context(), auth.LoginInput{
		Email: email, Password: password,
		IP: ClientIPFrom(r.Context()), UserAgent: r.UserAgent(),
	})
	if lerr != nil {
		seeOther(w, r, "/login")
		return
	}
	maxAge := int(h.Config.Auth.SessionAbsoluteTTL.Seconds())
	http.SetCookie(w, NewSessionCookie(res.Token, h.Config.SecureCookies, maxAge))
	seeOther(w, r, "/dashboard")
}
