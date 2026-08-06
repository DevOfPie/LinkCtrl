package httpx

import (
	"net/http"

	"github.com/google/uuid"
)

// WorkspaceSwitch moves the browser into another workspace and returns it
// where it was.
//
// A plain form post, same as the appearance control. The switcher sits in the
// page chrome, so the destination comes back on the form rather than from
// Referer: switching on the links page should leave you on the links page,
// looking at the other workspace's links.
func (h *Web) WorkspaceSwitch(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	id, err := uuid.Parse(r.PostFormValue("workspace_id"))
	if err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "That is not a workspace.")
		return
	}
	if err := h.Auth.SwitchWorkspace(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, safeNext(r.PostFormValue("next")))
}

// WorkspaceDefault sets, or clears, the pinned default workspace.
//
// An empty workspace_id is the *Last-Used* option rather than a missing field:
// the control's first choice is the derived behaviour, and choosing it has to
// be a way back from having pinned something.
func (h *Web) WorkspaceDefault(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	var pin *uuid.UUID
	if raw := r.PostFormValue("workspace_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			h.errorPage(w, r, http.StatusBadRequest, "Bad request", "That is not a workspace.")
			return
		}
		pin = &id
	}

	if err := h.Auth.SetDefaultWorkspace(r.Context(), IdentityFrom(r.Context()), pin); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/account?workspace=1")
}
