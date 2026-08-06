package httpx

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// WorkspaceAPI is the switcher, as a program sees it.
//
// The dashboard's switcher posts forms at the handlers in web_workspaces.go and
// both reach the same three service calls, so a client can do everything the
// nav dropdown and the account setting can.
type WorkspaceAPI struct {
	Auth *auth.Service
}

// List returns every workspace the caller may act in, flagging which one this
// request is in and which one new sessions start in.
//
// No pagination. A person's memberships are a handful of rows by construction —
// a cursor here would be machinery for a page that cannot fill.
func (a *WorkspaceAPI) List(w http.ResponseWriter, r *http.Request) {
	items, err := a.Auth.Workspaces(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Switch moves the calling session into a workspace.
//
// 204 rather than the new identity: the caller already knows what it asked
// for, and the next request resolves the change anyway. Refused for an API key,
// for the two reasons SwitchWorkspace gives — neither of which is that a key
// would be unaffected by the result.
func (a *WorkspaceAPI) Switch(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Auth.SwitchWorkspace(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setDefaultRequest carries a nullable id: `null` is the way back to last-used,
// and a pointer is how "absent" and "explicitly null" stay distinguishable
// through DisallowUnknownFields.
type setDefaultRequest struct {
	WorkspaceID *uuid.UUID `json:"workspace_id"`
}

// SetDefault pins where new sessions start, or clears the pin with a null.
func (a *WorkspaceAPI) SetDefault(w http.ResponseWriter, r *http.Request) {
	var req setDefaultRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.WorkspaceID != nil && *req.WorkspaceID == uuid.Nil {
		WriteError(w, r, domain.ValidationErrors{{
			Field: "workspace_id", Code: "invalid",
			Message: "workspace_id must be a workspace id, or null to follow last-used",
		}})
		return
	}
	if err := a.Auth.SetDefaultWorkspace(r.Context(), IdentityFrom(r.Context()), req.WorkspaceID); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
