package httpx

import (
	"fmt"
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
// and TestTopLevelNavHoldsTwoDestinations asserts the count exactly — two since
// M46 moved API keys down beside them — so that the next milestone wanting a
// slot argues for one instead of drifting into it —
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
	// OrgWorkspaces on *this* page is the workspaces the reader may grant in,
	// not every workspace they can see. The unfiltered list offered a
	// workspace-scoped admin every workspace in the organization, which is how
	// F27's first move — granting themselves into a workspace they held nothing
	// in — was three dropdown picks rather than a forged request. Filtered where
	// it is loaded rather than in the template, so that an empty list also
	// removes the form instead of leaving an empty select behind.
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
	CanWrite bool
	// Form.Role is which role the grant form arrives on, and it is the *lowest*
	// one the actor may assign (D173, F182). It used to be unset, so the browser
	// selected the first option of a list ordered most powerful first, and an
	// owner who filled in nothing but the address granted owner.
	Form struct{ Role string }
	// GrantNote is what the grant form says the button will actually do for the
	// target, workspace and role selected in it, and GrantNoteWarn is whether
	// that answer is one to stop at. Conditioned rather than generic (F163): the
	// sentence above the form is true of every grant and therefore says nothing
	// about the one about to be made.
	GrantNote     string
	GrantNoteWarn bool
	FieldErrors   map[string]string
	Notice        string
	Error         string
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
	// Manageable is workspace.write in that workspace, answered per row by
	// team.Service.Workspaces through canInWorkspace. Both built-in roles that
	// hold members.write hold workspace.write too (00700_seed.sql), so it is the
	// same set — and where an operator has composed a role that separates them,
	// the service refuses on members.write regardless. This is the affordance,
	// and Grant is the control.
	for _, ws := range workspaces {
		if ws.Manageable {
			data.OrgWorkspaces = append(data.OrgWorkspaces, ws)
		}
	}

	// The grant form's state. Read from the query string because the two selects
	// ask for this note again on change — the same hx-get the links filters use —
	// and from the defaults otherwise, which is what a plain GET renders and what
	// a reader without script keeps.
	data.Form.Role = weakestRole(data.RoleOptions, func(r team.Role) (string, int32) {
		return r.Slug, r.Rank
	})
	if picked, ok := pickRole(data.RoleOptions, r.URL.Query().Get("role")); ok {
		data.Form.Role = picked.Slug
	}
	target, tok := pickMember(data.GrantTargets, r.URL.Query().Get("user_id"))
	ws, wok := pickWorkspace(data.OrgWorkspaces, r.URL.Query().Get("workspace_id"))
	role, rok := pickRole(data.RoleOptions, data.Form.Role)
	if tok && wok && rok {
		data.GrantNote, data.GrantNoteWarn = grantNote(data.Members, target, ws, role)
	}
	return data, true
}

// weakestRole is the slug of the least powerful role in a list, and it is what
// both forms that hand out authority now arrive on (D173, F182).
//
// Rank counts downward — owner is 10 and viewer 40 in 00700_seed.sql — so the
// weakest role is the *highest* rank. Read from the ranks rather than by taking
// the last entry, even though both lists happen to be ordered most powerful
// first: a default that silently becomes "owner" when an ordering changes is
// precisely the defect this closes.
//
// Generic over an accessor because invite.Role and team.Role are the same four
// fields declared twice, in packages that do not import one another. Empty for
// an empty list, which selects nothing — the honest answer when there is
// nothing to select, and a state in which neither form is drawn at all.
func weakestRole[T any](roles []T, of func(T) (slug string, rank int32)) string {
	slug, weakest := "", int32(0)
	for _, r := range roles {
		s, rank := of(r)
		if slug == "" || rank > weakest {
			slug, weakest = s, rank
		}
	}
	return slug
}

// pickMember, pickWorkspace and pickRole resolve one option of a select, and
// they answer only from the list the page itself drew. An id that is not in it
// is not a lookup that failed but an input that has no meaning here, and the
// note is then left off rather than computed against a guess.
func pickMember(targets []team.Member, id string) (team.Member, bool) {
	for _, m := range targets {
		if m.UserID.String() == id {
			return m, true
		}
	}
	if id == "" && len(targets) > 0 {
		return targets[0], true
	}
	return team.Member{}, false
}

func pickWorkspace(workspaces []team.Workspace, id string) (team.Workspace, bool) {
	for _, ws := range workspaces {
		if ws.ID.String() == id {
			return ws, true
		}
	}
	if id == "" && len(workspaces) > 0 {
		return workspaces[0], true
	}
	return team.Workspace{}, false
}

func pickRole(roles []team.Role, slug string) (team.Role, bool) {
	for _, r := range roles {
		if r.Slug == slug {
			return r, true
		}
	}
	return team.Role{}, false
}

// orgWideMembership is the organization-wide membership a person holds, if any:
// the row with no workspace, which covers every workspace in the organization.
// At most one can exist — the COALESCE unique index in 00200_identity.sql is
// what says so — and it is the only membership that can make a workspace-scoped
// grant redundant.
func orgWideMembership(members []team.Member, userID uuid.UUID) (team.Member, bool) {
	for _, m := range members {
		if m.UserID == userID && m.WorkspaceID == nil {
			return m, true
		}
	}
	return team.Member{}, false
}

// membershipInWorkspace is the row scoped to exactly one workspace, which is
// what the uniqueness index refuses a second of.
func membershipInWorkspace(members []team.Member, userID, workspaceID uuid.UUID) (team.Member, bool) {
	for _, m := range members {
		if m.UserID == userID && m.WorkspaceID != nil && *m.WorkspaceID == workspaceID {
			return m, true
		}
	}
	return team.Member{}, false
}

// grantNote answers what pressing Grant access would do, for one selection.
//
// Three outcomes, and only the middle one is the case D31 kept this path open
// for. The grant is refused outright when a membership in that workspace
// already exists; it changes nothing when an organization-wide role the person
// already holds reaches everything the chosen one does; otherwise it widens,
// which is what the form is for.
//
// **It reports, and it never refuses.** D31 rejected refusing a redundant grant
// and F163 did not reopen that: the same path carries the useful *org editor
// plus workspace admin* case, and what was wrong was a page claiming an effect
// that had not occurred. The bool is whether the answer is one to stop at, not
// whether the button is disabled.
//
// Rank rather than a permission-set comparison, and the assumption is worth
// naming: the four built-in roles nest — viewer ⊂ editor ⊂ admin ⊂ owner, in the
// order their ranks put them — and 00700_seed.sql is the only INSERT INTO roles
// in the tree, so nothing else can be held. A composed role at an intermediate
// rank would break the implication, and the day one is writable this has to
// compare the permissions instead.
func grantNote(members []team.Member, target team.Member, ws team.Workspace, role team.Role) (string, bool) {
	if held, ok := membershipInWorkspace(members, target.UserID, ws.ID); ok {
		return fmt.Sprintf(
			"%s already has a role in %s — %s. This form only adds, so a second one "+
				"there is refused; remove that membership first to change it.",
			target.Email, ws.Name, held.Role), true
	}
	orgWide, ok := orgWideMembership(members, target.UserID)
	if ok && orgWide.RoleRank <= role.Rank {
		return fmt.Sprintf(
			"%s already holds %s across every workspace, which reaches everything "+
				"%s does. This would add a membership to the list and nothing to what "+
				"they can do.",
			target.Email, orgWide.Role, role.Slug), true
	}
	if ok {
		return fmt.Sprintf(
			"%s holds %s across every workspace. This makes them %s in %s as well, "+
				"and leaves the rest as it was.",
			target.Email, orgWide.Role, role.Slug, ws.Name), false
	}
	return fmt.Sprintf(
		"This makes %s %s in %s. Their access to the workspaces they already hold "+
			"is unchanged.",
		target.Email, role.Slug, ws.Name), false
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
	case "granted-nochange":
		// F163. The line above is what this page used to say for every grant,
		// including one whose role is weaker than an organization-wide role the
		// person already holds — where a row is inserted, an audit entry is
		// written, a second line appears in the list, and nothing they can do
		// changes at all. The membership is real and is not a failure; the
		// sentence is about what it did.
		data.Notice = "The membership was created, and it changes nothing they can do: " +
			"a role they already hold across every workspace reaches everything this " +
			"one does."
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

	actor := IdentityFrom(r.Context())
	member, err := h.Team.Grant(r.Context(), actor, team.GrantInput{
		UserID: userID, WorkspaceID: workspaceID, Role: r.PostFormValue("role"),
	})
	if err != nil {
		h.renderMembersError(w, r, err)
		return
	}

	// Whether the grant *changed* anything is a second question, and F163 is
	// that the confirmation used to answer it wrongly. Asked of the list rather
	// than of the grant, because the answer is about the memberships this person
	// holds elsewhere and the returned row knows only itself. One extra query on
	// an action somebody performs by hand; a failure here loses the distinction
	// and says the weaker thing, which is the truthful direction to fail in.
	done := "granted"
	if members, merr := h.Team.Members(r.Context(), actor); merr == nil {
		if orgWide, ok := orgWideMembership(members, member.UserID); ok &&
			orgWide.RoleRank <= member.RoleRank {
			done = "granted-nochange"
		}
	}
	seeOther(w, r, "/members?done="+done)
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
