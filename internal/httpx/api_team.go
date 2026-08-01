package httpx

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/team"
)

// TeamAPI is member management, workspace lifecycle and organization creation,
// as a program sees it.
//
// The dashboard's forms post at the handlers in web_team.go and both reach the
// same service calls, so a client can re-role, remove, grant, create, rename and
// delete exactly as a person can.
//
// The permissions are the ones the service enforces, and nothing here re-checks
// them: members.read to look, members.write to change a membership,
// workspace.write to change a workspace, orgs.create to make an organization.
// All four are delegable to an API key — none of them discloses an identity tied
// to network data, and none lets a key widen its own reach, because a key's
// permissions are its scopes intersected with its owner's role on every request
// (D18).
//
// org.delete, added by M28.5, is the exception and was already decided: it has
// sat in auth.NonDelegableScopes since Phase 1, on D18's **irreversible** limb —
// an action with no undo belongs behind an interactive sign-in rather than
// behind a token in a CI variable. M28.5 gives it its first operation and
// changes nothing about that; the map is the only mechanism, and no handler here
// asks which credential it was called with.
type TeamAPI struct {
	Team *team.Service
}

// ListMembers returns every membership in the caller's organization.
//
// Not paginated, for the reason the invitation list is not: an organization's
// membership is a handful of rows by construction, and a cursor here would be
// machinery for a page that cannot fill.
func (a *TeamAPI) ListMembers(w http.ResponseWriter, r *http.Request) {
	items, err := a.Team.Members(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type grantMemberRequest struct {
	UserID      uuid.UUID `json:"user_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Role        string    `json:"role"`
}

// GrantMember gives an existing member a role in one workspace.
//
// It **adds** access and never narrows it (D31): permissions resolve as the
// union of every matching membership, so an organization-wide editor granted
// admin in one workspace is an admin there and an editor everywhere else. There
// is no operation that restricts somebody to a workspace, and this is not it.
func (a *TeamAPI) GrantMember(w http.ResponseWriter, r *http.Request) {
	var req grantMemberRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.UserID == uuid.Nil {
		WriteError(w, r, domain.ValidationErrors{{
			Field: "user_id", Code: "required", Message: "name the member being given access",
		}})
		return
	}
	if req.WorkspaceID == uuid.Nil {
		WriteError(w, r, domain.ValidationErrors{{
			Field: "workspace_id", Code: "required", Message: "name the workspace",
		}})
		return
	}
	member, err := a.Team.Grant(r.Context(), IdentityFrom(r.Context()), team.GrantInput{
		UserID: req.UserID, WorkspaceID: req.WorkspaceID, Role: req.Role,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, member)
}

type changeRoleRequest struct {
	Role string `json:"role"`
}

// ChangeMemberRole re-roles one membership.
//
// 403 when the membership is not below the caller's own rank — including their
// own, which is what "an admin cannot demote themselves" looks like from here —
// and 409 when it would leave the organization without an owner.
func (a *TeamAPI) ChangeMemberRole(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req changeRoleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Team.ChangeRole(r.Context(), IdentityFrom(r.Context()), id, req.Role); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RemoveMember ends one membership.
//
// The membership, not the account: everything else that account holds survives,
// which is what makes this reversible by inviting them back.
func (a *TeamAPI) RemoveMember(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Team.Remove(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type workspaceNameRequest struct {
	Name string `json:"name"`
}

// CreateWorkspace adds a workspace to the caller's organization.
func (a *TeamAPI) CreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req workspaceNameRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	ws, err := a.Team.CreateWorkspace(r.Context(), IdentityFrom(r.Context()), req.Name)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, ws)
}

// RenameWorkspace changes a workspace's name, and its slug with it.
func (a *TeamAPI) RenameWorkspace(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req workspaceNameRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	ws, err := a.Team.RenameWorkspace(r.Context(), IdentityFrom(r.Context()), id, req.Name)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, ws)
}

// DeleteWorkspace removes a workspace, and refuses with 409 while it holds any
// link at all or while it is the organization's last one (D32).
func (a *TeamAPI) DeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Team.DeleteWorkspace(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreateOrganization provisions an organization, its first workspace and an
// owner membership for the caller, in one transaction.
//
// Requires `orgs.create`, which on a default instance is held by the account
// from the setup form and by nobody else (D16).
func (a *TeamAPI) CreateOrganization(w http.ResponseWriter, r *http.Request) {
	var req workspaceNameRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	org, err := a.Team.CreateOrganization(r.Context(), IdentityFrom(r.Context()), req.Name)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, org)
}

// DeleteOrganization tears down the organization the caller is acting in.
//
// The id names the organization being deleted and must be the caller's current
// one; anything else is 404, so an id cannot be probed and a mistyped one
// deletes nothing. 409 while it still holds any link (D37) or while it is the
// instance's only organization.
//
// Requires `org.delete`, held by the `owner` role alone and not delegable to an
// API key — so this endpoint answers a session and nothing else, without
// branching on the credential (D18, auth.NonDelegableScopes).
func (a *TeamAPI) DeleteOrganization(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Team.DeleteOrganization(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
