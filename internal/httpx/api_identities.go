package httpx

import (
	"fmt"
	"net/http"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// IdentityAPI serves the connected sign-in providers on an account (M70, F315).
//
// **Why this exists at all.** M65 built the linking table, the flow that fills it
// and the refusals that read it, and shipped no way to see or sever a row — so
// somebody who connected a provider was connected for the life of the account,
// and an operator responding to a compromised provider had `DELETE FROM
// addon_identity_links` and nothing else. A link signs somebody in with **no
// password and no second factor of this product's**, which is why account
// deletion already removes these rows; what was missing was undoing one on
// purpose.
//
// Two principals, two questions, and they are separate operations rather than one
// with a scope parameter. A person asks *what can sign me in*, and the answer is
// theirs by definition. An operator asks *whose accounts does this add-on hold*,
// which costs the instance's non-delegable `addons.manage` and needs the email to
// mean anything.
type IdentityAPI struct {
	Auth *auth.Service
}

// List is every provider the calling account has connected.
//
// No pagination, for [WorkspaceAPI.List]'s reason: a person's connected providers
// are a handful of rows by construction.
//
// **No subject in the response.** It is the provider's identifier for a person,
// it answers none of the questions this endpoint exists for — which add-on, which
// issuer, when it was last used — and an opaque external id on a surface of ours
// invites a caller to treat it as one of ours.
func (a *IdentityAPI) List(w http.ResponseWriter, r *http.Request) {
	items, err := a.Auth.ConnectedIdentities(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": apiIdentities(items, false)})
}

// Delete severs one of the calling account's own connected providers.
//
// **Not a sign-out.** Sessions the link already minted stay valid until they
// lapse or the account revokes them, which is a separate act with its own record.
// What this removes is the standing way back in.
func (a *IdentityAPI) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Auth.DisconnectIdentity(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteForAddon severs one account's link to a named add-on, for the operator.
//
// The permission is checked here rather than in `internal/auth`, which has no
// view of the instance grant vocabulary — the same split every other
// instance-scoped operation in this package uses.
func (a *IdentityAPI) DeleteForAddon(w http.ResponseWriter, r *http.Request) {
	actor := IdentityFrom(r.Context())
	if !actor.Can(auth.PermAddonsManage) {
		WriteError(w, r, fmt.Errorf("%w: disconnecting an identity requires %s",
			domain.ErrForbidden, auth.PermAddonsManage))
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Auth.DisconnectIdentityFor(r.Context(), actor, r.PathValue("name"), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiIdentity is one connected provider as the API renders it.
type apiIdentity struct {
	ID     string `json:"id"`
	Addon  string `json:"addon"`
	Issuer string `json:"issuer"`
	// time.Time rather than a formatted string, so these serialize as RFC 3339
	// like every other timestamp this API answers with. A second format on one
	// endpoint is the kind of divergence a client discovers in production.
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	// Email is set only on the operator's listing, where the question is whose
	// account this is. Omitted rather than empty on the account's own, because a
	// key that is always the caller's own address is a field that means nothing.
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

func apiIdentities(in []auth.ConnectedIdentity, withAccount bool) []apiIdentity {
	out := make([]apiIdentity, 0, len(in))
	for _, l := range in {
		row := apiIdentity{
			ID: l.ID.String(), Addon: l.Addon, Issuer: l.Issuer,
			CreatedAt: l.CreatedAt.UTC(), LastUsedAt: l.LastUsedAt,
		}
		if withAccount {
			row.Email, row.Name = l.Email, l.Name
		}
		out = append(out, row)
	}
	return out
}
