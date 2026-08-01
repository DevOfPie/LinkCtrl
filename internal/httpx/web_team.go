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
	// OrgWorkspaces and GrantTargets are the grant form's two selects: where
	// access can be given, and to whom. Targets are one entry per *person*
	// rather than per membership, because the form names a user — somebody who
	// already holds a workspace-scoped role would otherwise appear twice and
	// mean the same thing both times.
	//
	// Named for the organization rather than `Workspaces` because the shell
	// already carries a field of that name, of a different type, for the
	// switcher — and a page field shadows it for the whole template, layout
	// included. TestNoPageDataStructShadowsTheShell is what now stops the next
	// one.
	OrgWorkspaces []team.Workspace
	GrantTargets  []team.Member
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
	data.OrgWorkspaces = workspaces
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
	// OrgWorkspaces is the list this page is about: the workspaces of the
	// organization being acted in, with the per-row rights to rename and delete
	// them.
	//
	// Not `Workspaces`, which is the shell's — every workspace this person may
	// act in, across every organization, in the switcher's own type. A page
	// field of the same name shadows the shell's for the entire template, the
	// layout's chrome included, which is how this page and /members came to
	// answer 500 while looking correct in every test (F20), and
	// TestNoPageDataStructShadowsTheShell is what now stops the next one.
	OrgWorkspaces []team.Workspace
	// CanWrite draws the create form and the per-row controls.
	CanWrite bool
	// CanCreateOrganization is `orgs.create` (D16). On a default instance that is
	// the account from the setup form and nobody else, so most readers never see
	// this section at all.
	CanCreateOrganization bool
	// CanDeleteOrganization is `org.delete` (M28.5), held by the owner role
	// alone. Drawn from the permission rather than from the role, so it stays
	// true of whoever actually holds it.
	CanDeleteOrganization bool
	// OrganizationID and OrganizationName describe the organization this page is
	// about, for the deletion control: an irreversible action has to name what it
	// will destroy, and the id is what the form posts back as its confirmation.
	// The name comes from the switcher's list, which the shell has already
	// loaded, rather than from a query of its own.
	OrganizationID   uuid.UUID
	OrganizationName string
	Form             struct{ Name, OrganizationName string }
	FieldErrors      map[string]string
	OrgFieldErrors   map[string]string
	Notice           string
	Error            string
}

func (h *Web) loadWorkspacesPage(w http.ResponseWriter, r *http.Request) (workspacesPageData, bool) {
	actor := IdentityFrom(r.Context())
	data := workspacesPageData{
		shell:                 h.shell(r, "Workspaces", "workspaces"),
		FieldErrors:           map[string]string{},
		OrgFieldErrors:        map[string]string{},
		CanWrite:              actor.Can(team.PermWorkspaceWrite),
		CanCreateOrganization: actor.Can(team.PermOrgsCreate),
		CanDeleteOrganization: actor.Can(team.PermOrgDelete),
		OrganizationID:        actor.OrgID,
	}
	// The shell's list — every workspace this person may act in, across every
	// organization — which is why it is the one that knows organization names.
	for _, ws := range data.Workspaces {
		if ws.OrganizationID == actor.OrgID {
			data.OrganizationName = ws.OrganizationName
			break
		}
	}
	items, err := h.Team.Workspaces(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	data.OrgWorkspaces = items
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
//
// Two callers post here and a refusal has to go back to whichever one it came
// from: the workspaces page's organization form, and the page an account that
// belongs to nothing is held on (D36). Re-rendering the workspaces page for the
// second would fail on the way in — listing workspaces needs workspace.read,
// which an account with no membership does not hold — so the branch below is
// about which page exists for this reader, not about presentation.
func (h *Web) OrganizationCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	name := r.PostFormValue("name")
	actor := IdentityFrom(r.Context())

	org, err := h.Team.CreateOrganization(r.Context(), actor, name)
	if err != nil {
		if !actor.HasOrganization() {
			h.renderOrganizationNewError(w, r, err, name)
			return
		}
		h.renderWorkspacesError(w, r, err, func(d *workspacesPageData) {
			d.Form.OrganizationName = name
			d.OrgFieldErrors, d.FieldErrors = d.FieldErrors, map[string]string{}
		})
		return
	}

	if serr := h.Auth.SwitchWorkspace(r.Context(), actor, org.WorkspaceID); serr != nil {
		seeOther(w, r, "/workspaces")
		return
	}
	seeOther(w, r, "/workspaces?done=organization")
}

// renderOrganizationNewError puts a refusal back on the only page an orphaned
// account can see. Anything without a message a reader could act on falls
// through to webError, which is where a genuine fault belongs.
func (h *Web) renderOrganizationNewError(
	w http.ResponseWriter, r *http.Request, err error, name string,
) {
	fields, general := fieldErrors(err)
	if msg := conflictMessage(err); msg != "" {
		general = msg
	}
	if len(fields) == 0 && general == "" {
		h.webError(w, r, err)
		return
	}
	data := organizationNewPageData{
		shell:       h.shell(r, "Create an organization", ""),
		FieldErrors: fields,
		Error:       general,
	}
	data.Form.Name = name
	h.render(w, r, http.StatusUnprocessableEntity, "organization_new", data)
}

// OrganizationDelete tears down the organization the reader is acting in.
//
// Afterwards they are somewhere else by definition, and which somewhere depends
// on what else they belong to — another organization, or nothing at all. Rather
// than work that out here, this hands the browser to /dashboard and lets the
// ordinary resolution decide: somebody with another membership lands on it, and
// somebody with none is met by RequireOrganization and offered one. The session
// needs no repair on the way, because sessions.workspace_id is SET NULL by the
// cascade and ResolveWorkspaceForUser answers again from scratch.
func (h *Web) OrganizationDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := h.Team.DeleteOrganization(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.renderWorkspacesError(w, r, err, nil)
		return
	}
	seeOther(w, r, "/dashboard")
}

// --- belonging to nothing (D36) ----------------------------------------------

type organizationNewPageData struct {
	shell
	Form        struct{ Name string }
	FieldErrors map[string]string
	Error       string
}

// OrganizationNewPage is the whole product for an account that belongs to
// nothing.
//
// It exists because D36 chose to let deletion orphan people rather than refuse
// on their behalf, and an orphaned account with no page to land on would be an
// error message where an empty state belongs. Everything else is refused while
// they are here — see RequireOrganization — so this page carries no navigation
// of its own beyond signing out.
//
// Somebody who *does* belong to an organization is sent to /workspaces instead,
// which is where the same form lives for them, alongside the workspaces it would
// otherwise be missing. One form, two homes, rather than two forms.
func (h *Web) OrganizationNewPage(w http.ResponseWriter, r *http.Request) {
	if IdentityFrom(r.Context()).HasOrganization() {
		seeOther(w, r, "/workspaces")
		return
	}
	h.render(w, r, http.StatusOK, "organization_new", organizationNewPageData{
		shell:       h.shell(r, "Create an organization", ""),
		FieldErrors: map[string]string{},
	})
}

// RequireOrganization sends an account that belongs to nothing to the page that
// offers it one, and lets everybody else past.
//
// It is an affordance and not the authorization boundary, which is the
// distinction worth keeping straight: an identity with no organization holds an
// empty permission set, so every service call it could reach already refuses on
// the check it always made. What this adds is that the refusals are never seen —
// a page rendered for a workspace that does not exist is an error where the
// milestone asks for an empty state, and eight pages each discovering that
// separately is eight chances to get it wrong once.
//
// Applied to the dashboard tree only. The JSON API needs no equivalent: its
// operations authorize on permissions, an orphaned caller holds none, and the
// handful of endpoints that are user-scoped rather than organization-scoped —
// the notification inbox, the workspace list — correctly answer with an empty
// list, which is the state rendered rather than an error.
func (h *Web) RequireOrganization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IdentityFrom(r.Context()).HasOrganization() {
			seeOther(w, r, "/organizations/new")
			return
		}
		next.ServeHTTP(w, r)
	})
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
