package httpx

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/team"
)

// The team surface is two pages, and the split is by object rather than by
// permission.
//
// /members is about people: who is in this organization, at what rank, and
// which workspaces that rank reaches. /workspaces is about places: the
// workspaces of this organization, and the form that creates an organization of
// your own.
//
// Neither takes a top-level nav slot. M26.5 cut that nav to three destinations
// and TestTopLevelNavHoldsThreeDestinations asserts the count exactly, so that
// the next milestone wanting a slot argues for one instead of drifting into it —
// and the argument this milestone makes is that it does not need one. Both pages
// are administrative surfaces visited when something changes, not places work
// happens, which is the same reason Invitations sits in the identity menu.

// --- members -----------------------------------------------------------------

type membersPageData struct {
	shell
	Members []team.Member
	// RoleOptions is the roles this actor may assign: their own rank and below.
	// Read from the seeded rows, so a control cannot offer something the service
	// will then refuse.
	RoleOptions []team.Role
	// Workspaces and GrantTargets are the grant form's two selects: where access
	// can be given, and to whom. Targets are one entry per *person* rather than
	// per membership, because the form names a user — somebody who already holds
	// a workspace-scoped role would otherwise appear twice and mean the same
	// thing both times.
	Workspaces   []team.Workspace
	GrantTargets []team.Member
	// CanWrite draws the controls that change something. The service refuses
	// again on the way in; this is the affordance, not the control.
	CanWrite    bool
	FieldErrors map[string]string
	Notice      string
	Error       string
}

func (h *Web) loadMembersPage(w http.ResponseWriter, r *http.Request) (membersPageData, bool) {
	actor := IdentityFrom(r.Context())
	data := membersPageData{
		shell:       h.shell(r, "Members", "members"),
		FieldErrors: map[string]string{},
		CanWrite:    actor.Can(team.PermMembersWrite),
	}

	members, err := h.Team.Members(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	data.Members = members
	// The list is ordered by rank, so the first row for a person is their
	// broadest membership — which is the role worth showing beside their name.
	seen := make(map[uuid.UUID]struct{}, len(members))
	for _, m := range members {
		if _, dup := seen[m.UserID]; dup {
			continue
		}
		seen[m.UserID] = struct{}{}
		data.GrantTargets = append(data.GrantTargets, m)
	}

	if !data.CanWrite {
		return data, true
	}
	roles, err := h.Team.Roles(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	data.RoleOptions = roles

	workspaces, err := h.Team.Workspaces(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	data.Workspaces = workspaces
	return data, true
}

// MembersPage lists the organization's memberships.
func (h *Web) MembersPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadMembersPage(w, r)
	if !ok {
		return
	}
	switch r.URL.Query().Get("done") {
	case "role":
		data.Notice = "Role changed. It applies to their next request."
	case "removed":
		data.Notice = "Removed. Their links and their account are untouched; " +
			"invite them again to restore access."
	case "granted":
		data.Notice = "Access granted. It adds to whatever they already had."
	}
	h.render(w, r, http.StatusOK, "members", data)
}

// MemberGrant gives an existing member a role in one workspace.
func (h *Web) MemberGrant(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	userID, uerr := uuid.Parse(r.PostFormValue("user_id"))
	workspaceID, werr := uuid.Parse(r.PostFormValue("workspace_id"))
	if uerr != nil || werr != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request",
			"That is not a member, or not a workspace.")
		return
	}

	_, err := h.Team.Grant(r.Context(), IdentityFrom(r.Context()), team.GrantInput{
		UserID: userID, WorkspaceID: workspaceID, Role: r.PostFormValue("role"),
	})
	if err != nil {
		h.renderMembersError(w, r, err)
		return
	}
	seeOther(w, r, "/members?done=granted")
}

// MemberRole re-roles one membership.
func (h *Web) MemberRole(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	if err := h.Team.ChangeRole(r.Context(), IdentityFrom(r.Context()), id,
		r.PostFormValue("role")); err != nil {
		h.renderMembersError(w, r, err)
		return
	}
	seeOther(w, r, "/members?done=role")
}

// MemberRemove ends one membership.
func (h *Web) MemberRemove(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := h.Team.Remove(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.renderMembersError(w, r, err)
		return
	}
	seeOther(w, r, "/members?done=removed")
}

// renderMembersError puts a refusal back on the page that caused it.
//
// The two refusals this milestone is really about — the rank bound and the last
// owner — are rules somebody has just run into, and a full-page error would take
// them away from the list that explains it. Anything else falls through to
// webError, which is where a genuine fault belongs.
func (h *Web) renderMembersError(w http.ResponseWriter, r *http.Request, err error) {
	fields, general := fieldErrors(err)
	if msg := conflictMessage(err); msg != "" {
		general = msg
	}
	if len(fields) == 0 && general == "" {
		h.webError(w, r, err)
		return
	}
	data, ok := h.loadMembersPage(w, r)
	if !ok {
		return
	}
	data.FieldErrors = fields
	data.Error = general
	h.render(w, r, http.StatusUnprocessableEntity, "members", data)
}

// --- workspaces and organizations --------------------------------------------

type workspacesPageData struct {
	shell
	Workspaces []team.Workspace
	// CanWrite draws the create form and the per-row controls.
	CanWrite bool
	// CanCreateOrganization is `orgs.create` (D16). On a default instance that is
	// the account from the setup form and nobody else, so most readers never see
	// this section at all.
	CanCreateOrganization bool
	Form                  struct{ Name, OrganizationName string }
	FieldErrors           map[string]string
	OrgFieldErrors        map[string]string
	Notice                string
	Error                 string
}

func (h *Web) loadWorkspacesPage(w http.ResponseWriter, r *http.Request) (workspacesPageData, bool) {
	actor := IdentityFrom(r.Context())
	data := workspacesPageData{
		shell:                 h.shell(r, "Workspaces", "workspaces"),
		FieldErrors:           map[string]string{},
		OrgFieldErrors:        map[string]string{},
		CanWrite:              actor.Can(team.PermWorkspaceWrite),
		CanCreateOrganization: actor.Can(team.PermOrgsCreate),
	}
	items, err := h.Team.Workspaces(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	data.Workspaces = items
	return data, true
}

// WorkspacesPage lists the workspaces of the caller's organization.
func (h *Web) WorkspacesPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadWorkspacesPage(w, r)
	if !ok {
		return
	}
	switch r.URL.Query().Get("done") {
	case "created":
		data.Notice = "Workspace created."
	case "renamed":
		data.Notice = "Workspace renamed."
	case "deleted":
		data.Notice = "Workspace deleted."
	case "organization":
		data.Notice = "Organization created, and you are now working in it. " +
			"Invite people to it from Invitations."
	}
	h.render(w, r, http.StatusOK, "workspaces", data)
}

// WorkspaceCreate adds a workspace to the caller's organization.
func (h *Web) WorkspaceCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	name := r.PostFormValue("name")
	if _, err := h.Team.CreateWorkspace(r.Context(), IdentityFrom(r.Context()), name); err != nil {
		h.renderWorkspacesError(w, r, err, func(d *workspacesPageData) { d.Form.Name = name })
		return
	}
	seeOther(w, r, "/workspaces?done=created")
}

// WorkspaceRename changes a workspace's name.
func (h *Web) WorkspaceRename(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	if _, err := h.Team.RenameWorkspace(r.Context(), IdentityFrom(r.Context()), id,
		r.PostFormValue("name")); err != nil {
		h.renderWorkspacesError(w, r, err, nil)
		return
	}
	seeOther(w, r, "/workspaces?done=renamed")
}

// WorkspaceDelete removes a workspace, and refuses while it holds any link.
func (h *Web) WorkspaceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := h.Team.DeleteWorkspace(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.renderWorkspacesError(w, r, err, nil)
		return
	}
	seeOther(w, r, "/workspaces?done=deleted")
}

// OrganizationCreate provisions an organization and moves the browser into it.
//
// The switch is the difference from the JSON endpoint, and it is the same trade
// the setup form makes: somebody who just created an organization meant to start
// using it, and the page they are on lists the *previous* organization's
// workspaces — so leaving them there would show them a list their new
// organization is not in. A failed switch is not undone; the organization
// exists either way, and the switcher in the chrome reaches it.
func (h *Web) OrganizationCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	name := r.PostFormValue("name")

	org, err := h.Team.CreateOrganization(r.Context(), IdentityFrom(r.Context()), name)
	if err != nil {
		h.renderWorkspacesError(w, r, err, func(d *workspacesPageData) {
			d.Form.OrganizationName = name
			d.OrgFieldErrors, d.FieldErrors = d.FieldErrors, map[string]string{}
		})
		return
	}

	if serr := h.Auth.SwitchWorkspace(r.Context(), IdentityFrom(r.Context()),
		org.WorkspaceID); serr != nil {
		seeOther(w, r, "/workspaces")
		return
	}
	seeOther(w, r, "/workspaces?done=organization")
}

// renderWorkspacesError puts a refusal back on the page that caused it, the way
// renderMembersError does. D32's "delete the links first" is the refusal this
// exists for: it is an instruction, and an instruction belongs beside the list
// it is about.
func (h *Web) renderWorkspacesError(
	w http.ResponseWriter, r *http.Request, err error, adjust func(*workspacesPageData),
) {
	fields, general := fieldErrors(err)
	if msg := conflictMessage(err); msg != "" {
		general = msg
	}
	if len(fields) == 0 && general == "" {
		h.webError(w, r, err)
		return
	}
	data, ok := h.loadWorkspacesPage(w, r)
	if !ok {
		return
	}
	data.FieldErrors = fields
	data.Error = general
	if adjust != nil {
		adjust(&data)
	}
	h.render(w, r, http.StatusUnprocessableEntity, "workspaces", data)
}
