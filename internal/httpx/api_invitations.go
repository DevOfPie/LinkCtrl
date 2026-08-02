package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/invite"
)

// InvitationAPI is the invitation lifecycle, as a program sees it.
//
// The dashboard's forms post at the handlers in web_invitations.go and both
// reach the same four service calls, so a client can issue, list, revoke and
// redeem exactly as a person can.
//
// Three of the four need `members.write`, which is delegable (D28) under a cap
// on what a key may issue at (D43, invite.KeyIssuableRoles). The cap and not the
// rank ceiling is what keeps D18's second limb from applying, because redemption
// needs no credential at all — it is how somebody who has none acquires one, and
// that is precisely why an unbounded key-issued invitation was a way for a key
// to widen its reach into scopes no key may hold.
type InvitationAPI struct {
	Invites *invite.Service
}

type createInvitationRequest struct {
	Email string `json:"email"`
	// Role is optional; omitting it invites at the least powerful built-in
	// role, so a caller that did not think about it admits somebody who can do
	// the least.
	Role string `json:"role"`
}

// Create issues an invitation.
//
// 201 with the link in it, because that link is the only copy of the token that
// will ever exist — the same reason creating an API key answers with the key.
// `emailed` says whether a message was also queued; false is an instance with
// no relay configured, not a failure.
func (a *InvitationAPI) Create(w http.ResponseWriter, r *http.Request) {
	var req createInvitationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	created, err := a.Invites.Create(r.Context(), IdentityFrom(r.Context()), invite.CreateInput{
		Email: req.Email, Role: req.Role,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, created)
}

// List returns the organization's invitations, newest first.
//
// Not paginated. An organization's invitations are a handful of rows by
// construction, and a cursor here would be machinery for a page that cannot
// fill.
func (a *InvitationAPI) List(w http.ResponseWriter, r *http.Request) {
	items, err := a.Invites.List(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Revoke ends an invitation that has not been redeemed.
func (a *InvitationAPI) Revoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Invites.Revoke(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type redeemInvitationRequest struct {
	Token    string `json:"token"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Redeem turns an invitation into a membership.
//
// Unauthenticated on purpose: this is the endpoint by which somebody who has no
// credential acquires one. It carries the login rate limit rather than the API
// one, because it verifies a password.
//
// No session is started. The caller knows the password they just supplied and
// POST /auth/login is one call away, and minting a cookie from an endpoint a
// non-browser client is the main user of would be a credential nobody asked
// for. The dashboard's own form does sign the person in, in its handler.
func (a *InvitationAPI) Redeem(w http.ResponseWriter, r *http.Request) {
	var req redeemInvitationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Invites.Redeem(r.Context(), invite.RedeemInput{
		Token: req.Token, Email: req.Email, Name: req.Name, Password: req.Password,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"user_id":           out.UserID,
		"organization_id":   out.OrganizationID,
		"organization_name": out.OrganizationName,
		"role":              out.Role,
		"account_created":   out.Created,
	})
}
