package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The folder tree's structural rules (M38).
//
// These run without a database on purpose. The cycle rule and the depth cap are
// the two failures m38.md names, and both are properties of a *shape* rather
// than of storage — so they are tested where every shape can be built in three
// lines, and again end to end in test/integration/folders_test.go where the
// question is whether the API actually applies them.

// fid makes a stable, ordered id from a small number, so a test's tree reads in
// the order it is written.
func fid(n byte) uuid.UUID {
	var u uuid.UUID
	u[0], u[6], u[8], u[15] = 1, 0x70, 0x80, n
	return u
}

// chain builds a straight line of folders, root first, and returns the tree.
func chain(names ...string) FolderTree {
	folders := make([]Folder, 0, len(names))
	for i, name := range names {
		f := Folder{ID: fid(byte(i + 1)), Name: name}
		if i > 0 {
			parent := fid(byte(i))
			f.ParentID = &parent
		}
		folders = append(folders, f)
	}
	return NewFolderTree(folders)
}

func TestAFolderCanNeverBecomeItsOwnDescendant(t *testing.T) {
	// a > b > c
	tree := chain("a", "b", "c")
	a, b, c := fid(1), fid(2), fid(3)

	for _, tc := range []struct {
		name           string
		move           uuid.UUID
		into           *uuid.UUID
		wantCycle      bool
		whatItProtects string
	}{
		{
			name: "into itself", move: a, into: &a, wantCycle: true,
			whatItProtects: "a folder whose parent is itself is unreachable from " +
				"the roots, so it and everything under it disappears from the tree",
		},
		{
			name: "into its own child", move: a, into: &b, wantCycle: true,
			whatItProtects: "the whole branch would be reachable only from inside " +
				"itself",
		},
		{
			name: "into its own grandchild", move: a, into: &c, wantCycle: true,
			whatItProtects: "the same loop, one level further down, which is the " +
				"case a self-and-parent check misses",
		},
		{
			name: "into its own parent", move: c, into: &b, wantCycle: false,
			whatItProtects: "moving downward is ordinary and must not be refused",
		},
		{
			name: "up to the root", move: c, into: nil, wantCycle: false,
			whatItProtects: "the top level is always a legal destination",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusal := tree.MoveRefusal(tc.move, tc.into)
			gotCycle := refusal != nil && refusal.Code == "cycle"
			if gotCycle != tc.wantCycle {
				t.Fatalf("MoveRefusal = %v, want cycle=%v.\nWithout this, %s.",
					refusal, tc.wantCycle, tc.whatItProtects)
			}
		})
	}
}

// TestTheDepthCapCountsTheSubtreeBeingMovedAndNotOnlyTheFolder is the case a
// per-folder depth check gets wrong.
//
// A line one short of the cap has room for exactly one more level. Dropping a
// *leaf* there is fine and must stay fine; dropping a two-level branch there
// puts its own child one level past the cap. Checking the folder rather than
// its subtree accepts both, and leaves a tree deeper than anything the create
// path would let anybody build by hand.
func TestTheDepthCapCountsTheSubtreeBeingMovedAndNotOnlyTheFolder(t *testing.T) {
	// A straight line with exactly one level of headroom.
	names := make([]string, MaxFolderDepth-1)
	for i := range names {
		names[i] = "level" + string(rune('a'+i))
	}
	folders := make([]Folder, 0, len(names)+3)
	for i, name := range names {
		f := Folder{ID: fid(byte(i + 1)), Name: name}
		if i > 0 {
			parent := fid(byte(i))
			f.ParentID = &parent
		}
		folders = append(folders, f)
	}
	// A two-level branch beside it, and a leaf beside that.
	branch, branchChild, leaf := fid(20), fid(21), fid(22)
	folders = append(folders,
		Folder{ID: branch, Name: "branch"},
		Folder{ID: branchChild, Name: "branch-child", ParentID: &branch},
		Folder{ID: leaf, Name: "leaf"},
	)
	tree := NewFolderTree(folders)

	deepest := fid(byte(MaxFolderDepth - 1))
	if got := tree.Depth(&deepest); got != MaxFolderDepth-1 {
		t.Fatalf("the fixture's deepest folder is at depth %d, want %d",
			got, MaxFolderDepth-1)
	}

	// The leaf fits: one level of headroom, one level of folder.
	if r := tree.MoveRefusal(leaf, &deepest); r != nil {
		t.Fatalf("a single folder moved into the last level was refused: %v.\n"+
			"The cap has to permit what it says it permits, or it is just a "+
			"refusal with a number on it.", r)
	}
	// The branch does not: its child would land one past the cap.
	r := tree.MoveRefusal(branch, &deepest)
	if r == nil || r.Code != "too_deep" {
		t.Fatalf("MoveRefusal(branch into the last level) = %v, want too_deep.\n"+
			"The moved folder fits and its child does not, which is exactly the "+
			"case a check on the folder alone lets through.", r)
	}
}

func TestSiblingNamesCollideCaseInsensitivelyAndOnlyAmongSiblings(t *testing.T) {
	root, other := fid(1), fid(2)
	inRoot := fid(3)
	tree := NewFolderTree([]Folder{
		{ID: root, Name: "Campaigns"},
		{ID: other, Name: "Docs"},
		{ID: inRoot, Name: "docs", ParentID: &root},
	})

	if _, ok := tree.SiblingNamed(nil, "CAMPAIGNS"); !ok {
		t.Error(`"CAMPAIGNS" did not collide with "Campaigns" at the top level; ` +
			"a tree whose entries differ only in case is one whose reader has to " +
			"guess where they filed something")
	}
	if _, ok := tree.SiblingNamed(&root, "Docs"); !ok {
		t.Error(`"Docs" did not collide with "docs" inside Campaigns`)
	}
	// The same name in two different places is fine, and is the whole reason the
	// rule is about siblings rather than about the workspace.
	if _, ok := tree.SiblingNamed(&other, "docs"); ok {
		t.Error(`"docs" collided across different parents; folders in different ` +
			"places may share a name")
	}
}

func TestMovingBesideAFolderOfTheSameNameIsRefused(t *testing.T) {
	root, other := fid(1), fid(2)
	moving := fid(3)
	tree := NewFolderTree([]Folder{
		{ID: root, Name: "Campaigns"},
		{ID: other, Name: "Archive"},
		{ID: moving, Name: "Archive", ParentID: &root},
	})
	r := tree.MoveRefusal(moving, nil)
	if r == nil || r.Code != "conflict" {
		t.Fatalf("MoveRefusal = %v, want a conflict: the destination already holds "+
			"a folder called Archive, and the unique index would refuse the write "+
			"with a message nobody can act on", r)
	}
}

func TestTheTreeShowsFoldersACycleWouldOtherwiseHide(t *testing.T) {
	// A pair of folders each claiming the other as its parent. Nothing in the
	// product can create this; the assertion is that meeting it renders every
	// folder rather than losing them, because the links inside them are what
	// would look deleted.
	a, b := fid(1), fid(2)
	tree := NewFolderTree([]Folder{
		{ID: a, Name: "a", ParentID: &b},
		{ID: b, Name: "b", ParentID: &a},
	})
	if got := tree.Len(); got != 2 {
		t.Fatalf("the tree holds %d of 2 folders; a cycle in the data must not "+
			"make folders vanish from the page whose job is to show where things are",
			got)
	}
	// And the cycle rule still refuses to make it worse.
	if r := tree.MoveRefusal(a, &b); r == nil {
		t.Error("a move into a folder already inside a cycle was allowed")
	}
}

func TestOrphanedFoldersAreShownAtTheTopLevel(t *testing.T) {
	missing := fid(9)
	orphan := fid(1)
	tree := NewFolderTree([]Folder{{ID: orphan, Name: "orphan", ParentID: &missing}})
	if tree.Len() != 1 {
		t.Fatal("a folder whose parent is missing vanished from the tree")
	}
	if got := tree.Depth(&orphan); got != 1 {
		t.Errorf("orphan is at depth %d, want 1 — it has to be somewhere a person "+
			"can reach it and move it", got)
	}
}

func TestFolderNamesAreTrimmedAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantCode, wantOut string
	}{
		{name: "trimmed", in: "  Campaigns \n", wantOut: "Campaigns"},
		{name: "empty", in: "   ", wantCode: "required"},
		{name: "too long", in: strings.Repeat("x", MaxFolderNameLength+1), wantCode: "too_long"},
		{name: "control character", in: "Camp\taigns", wantCode: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, errs := ValidateFolderName(tc.in)
			switch {
			case tc.wantCode == "":
				if len(errs) > 0 {
					t.Fatalf("ValidateFolderName(%q) = %v, want no error", tc.in, errs)
				}
				if got != tc.wantOut {
					t.Fatalf("ValidateFolderName(%q) = %q, want %q", tc.in, got, tc.wantOut)
				}
			case len(errs) == 0:
				t.Fatalf("ValidateFolderName(%q) was accepted, want %s", tc.in, tc.wantCode)
			case errs[0].Code != tc.wantCode:
				t.Fatalf("ValidateFolderName(%q) code = %q, want %q", tc.in, errs[0].Code, tc.wantCode)
			}
		})
	}
}

func TestTheTreeOrdersParentsBeforeTheirDescendants(t *testing.T) {
	root, child, grandchild, sibling := fid(1), fid(2), fid(3), fid(4)
	tree := NewFolderTree([]Folder{
		{ID: root, Name: "a"},
		{ID: child, Name: "b", ParentID: &root},
		{ID: grandchild, Name: "c", ParentID: &child},
		{ID: sibling, Name: "d"},
	})
	var got []string
	for _, f := range tree.Flat() {
		got = append(got, f.Name)
	}
	want := "a b c d"
	if strings.Join(got, " ") != want {
		t.Fatalf("Flat() = %v, want %q — the API and the page both rely on a "+
			"folder being immediately followed by its descendants", got, want)
	}
	if d := tree.Depth(&grandchild); d != 3 {
		t.Errorf("Depth(grandchild) = %d, want 3", d)
	}
}
