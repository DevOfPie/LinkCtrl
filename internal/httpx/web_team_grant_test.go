package httpx

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/team"
)

// M58, on the two authority surfaces: which role a creation form arrives on
// (F182), and what the page is allowed to claim a grant did (F163).
//
// Unit tests rather than integration ones because the subjects are decisions
// taken over a list that a service has already produced. The list itself — that
// `Roles` is capped at the actor's rank, that `Grant` refuses across scopes — is
// covered where it belongs, in test/integration/team_test.go, and repeating it
// here would test the fixture.

// F182. Both forms that hand out authority default to the lowest role their
// actor may assign, which is D173's answer over requiring a deliberate choice.
//
// The four-role case is the finding as it was found: an owner is offered
// [Owner, Admin, Editor, Viewer] and, with nothing selected, a browser takes
// the first. The admin case is the same defect one rank down, and it is why
// "not owner" would have been the wrong assertion — the default was the
// *actor's* maximum, not the instance's.
func TestWeakestRoleIsTheDefaultOnBothCreationForms(t *testing.T) {
	teamRoles := func(slugs ...string) []team.Role {
		ranks := map[string]int32{"owner": 10, "admin": 20, "editor": 30, "viewer": 40}
		out := make([]team.Role, 0, len(slugs))
		for _, s := range slugs {
			out = append(out, team.Role{Slug: s, Rank: ranks[s]})
		}
		return out
	}
	of := func(r team.Role) (string, int32) { return r.Slug, r.Rank }

	cases := []struct {
		name  string
		roles []team.Role
		want  string
	}{
		{"an owner", teamRoles("owner", "admin", "editor", "viewer"), "viewer"},
		{"an admin", teamRoles("admin", "editor", "viewer"), "viewer"},
		{"a key, capped at editor by D43", teamRoles("editor", "viewer"), "viewer"},
		{"one role on offer", teamRoles("viewer"), "viewer"},
		{"nothing on offer", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := weakestRole(tc.roles, of); got != tc.want {
				t.Errorf("default role = %q, want %q", got, tc.want)
			}
		})
	}

	// Order is not what decides it. Both services happen to return their roles
	// most powerful first, and a default that reads "the last entry" would
	// become "owner" the day one of them stops — which is the defect, arriving
	// from the other direction.
	reversed := teamRoles("viewer", "editor", "admin", "owner")
	if got := weakestRole(reversed, of); got != "viewer" {
		t.Errorf("with the list reversed the default is %q; it is read from the "+
			"ranks or it is read from the ordering, and the ordering is not a promise", got)
	}

	// The invitation form's list is the same four fields under a different
	// package, and it gets the same answer or the two surfaces disagree.
	inviteRoles := []invite.Role{
		{Slug: "owner", Rank: 10}, {Slug: "admin", Rank: 20},
		{Slug: "editor", Rank: 30}, {Slug: "viewer", Rank: 40},
	}
	got := weakestRole(inviteRoles, func(r invite.Role) (string, int32) { return r.Slug, r.Rank })
	if got != "viewer" {
		t.Errorf("the invitation form defaults to %q, want viewer", got)
	}
}

// grantFixture is one organization's memberships as /members loads them: an
// organization-wide admin, somebody scoped to a single workspace, and the
// workspaces they are granted in.
type grantFixture struct {
	orgAdmin  uuid.UUID
	scoped    uuid.UUID
	marketing uuid.UUID
	finance   uuid.UUID
	members   []team.Member
}

func newGrantFixture() grantFixture {
	f := grantFixture{
		orgAdmin:  uuid.MustParse("0198c9c5-0000-7000-8000-00000000001c"),
		scoped:    uuid.MustParse("0198c9c5-0000-7000-8000-00000000001d"),
		marketing: uuid.MustParse("0198c9c5-0000-7000-8000-000000000011"),
		finance:   uuid.MustParse("0198c9c5-0000-7000-8000-000000000012"),
	}
	marketing := f.marketing
	f.members = []team.Member{
		{UserID: f.orgAdmin, Email: "admin@example.com", Role: "admin", RoleRank: 20},
		{UserID: f.scoped, Email: "scoped@example.com", Role: "editor", RoleRank: 30,
			WorkspaceID: &marketing, WorkspaceName: "Marketing"},
	}
	return f
}

// F163. The grant form says what this grant would do, and the three answers are
// different acts.
//
// The middle one is the case D31 kept the path open for and the reason this is
// a message rather than a refusal: *org editor plus workspace admin* travels the
// same code as the grant that changes nothing, and refusing the second would
// take the first with it.
func TestTheGrantNoteDistinguishesAddingFromChangingNothing(t *testing.T) {
	f := newGrantFixture()
	admin := team.Role{Slug: "admin", Rank: 20}
	editor := team.Role{Slug: "editor", Rank: 30}
	finance := team.Workspace{ID: f.finance, Name: "Finance"}
	marketing := team.Workspace{ID: f.marketing, Name: "Marketing"}

	target := f.members[0] // the organization-wide admin

	t.Run("weaker than a role they already hold everywhere", func(t *testing.T) {
		note, warn := grantNote(f.members, target, finance, editor)
		if !warn {
			t.Error("a grant that changes nothing is not flagged")
		}
		for _, want := range []string{"admin@example.com", "already holds admin", "nothing to what they can do"} {
			if !strings.Contains(note, want) {
				t.Errorf("the note is missing %q: %s", want, note)
			}
		}
	})

	t.Run("equal to a role they already hold everywhere", func(t *testing.T) {
		// Not a subset but the same set, and the same no-op. Asserted because
		// the comparison is <= and an off-by-one there would report a change.
		if _, warn := grantNote(f.members, target, finance, admin); !warn {
			t.Error("granting the role they already hold organization-wide is " +
				"reported as adding something")
		}
	})

	t.Run("stronger than the role they hold everywhere", func(t *testing.T) {
		weakerOrgWide := []team.Member{{
			UserID: f.orgAdmin, Email: "admin@example.com", Role: "editor", RoleRank: 30,
		}}
		note, warn := grantNote(weakerOrgWide, weakerOrgWide[0], finance, admin)
		if warn {
			t.Error("the useful case D31 exists for is flagged as changing nothing")
		}
		if !strings.Contains(note, "admin in Finance") {
			t.Errorf("the note does not say what is being added: %s", note)
		}
	})

	t.Run("no organization-wide membership at all", func(t *testing.T) {
		note, warn := grantNote(f.members, f.members[1], finance, editor)
		if warn {
			t.Error("granting a second workspace to somebody scoped to one is " +
				"flagged as changing nothing")
		}
		if !strings.Contains(note, "editor in Finance") {
			t.Errorf("the note does not say what is being added: %s", note)
		}
	})

	t.Run("a membership in that workspace already", func(t *testing.T) {
		// The unique index refuses this one, and a note promising it would be a
		// second false claim beside the one being fixed.
		note, warn := grantNote(f.members, f.members[1], marketing, admin)
		if !warn {
			t.Error("a grant the uniqueness index will refuse is not flagged")
		}
		if !strings.Contains(note, "already has a role in Marketing") {
			t.Errorf("the note does not name the membership in the way: %s", note)
		}
	})
}

// The two lookups the note and the confirmation are both built on. Asymmetric
// on purpose: an organization-wide membership covers every workspace, and a
// workspace-scoped one covers exactly its own.
func TestMembershipLookupsAreScopedTheWayAuthorityIs(t *testing.T) {
	f := newGrantFixture()

	if m, ok := orgWideMembership(f.members, f.orgAdmin); !ok || m.Role != "admin" {
		t.Errorf("the organization-wide admin was not found: %v, %v", m, ok)
	}
	if _, ok := orgWideMembership(f.members, f.scoped); ok {
		t.Error("a workspace-scoped membership was read as an organization-wide one; " +
			"that is F27's mistake, and it would make every grant to that person " +
			"look redundant")
	}
	if _, ok := membershipInWorkspace(f.members, f.scoped, f.finance); ok {
		t.Error("a membership in Marketing answered for Finance")
	}
	if m, ok := membershipInWorkspace(f.members, f.scoped, f.marketing); !ok || m.Role != "editor" {
		t.Errorf("the membership in Marketing was not found: %v, %v", m, ok)
	}
	if _, ok := membershipInWorkspace(f.members, f.orgAdmin, f.marketing); ok {
		t.Error("the organization-wide membership answered as a row in Marketing; " +
			"the uniqueness index refuses a second scoped row, not a scoped row " +
			"beside an organization-wide one")
	}
}

// The selects resolve against the list the page drew, and against nothing else.
//
// An id that is not on the page is not a lookup that failed: it is an input
// with no meaning here, and computing a note against a guess would put a
// sentence about somebody else beside the button.
func TestTheGrantFormResolvesOnlyWhatItOffered(t *testing.T) {
	f := newGrantFixture()
	targets := []team.Member{f.members[0], f.members[1]}

	if m, ok := pickMember(targets, ""); !ok || m.UserID != f.orgAdmin {
		t.Error("with nothing chosen the first option is not what the note describes")
	}
	if m, ok := pickMember(targets, f.scoped.String()); !ok || m.UserID != f.scoped {
		t.Error("a chosen target was not resolved")
	}
	if _, ok := pickMember(targets, uuid.Nil.String()); ok {
		t.Error("an id the page never offered resolved to somebody")
	}
	if _, ok := pickMember(nil, ""); ok {
		t.Error("an empty target list resolved to somebody")
	}

	workspaces := []team.Workspace{{ID: f.marketing, Name: "Marketing"}, {ID: f.finance, Name: "Finance"}}
	if ws, ok := pickWorkspace(workspaces, ""); !ok || ws.ID != f.marketing {
		t.Error("with nothing chosen the first workspace is not what the note describes")
	}
	if _, ok := pickWorkspace(workspaces, uuid.Nil.String()); ok {
		t.Error("a workspace the page never offered resolved")
	}

	roles := []team.Role{{Slug: "admin", Rank: 20}, {Slug: "editor", Rank: 30}}
	if r, ok := pickRole(roles, "editor"); !ok || r.Rank != 30 {
		t.Error("a role on offer was not resolved")
	}
	if _, ok := pickRole(roles, "owner"); ok {
		t.Error("a role above the actor's own rank resolved out of a list that " +
			"does not contain it")
	}
}
