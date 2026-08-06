//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Folders end to end (M38).
//
// Written against ruleFixture rather than a fixture of its own for the reason
// split_test.go gives: it already wires the link service, the dashboard and the
// API against one database, and a second wiring of the same three components
// would be a second thing to keep in step.
//
// The two tests that matter most here are the ones m38.md names —
// TestAFolderCanNeverBecomeItsOwnDescendant and
// TestDeletingAFolderKeepsItsLinks. Both were confirmed to fail against a
// deliberately broken build before being trusted.

func (f *ruleFixture) addFolder(name string, parent *uuid.UUID) *domain.Folder {
	f.t.Helper()
	folder, err := f.links.CreateFolder(f.t.Context(), f.owner,
		link.CreateFolderInput{Name: name, ParentID: parent})
	if err != nil {
		f.t.Fatalf("create folder %q: %v", name, err)
	}
	return folder
}

// folderOf reads a link's folder straight from the column, so an assertion about
// what deleting a folder did to a link is not routed through the same service
// that might be wrong about it.
func (f *ruleFixture) folderOf(linkID uuid.UUID) *uuid.UUID {
	f.t.Helper()
	var got *uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT folder_id FROM links WHERE id = $1`, linkID).Scan(&got); err != nil {
		f.t.Fatalf("read folder_id of %s: %v", linkID, err)
	}
	return got
}

func (f *ruleFixture) linkExists(linkID uuid.UUID) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM links WHERE id = $1 AND deleted_at IS NULL`, linkID).Scan(&n); err != nil {
		f.t.Fatalf("count link %s: %v", linkID, err)
	}
	return n == 1
}

// --- the two named failures ---------------------------------------------------

// TestDeletingAFolderKeepsItsLinks is the failure m38.md names in so many words:
// losing links because a container was deleted.
//
// Asserted over a whole branch rather than one folder, because the cascade is
// what makes the subtree case work: `folders.parent_id ON DELETE CASCADE`
// removes the descendants, and each of those removals fires
// `links.folder_id ON DELETE SET NULL` in turn. A test on a leaf folder would
// pass against an implementation that deleted the links in every folder below
// the one being removed.
func TestDeletingAFolderKeepsItsLinks(t *testing.T) {
	f := newRules(t)
	f.claim()

	parent := f.addFolder("Campaigns", nil)
	child := f.addFolder("Summer", &parent.ID)
	elsewhere := f.addFolder("Docs", nil)

	inParent := f.createFiledLink("in-parent", "https://example.com/1", &parent.ID)
	inChild := f.createFiledLink("in-child", "https://example.com/2", &child.ID)
	untouched := f.createFiledLink("elsewhere", "https://example.com/3", &elsewhere.ID)
	unfiled := f.createLink("unfiled", "https://example.com/4")

	if err := f.links.DeleteFolder(t.Context(), f.owner, parent.ID); err != nil {
		t.Fatalf("delete the parent folder: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   uuid.UUID
	}{
		{"the link filed in the deleted folder", inParent},
		{"the link filed in a folder inside it", inChild},
	} {
		if !f.linkExists(tc.id) {
			t.Fatalf("%s is gone. Deleting a folder must never delete a link: "+
				"links.folder_id is ON DELETE SET NULL precisely so that removing "+
				"a container leaves its contents where a person can still find them.",
				tc.name)
		}
		if got := f.folderOf(tc.id); got != nil {
			t.Errorf("%s still points at folder %s, which no longer exists", tc.name, got)
		}
	}

	// The branch really did go, or the assertions above would be about nothing.
	tree, err := f.links.Folders(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tree.Get(child.ID); ok {
		t.Error("the folder inside the deleted one survived; a subtree is deleted " +
			"whole, and a child left behind would be an orphan nobody asked for")
	}
	if _, ok := tree.Get(elsewhere.ID); !ok {
		t.Error("an unrelated folder was deleted too")
	}
	if got := f.folderOf(untouched); got == nil || *got != elsewhere.ID {
		t.Errorf("a link in an unrelated folder was unfiled: folder_id = %v", got)
	}
	if got := f.folderOf(unfiled); got != nil {
		t.Errorf("an already-unfiled link acquired a folder: %v", got)
	}

	// And the unfiled links are reachable: "still in the table" is not the same
	// promise as "still findable", and the second is the one that matters to
	// somebody who has just tidied up a tree.
	page, err := f.links.List(t.Context(), f.owner, domain.LinkFilter{Unfiled: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := map[uuid.UUID]bool{}
	for _, l := range page.Items {
		found[l.ID] = true
	}
	for _, id := range []uuid.UUID{inParent, inChild, unfiled} {
		if !found[id] {
			t.Errorf("link %s is not in the unfiled list; a link nobody can list "+
				"is lost whatever the table says", id)
		}
	}
	if found[untouched] {
		t.Error("a link still in a folder was listed as unfiled")
	}
}

// TestAFolderCannotBecomeItsOwnDescendantOverTheAPI is the cycle rule at the
// surface a client actually meets.
//
// The rule itself is exercised over every shape in
// internal/domain/folder_test.go; this is the assertion that the API applies it
// and that the tree survives the attempt.
func TestAFolderCannotBecomeItsOwnDescendantOverTheAPI(t *testing.T) {
	f := newRules(t)
	f.claim()

	top := f.addFolder("Top", nil)
	mid := f.addFolder("Mid", &top.ID)
	bottom := f.addFolder("Bottom", &mid.ID)

	for _, tc := range []struct {
		name string
		into uuid.UUID
	}{
		{"into itself", top.ID},
		{"into its child", mid.ID},
		{"into its grandchild", bottom.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.postJSON("/api/v1/folders/"+top.ID.String()+"/move",
				`{"parent_id":"`+tc.into.String()+`"}`)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("move = %d, want 422", resp.StatusCode)
			}
		})
	}

	// The tree is unchanged, which is the assertion the status code alone does
	// not make: a refusal that had already written would leave three folders
	// nothing could reach.
	tree, err := f.links.Folders(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Len() != 3 {
		t.Fatalf("the workspace has %d folders after three refused moves, want 3", tree.Len())
	}
	got, ok := tree.Get(top.ID)
	if !ok || got.ParentID != nil || got.Depth != 1 {
		t.Fatalf("the top folder is now %+v; a refused move must change nothing", got)
	}
	if b, _ := tree.Get(bottom.ID); b.Depth != 3 {
		t.Errorf("the branch's shape changed: bottom is at depth %d, want 3", b.Depth)
	}
}

// --- the rest of the service rules --------------------------------------------

func TestSiblingsMayNotShareANameButFoldersElsewhereMay(t *testing.T) {
	f := newRules(t)
	f.claim()

	parent := f.addFolder("Campaigns", nil)
	f.addFolder("Archive", &parent.ID)
	// The same name in a different place is fine.
	f.addFolder("Archive", nil)

	_, err := f.links.CreateFolder(t.Context(), f.owner,
		link.CreateFolderInput{Name: "archive", ParentID: &parent.ID})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || ve[0].Code != "conflict" {
		t.Fatalf("creating a second Archive inside Campaigns returned %v, want a "+
			"conflict: the match is case-insensitive, like the unique index in 02400", err)
	}

	// Renaming into a collision is refused the same way, and renaming a folder to
	// a different case of its own name is not a collision.
	if _, err := f.links.RenameFolder(t.Context(), f.owner, parent.ID, "ARCHIVE"); err == nil {
		t.Error("renaming Campaigns to ARCHIVE beside the top-level Archive was allowed")
	}
	sub, err := f.links.Folders(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := sub.SiblingNamed(&parent.ID, "Archive")
	if _, err := f.links.RenameFolder(t.Context(), f.owner, inner.ID, "ARCHIVE"); err != nil {
		t.Errorf("renaming a folder to another case of its own name was refused: %v", err)
	}
}

func TestTheDepthCapIsEnforcedOnCreateAndOnMove(t *testing.T) {
	f := newRules(t)
	f.claim()

	// Build the deepest legal chain.
	var parent *uuid.UUID
	var ids []uuid.UUID
	for i := range domain.MaxFolderDepth {
		folder := f.addFolder("level"+string(rune('a'+i)), parent)
		ids = append(ids, folder.ID)
		parent = &folder.ID
	}

	_, err := f.links.CreateFolder(t.Context(), f.owner,
		link.CreateFolderInput{Name: "one-too-deep", ParentID: parent})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || ve[0].Code != "too_deep" {
		t.Fatalf("creating a folder below the cap returned %v, want too_deep", err)
	}

	// Moving a two-level branch under the second-to-last level is refused, and
	// moving a single folder there is not — the distinction the cap exists for.
	branch := f.addFolder("branch", nil)
	f.addFolder("branch-child", &branch.ID)
	leaf := f.addFolder("leaf", nil)
	penultimate := ids[domain.MaxFolderDepth-2]

	if _, err := f.links.MoveFolder(t.Context(), f.owner, branch.ID, &penultimate); err == nil {
		t.Error("a two-level branch was moved into the last permitted level; its " +
			"child would sit one below the cap")
	}
	if _, err := f.links.MoveFolder(t.Context(), f.owner, leaf.ID, &penultimate); err != nil {
		t.Errorf("a single folder was refused the last permitted level: %v", err)
	}
}

// TestTwoConcurrentMovesCannotBuildACycle is F108, and it reopens
// [M38](../../docs/build-notes/phase-details/m38.md)'s own claim that a folder
// can never become its own descendant.
//
// MoveRefusal answers "is the new parent inside the subtree being moved", a
// question about the whole tree that cannot be written as a column check. It was
// computed from a tree read on its own connection and then written outside that
// read, so two moves a few milliseconds apart each passed a check the other
// invalidated: A under B while B moves under A satisfies both and detaches the
// pair from the root, reachable from nothing.
//
// **The test drives the mechanism, not the race.** F67 established that racing
// goroutines cannot reach a window of tens of microseconds, and that merely
// holding a row proves nothing when the write blocks on it anyway. What
// discriminates is moving the tree *under* a request already parked on the lock:
// the parked move read a tree in which its destination was legal, and resumes
// against one in which it is not. Re-reading refuses it; not re-reading writes
// the cycle.
func TestTwoConcurrentMovesCannotBuildACycle(t *testing.T) {
	f := newRules(t)
	f.claim()
	ctx := t.Context()

	a, err := f.links.CreateFolder(ctx, f.owner, link.CreateFolderInput{Name: "A"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.links.CreateFolder(ctx, f.owner, link.CreateFolderInput{Name: "B"})
	if err != nil {
		t.Fatal(err)
	}

	// Hold the workspace's folders exactly as MoveFolder's own lock does.
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx,
		`SELECT id FROM folders WHERE workspace_id = $1 AND deleted_at IS NULL
		  ORDER BY id FOR UPDATE`, f.owner.WorkspaceID); err != nil {
		t.Fatal(err)
	}

	// Move A under B, parked on that lock. At the moment it was issued this is
	// legal: B is not inside A.
	parked := make(chan error, 1)
	go func() {
		_, err := f.links.MoveFolder(context.Background(), f.owner, a.ID, &b.ID)
		parked <- err
	}()
	select {
	case err := <-parked:
		t.Fatalf("the move did not park on the held lock; it returned %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// Now move B under A from the holding transaction, and release. The parked
	// move resumes against a tree where its destination is inside its own
	// subtree.
	if _, err := holder.Exec(ctx,
		`UPDATE folders SET parent_id = $2 WHERE id = $1`, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var ve domain.ValidationErrors
	if err := <-parked; !errors.As(err, &ve) {
		t.Errorf("a move that waited while its destination moved into its own "+
			"subtree answered %v; want the cycle refusal, which needs the tree "+
			"re-read inside the transaction that writes", err)
	}

	// And no cycle exists: walking up from either folder reaches the root.
	for _, start := range []uuid.UUID{a.ID, b.ID} {
		var depth int
		if err := f.pool.QueryRow(ctx, `
			WITH RECURSIVE up(id, parent_id, n) AS (
			    SELECT id, parent_id, 0 FROM folders WHERE id = $1
			    UNION ALL
			    SELECT f.id, f.parent_id, up.n + 1
			      FROM folders f JOIN up ON f.id = up.parent_id
			     WHERE up.n < 50
			)
			SELECT max(n) FROM up`, start).Scan(&depth); err != nil {
			t.Fatal(err)
		}
		if depth >= 50 {
			t.Errorf("walking up from %s ran to the recursion bound: the folder is "+
				"its own descendant, which is what M38 says cannot happen", start)
		}
	}
}

// TestACreateThatWaitedOutAMoveCannotBreachTheDepthCap reopens the depth cap
// the way F108 reopened the cycle rule, on the create path this time.
//
// Create and move guard the cap with different predicates — create asks how
// deep the parent sits, move asks how far the subtree reaches — so a create
// under a top-level leaf and a move of that leaf to the last permitted level
// each pass while their composition lands the new folder one past the cap. The
// insert's own blocking repairs nothing: a nested create takes FOR KEY SHARE
// on its parent's row, waits out the move's FOR UPDATE, and then proceeds on
// the decision it made before waiting. As with the cycle, the discriminating
// drive is a request parked on the lock while the tree changes under it:
// re-reading refuses it, not re-reading writes a folder below the cap.
func TestACreateThatWaitedOutAMoveCannotBreachTheDepthCap(t *testing.T) {
	f := newRules(t)
	f.claim()
	ctx := t.Context()

	// A chain one short of the cap, and a top-level leaf. Moving the leaf
	// under the chain's end is legal — its depth becomes exactly the cap — and
	// creating under the leaf while it is top-level is legal too. Both
	// together are not.
	var parent *uuid.UUID
	for i := range domain.MaxFolderDepth - 1 {
		folder := f.addFolder("chain"+string(rune('a'+i)), parent)
		parent = &folder.ID
	}
	deepest := *parent
	leaf := f.addFolder("leaf", nil)

	// Hold the workspace's folders exactly as the service's own lock does.
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx,
		`SELECT id FROM folders WHERE workspace_id = $1 AND deleted_at IS NULL
		  ORDER BY id FOR UPDATE`, f.owner.WorkspaceID); err != nil {
		t.Fatal(err)
	}

	// Create under the leaf, parked on that lock. At the moment it was issued
	// this is legal: the leaf is at the top level.
	parked := make(chan error, 1)
	go func() {
		_, err := f.links.CreateFolder(context.Background(), f.owner,
			link.CreateFolderInput{Name: "one-too-deep", ParentID: &leaf.ID})
		parked <- err
	}()
	select {
	case err := <-parked:
		t.Fatalf("the create did not park on the held lock; it returned %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// Now move the leaf to the last permitted level from the holding
	// transaction, and release. The parked create resumes against a tree in
	// which its parent sits at the cap.
	if _, err := holder.Exec(ctx,
		`UPDATE folders SET parent_id = $2 WHERE id = $1`, leaf.ID, deepest); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var ve domain.ValidationErrors
	if err := <-parked; !errors.As(err, &ve) || ve[0].Code != "too_deep" {
		t.Errorf("a create that waited while its parent moved to the deepest "+
			"permitted level answered %v; want too_deep, which needs the tree "+
			"re-read inside the transaction that writes", err)
	}

	// And nothing sits past the cap, which is the claim the status code alone
	// does not make.
	tree, err := f.links.Folders(ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, folder := range tree.Flat() {
		if folder.Depth > domain.MaxFolderDepth {
			t.Errorf("folder %q sits at depth %d, past the cap of %d",
				folder.Name, folder.Depth, domain.MaxFolderDepth)
		}
	}
}

// TestTheFolderCountCapRefusesTheFolderPastIt is the count cap, serially: the
// folder that reaches domain.MaxFoldersPerWorkspace is allowed, the one past
// it is refused, and deleting one makes room again — a count, not a ratchet.
func TestTheFolderCountCapRefusesTheFolderPastIt(t *testing.T) {
	f := newRules(t)
	f.claim()
	ctx := t.Context()

	// Everything up to one below the cap goes straight into the table: five
	// hundred service calls would each read the whole tree, and the check
	// under test is the one the last two calls make.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO folders (id, workspace_id, name)
		 SELECT gen_random_uuid(), $1, 'bulk-' || n
		   FROM generate_series(1, $2) n`,
		f.owner.WorkspaceID, domain.MaxFoldersPerWorkspace-1); err != nil {
		t.Fatal(err)
	}

	last := f.addFolder("the-last-one", nil)

	_, err := f.links.CreateFolder(ctx, f.owner,
		link.CreateFolderInput{Name: "one-past-the-cap"})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || ve[0].Code != "too_many" {
		t.Fatalf("creating folder %d returned %v, want too_many",
			domain.MaxFoldersPerWorkspace+1, err)
	}

	if err := f.links.DeleteFolder(ctx, f.owner, last.ID); err != nil {
		t.Fatal(err)
	}
	f.addFolder("room-again", nil)
}

// TestACreateThatWaitedOutAnotherCreateCannotBreachTheCountCap is the count
// half of the concurrent reopening. Two creates arriving with the workspace
// one below the cap each count MaxFoldersPerWorkspace-1 folders and both
// pass; whichever writes second puts the workspace one past it. A top-level
// create references no parent row, so nothing on the write path so much as
// delays it — this is the case where only the service's own lock stands
// between the stale count and the insert, which is why the park assertion
// below is itself part of the test.
func TestACreateThatWaitedOutAnotherCreateCannotBreachTheCountCap(t *testing.T) {
	f := newRules(t)
	f.claim()
	ctx := t.Context()

	if _, err := f.pool.Exec(ctx,
		`INSERT INTO folders (id, workspace_id, name)
		 SELECT gen_random_uuid(), $1, 'bulk-' || n
		   FROM generate_series(1, $2) n`,
		f.owner.WorkspaceID, domain.MaxFoldersPerWorkspace-1); err != nil {
		t.Fatal(err)
	}

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx,
		`SELECT id FROM folders WHERE workspace_id = $1 AND deleted_at IS NULL
		  ORDER BY id FOR UPDATE`, f.owner.WorkspaceID); err != nil {
		t.Fatal(err)
	}

	// The create that loses the race, parked on the lock. At the moment it
	// was issued it is legal: one slot remains.
	parked := make(chan error, 1)
	go func() {
		_, err := f.links.CreateFolder(context.Background(), f.owner,
			link.CreateFolderInput{Name: "second-to-arrive"})
		parked <- err
	}()
	select {
	case err := <-parked:
		t.Fatalf("the create did not park on the held lock; it returned %v — "+
			"a top-level create blocks on no row of its own, so only the "+
			"service taking the workspace lock can make it wait", err)
	case <-time.After(500 * time.Millisecond):
	}

	// The create that wins, from the holding transaction, and release. The
	// parked create resumes against a workspace with no slot left.
	if _, err := holder.Exec(ctx,
		`INSERT INTO folders (id, workspace_id, name)
		 VALUES (gen_random_uuid(), $1, 'first-to-arrive')`, f.owner.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var ve domain.ValidationErrors
	if err := <-parked; !errors.As(err, &ve) || ve[0].Code != "too_many" {
		t.Errorf("a create that waited while another filled the last slot "+
			"answered %v; want too_many, which needs the count re-taken inside "+
			"the transaction that writes", err)
	}

	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM folders WHERE workspace_id = $1 AND deleted_at IS NULL`,
		f.owner.WorkspaceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != domain.MaxFoldersPerWorkspace {
		t.Errorf("the workspace holds %d folders, want exactly %d",
			n, domain.MaxFoldersPerWorkspace)
	}
}

func TestALinkCannotBeFiledIntoAnotherWorkspacesFolder(t *testing.T) {
	f := newRules(t)
	f.claim()

	// A folder id that is a real uuid and belongs to nobody here reaches the same
	// refusal a cross-tenant one does — GetFolder is scoped to the workspace, so
	// "another workspace's" and "no such folder" are one answer by construction.
	stranger := uuid.Must(uuid.NewV7())
	_, err := f.links.Create(t.Context(), f.owner, link.CreateInput{
		Alias: "filed-elsewhere", URL: "https://example.com/x", FolderID: &stranger,
	})
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || ve[0].Field != "folder_id" {
		t.Fatalf("creating a link in an unknown folder returned %v, want a "+
			"folder_id validation error rather than a foreign-key failure", err)
	}
}

// --- the links list -----------------------------------------------------------

func TestTheLinkListFiltersByFolder(t *testing.T) {
	f := newRules(t)
	f.claim()

	campaigns := f.addFolder("Campaigns", nil)
	summer := f.addFolder("Summer", &campaigns.ID)

	inCampaigns := f.createFiledLink("camp1", "https://example.com/c1", &campaigns.ID)
	inSummer := f.createFiledLink("summ1", "https://example.com/s1", &summer.ID)
	loose := f.createLink("loose", "https://example.com/loose")

	// One folder, not its subtree. The count beside a folder on the tree page is
	// this number, and a parent that reported its descendants' links would
	// disagree with the list it filters.
	ids := f.listIDs(domain.LinkFilter{FolderID: &campaigns.ID, IncludeTotal: true})
	if len(ids) != 1 || ids[0] != inCampaigns {
		t.Fatalf("?folder=Campaigns returned %v, want only %s", ids, inCampaigns)
	}
	if ids := f.listIDs(domain.LinkFilter{FolderID: &summer.ID}); len(ids) != 1 || ids[0] != inSummer {
		t.Fatalf("?folder=Summer returned %v, want only %s", ids, inSummer)
	}
	if ids := f.listIDs(domain.LinkFilter{Unfiled: true}); len(ids) != 1 || ids[0] != loose {
		t.Fatalf("?folder=none returned %v, want only %s", ids, loose)
	}
	if got := len(f.listIDs(domain.LinkFilter{})); got != 3 {
		t.Fatalf("the unfiltered list returned %d links, want 3", got)
	}

	// The total must describe the same set as the items beside it, which is the
	// bug the tag filter already had once.
	page, err := f.links.List(t.Context(), f.owner,
		domain.LinkFilter{FolderID: &campaigns.ID, IncludeTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total == nil || *page.Total != 1 {
		t.Fatalf("a folder-filtered page reported a total of %v, want 1: the count "+
			"has to apply the same filter the page did", page.Total)
	}

	// And over HTTP, where the filter is a word rather than a struct field.
	body := f.getJSON("/api/v1/links?folder=" + campaigns.ID.String())
	if !strings.Contains(body, `"alias":"camp1"`) || strings.Contains(body, `"alias":"summ1"`) {
		t.Errorf("GET /api/v1/links?folder= returned the wrong set:\n%s", body)
	}
	if body := f.getJSON("/api/v1/links?folder=none"); !strings.Contains(body, `"alias":"loose"`) {
		t.Errorf("GET /api/v1/links?folder=none did not return the unfiled link:\n%s", body)
	}
}

func (f *ruleFixture) listIDs(filter domain.LinkFilter) []uuid.UUID {
	f.t.Helper()
	page, err := f.links.List(f.t.Context(), f.owner, filter)
	if err != nil {
		f.t.Fatalf("list links: %v", err)
	}
	out := make([]uuid.UUID, 0, len(page.Items))
	for _, l := range page.Items {
		out = append(out, l.ID)
	}
	return out
}

func (f *ruleFixture) createFiledLink(alias, dest string, folder *uuid.UUID) uuid.UUID {
	f.t.Helper()
	l, err := f.links.Create(f.t.Context(), f.owner,
		link.CreateInput{Alias: alias, URL: dest, FolderID: folder})
	if err != nil {
		f.t.Fatalf("create /%s in folder %v: %v", alias, folder, err)
	}
	return l.ID
}

// --- the dashboard ------------------------------------------------------------

// TestTheFolderTreeWorksWithoutJavaScript is the progressive-enhancement claim,
// asserted rather than described.
//
// Every step here is a plain form post followed by a redirect, which is what a
// browser with scripting off does. No request carries the HX-Request header, so
// nothing on this path can depend on htmx being present.
func TestTheFolderTreeWorksWithoutJavaScript(t *testing.T) {
	f := newRules(t)
	f.claim()

	// Create, from the page's own form.
	f.postFolderForm(t, "/folders", url.Values{"name": {"Campaigns"}})
	tree, _ := f.links.Folders(t.Context(), f.owner)
	campaigns, ok := tree.SiblingNamed(nil, "Campaigns")
	if !ok {
		t.Fatal("the create form did not make a folder")
	}
	f.postFolderForm(t, "/folders", url.Values{
		"name": {"Summer"}, "parent_id": {campaigns.ID.String()},
	})
	f.postFolderForm(t, "/folders", url.Values{"name": {"Docs"}})

	tree, _ = f.links.Folders(t.Context(), f.owner)
	summer, _ := tree.SiblingNamed(&campaigns.ID, "Summer")
	docs, _ := tree.SiblingNamed(nil, "Docs")

	// The move page: click "Move" on Summer, and the destinations are rendered as
	// forms rather than as drag targets.
	page := f.getHTML("/folders?move=" + summer.ID.String())
	if !strings.Contains(page, `action="/folders/`+summer.ID.String()+`/move"`) {
		t.Fatalf("the move page offers no form posting to Summer's move URL:\n%s", page)
	}
	// The destination buttons are hidden inputs inside the move forms, which is
	// what distinguishes them from the create form's parent select — that lists
	// every folder, including ones no move may target.
	destination := func(id uuid.UUID) string {
		return `<input type="hidden" name="parent_id" value="` + id.String() + `">`
	}
	if !strings.Contains(page, destination(docs.ID)) {
		t.Error("Docs is a legal destination for Summer but the page offers no " +
			"button for it")
	}
	// Its own parent is not offered, because moving a folder to where it already
	// is changes nothing.
	if strings.Contains(page, destination(campaigns.ID)) {
		t.Error("the page offers to move Summer into the folder it is already in")
	}
	if strings.Contains(page, destination(summer.ID)) {
		t.Error("the page offers to move Summer into itself; the cycle rule has to " +
			"reach the rendering as well as the write")
	}
	// **No drag-and-drop, deliberately.** M38 says so, and this is the assertion
	// rather than a comment: a `draggable` attribute or a drop handler appearing
	// here would mean the feature had quietly acquired custom JavaScript, which
	// `ui` has no build step for and the CSP does not permit.
	for _, forbidden := range []string{"draggable", "ondragstart", "ondrop", "dragover"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("the folder page carries %q; moving is click-to-move via POST "+
				"forms, and drag-and-drop is deliberately out", forbidden)
		}
	}

	// Second click: move it.
	f.postFolderForm(t, "/folders/"+summer.ID.String()+"/move",
		url.Values{"parent_id": {docs.ID.String()}})
	tree, _ = f.links.Folders(t.Context(), f.owner)
	moved, _ := tree.Get(summer.ID)
	if moved.ParentID == nil || *moved.ParentID != docs.ID {
		t.Fatalf("Summer's parent is %v, want Docs (%s)", moved.ParentID, docs.ID)
	}

	// Rename, and then move back to the top level — the destination that is not a
	// row on the page.
	f.postFolderForm(t, "/folders/"+summer.ID.String(), url.Values{"name": {"Autumn"}})
	f.postFolderForm(t, "/folders/"+summer.ID.String()+"/move", url.Values{"parent_id": {""}})
	tree, _ = f.links.Folders(t.Context(), f.owner)
	back, _ := tree.Get(summer.ID)
	if back.Name != "Autumn" || back.ParentID != nil {
		t.Fatalf("after rename and move to the root the folder is %+v", back)
	}

	// Delete.
	f.postFolderForm(t, "/folders/"+summer.ID.String()+"/delete", url.Values{})
	tree, _ = f.links.Folders(t.Context(), f.owner)
	if _, ok := tree.Get(summer.ID); ok {
		t.Error("the delete form did not delete the folder")
	}
}

// postFolderForm posts a dashboard form the way a browser without JavaScript
// does, and insists on the redirect that makes the back button safe.
func (f *ruleFixture) postFolderForm(t *testing.T, path string, vals url.Values) {
	t.Helper()
	resp := f.postForm(path, vals)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body := make([]byte, 2048)
		n, _ := resp.Body.Read(body)
		t.Fatalf("POST %s = %d, want 303 (a form post has to redirect, or a "+
			"reload offers to resubmit it)\n%s", path, resp.StatusCode, body[:n])
	}
}

// TestTheLinkPageFilesALinkAndTakesItOutAgain is the UI half of "links are
// assignable to folders in API and UI".
func TestTheLinkPageFilesALinkAndTakesItOutAgain(t *testing.T) {
	f := newRules(t)
	f.claim()

	folder := f.addFolder("Campaigns", nil)
	id := f.createLink("filed", "https://example.com/filed")

	form := url.Values{
		"url": {"https://example.com/filed"}, "alias": {"filed"},
		"title": {""}, "description": {""}, "expires_at": {""}, "tags": {""},
		"max_clicks": {""}, "folder_id": {folder.ID.String()},
	}
	resp := f.postForm("/links/"+id.String(), form)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("filing the link returned %d, want 303", resp.StatusCode)
	}
	if got := f.folderOf(id); got == nil || *got != folder.ID {
		t.Fatalf("the link's folder is %v, want %s", got, folder.ID)
	}

	// The select's first option is empty, and the form posts every field, so an
	// empty value is how a link comes back out of a folder.
	form.Set("folder_id", "")
	resp = f.postForm("/links/"+id.String(), form)
	_ = resp.Body.Close()
	if got := f.folderOf(id); got != nil {
		t.Fatalf("the link is still filed in %v; an empty folder select has to "+
			"unfile it, or a link can be put in a folder and never taken out", got)
	}
}
