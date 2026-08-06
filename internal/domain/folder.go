package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Folders (M38).
//
// The table has existed since Phase 1 with nothing able to create a row. What
// this file adds is the part of "a folder tree" that is neither storage nor
// HTTP: what a name may be, how deep the tree may go, and the two questions the
// move operation has to answer before it writes anything —
//
//   - would this move make a folder its own descendant?
//   - would this move push some folder past the depth cap?
//
// Both are walks over the same small set, so both live here rather than in SQL,
// and the reasoning is written out in internal/store/query/folders.sql.

// MaxFolderDepth is how many levels a folder tree may have. A folder with no
// parent is at depth 1.
//
// Eight, and the number comes from the two surfaces that have to render it. The
// tree page indents each level, so the deepest row starts a fixed distance in
// and must still leave room for a name and its controls; the move control is a
// `<select>` listing every folder in the workspace with its depth spelled out in
// the option label, and an option that is mostly indentation is one nobody can
// read. Neither breaks at eight and both are unusable well before twenty.
//
// It is a product limit rather than a technical one. Nothing here fails at
// depth 40 — but a link filed nine levels down is a link nobody finds again,
// and a cap somebody hits is better than a tree they get lost in.
const MaxFolderDepth = 8

// MaxFolderNameLength bounds a folder name, in runes. The same 64 a tag name
// gets, because they sit in the same lists and are read the same way.
const MaxFolderNameLength = 64

// MaxFoldersPerWorkspace bounds the whole tree.
//
// The tree page and every folder `<select>` load all of them, so this is the
// number that keeps those a single small query rather than something needing
// pagination — and a workspace wanting more than this is asking for tags, which
// it already has and which are not a hierarchy.
const MaxFoldersPerWorkspace = 500

// Folder is one folder as the product understands it.
//
// Depth and LinkCount are computed rather than stored: Depth by walking the
// tree this folder is in, LinkCount by counting the links filed directly in it.
// Neither is a column, so neither can drift.
type Folder struct {
	ID          uuid.UUID  `json:"id"`
	WorkspaceID uuid.UUID  `json:"workspace_id"`
	ParentID    *uuid.UUID `json:"parent_id,omitempty"`
	Name        string     `json:"name"`
	// Depth is 1 for a folder with no parent.
	Depth int `json:"depth"`
	// LinkCount is the links filed *directly* in this folder, never its
	// descendants' — it has to mean the same thing as the number of rows the
	// links list shows when this folder is the filter.
	LinkCount int64     `json:"link_count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FolderTree is a workspace's folders, assembled.
//
// Built from a flat list, in one pass, and it answers every structural question
// the service asks. Nothing in it queries anything, which is what makes the
// cycle rule testable without a database.
type FolderTree struct {
	// nodes is depth-first: a parent immediately precedes its descendants, and
	// siblings keep the order they arrived in.
	nodes    []Folder
	index    map[uuid.UUID]int
	children map[uuid.UUID][]uuid.UUID
}

// NewFolderTree assembles folders into a tree.
//
// The input needs only ID, ParentID, Name and LinkCount; Depth is filled in
// here. Siblings keep the caller's order, which ListFolders makes name order.
//
// **Anything unreachable from a root is appended at the end as a root.** A
// parent that is missing, or a cycle that somehow reached the table, would
// otherwise make those folders — and every link in them — vanish from a page
// whose whole job is to show where things are. Losing rows from a view is how a
// data problem becomes a support ticket about deleted links, so they are shown
// instead, at the top level, where they can be moved.
func NewFolderTree(folders []Folder) FolderTree {
	t := FolderTree{
		nodes:    make([]Folder, 0, len(folders)),
		index:    make(map[uuid.UUID]int, len(folders)),
		children: make(map[uuid.UUID][]uuid.UUID, len(folders)),
	}

	byID := make(map[uuid.UUID]Folder, len(folders))
	var roots []uuid.UUID
	for _, f := range folders {
		byID[f.ID] = f
	}
	for _, f := range folders {
		if f.ParentID == nil {
			roots = append(roots, f.ID)
			continue
		}
		if _, ok := byID[*f.ParentID]; !ok {
			// An orphan. Rendered as a root rather than dropped; see above.
			roots = append(roots, f.ID)
			continue
		}
		t.children[*f.ParentID] = append(t.children[*f.ParentID], f.ID)
	}

	placed := make(map[uuid.UUID]bool, len(folders))
	var walk func(id uuid.UUID, depth int)
	walk = func(id uuid.UUID, depth int) {
		if placed[id] {
			// The visited set. A cycle in the data would otherwise recurse until
			// the stack ran out, and a page that crashes tells nobody anything.
			return
		}
		placed[id] = true
		f := byID[id]
		f.Depth = depth
		t.index[id] = len(t.nodes)
		t.nodes = append(t.nodes, f)
		for _, child := range t.children[id] {
			walk(child, depth+1)
		}
	}
	for _, id := range roots {
		walk(id, 1)
	}
	// Whatever a cycle kept out of the walk, shown at the top level.
	for _, f := range folders {
		if !placed[f.ID] {
			walk(f.ID, 1)
		}
	}
	return t
}

// Flat returns the folders depth-first, parents before their descendants.
func (t FolderTree) Flat() []Folder {
	out := make([]Folder, len(t.nodes))
	copy(out, t.nodes)
	return out
}

// Len is how many folders the tree holds.
func (t FolderTree) Len() int { return len(t.nodes) }

// Get returns one folder.
func (t FolderTree) Get(id uuid.UUID) (Folder, bool) {
	i, ok := t.index[id]
	if !ok {
		return Folder{}, false
	}
	return t.nodes[i], true
}

// Depth is how deep a folder sits; 1 for a folder with no parent. A folder the
// tree does not hold — which is how "no parent" is spelled at the call sites —
// is depth 0, so that `Depth(parent) + 1` is the depth of a new child in both
// cases without the caller branching.
func (t FolderTree) Depth(id *uuid.UUID) int {
	if id == nil {
		return 0
	}
	f, ok := t.Get(*id)
	if !ok {
		return 0
	}
	return f.Depth
}

// Height is how many levels a folder's subtree occupies, counting itself. A
// leaf is 1.
func (t FolderTree) Height(id uuid.UUID) int {
	best := 0
	seen := map[uuid.UUID]bool{}
	var walk func(uuid.UUID, int)
	walk = func(cur uuid.UUID, depth int) {
		if seen[cur] {
			return
		}
		seen[cur] = true
		if depth > best {
			best = depth
		}
		for _, child := range t.children[cur] {
			walk(child, depth+1)
		}
	}
	walk(id, 1)
	return best
}

// IsAncestor reports whether `ancestor` is `id` itself or sits above it.
//
// This is the cycle rule, and it is stated as "is the proposed parent inside the
// subtree being moved" rather than as "does the subtree contain the parent"
// because that is the direction the walk is cheapest in: the chain upward from
// any folder is at most MaxFolderDepth long, whatever the tree's shape.
func (t FolderTree) IsAncestor(ancestor, id uuid.UUID) bool {
	seen := map[uuid.UUID]bool{}
	for cur := id; ; {
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			// Only reachable if the stored tree already holds a cycle. Answering
			// "no" here would let the move that closes a second one through.
			return true
		}
		seen[cur] = true
		f, ok := t.Get(cur)
		if !ok || f.ParentID == nil {
			return false
		}
		cur = *f.ParentID
	}
}

// SiblingNamed returns the folder with this name directly under `parent`, if
// there is one. Case-insensitive, matching migration 02400's unique index.
func (t FolderTree) SiblingNamed(parent *uuid.UUID, name string) (Folder, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, f := range t.nodes {
		if !sameParent(f.ParentID, parent) {
			continue
		}
		if strings.ToLower(f.Name) == want {
			return f, true
		}
	}
	return Folder{}, false
}

// MoveRefusal reports why `id` may not become a child of `parent`, or nil when
// the move is allowed. A nil parent is the top level.
//
// **One definition, two callers, and that is the reason it is here rather than
// in the service.** internal/link returns whatever this produces as the move's
// validation error; the dashboard's tree calls it to decide which rows to offer
// a "Move here" button on. Written twice, the page would eventually offer a
// destination the service refuses — a button that fails is worse than no button,
// and the drift would be invisible until somebody clicked it.
//
// Moving a folder to where it already is is not a refusal. It is a no-op the
// service performs happily; the page declines to offer it separately, because a
// button that changes nothing is noise rather than an error.
func (t FolderTree) MoveRefusal(id uuid.UUID, parent *uuid.UUID) *FieldError {
	moving, ok := t.Get(id)
	if !ok {
		return &FieldError{
			Field: "id", Code: "not_found", Message: "no folder with that id in this workspace",
		}
	}
	if parent != nil {
		if _, ok := t.Get(*parent); !ok {
			return &FieldError{
				Field: "parent_id", Code: "not_found",
				Message: "no folder with that id in this workspace",
			}
		}
		// The cycle rule. True both when the two are the same folder and when the
		// destination sits somewhere beneath it, and the message says both
		// because somebody who has just picked a folder off a list does not think
		// of those as one case.
		if t.IsAncestor(id, *parent) {
			return &FieldError{
				Field: "parent_id", Code: "cycle",
				Message: "a folder cannot be moved into itself or into one of the " +
					"folders inside it; the folders in the moved branch would stop " +
					"being reachable from the top level at all",
			}
		}
	}
	// The depth check runs on the *subtree*, not on the folder. Moving a
	// three-level branch under a folder at depth 6 puts its leaves at 9 —
	// checking only the folder being moved would let that through and leave a
	// tree deeper than the create path allows anybody to build.
	if t.Depth(parent)+t.Height(id) > MaxFolderDepth {
		return &FieldError{
			Field: "parent_id", Code: "too_deep",
			Message: fmt.Sprintf("folders may be nested %d levels deep, and moving "+
				"this branch there would put part of it below that", MaxFolderDepth),
		}
	}
	if existing, taken := t.SiblingNamed(parent, moving.Name); taken && existing.ID != id {
		return &FieldError{
			Field: "parent_id", Code: "conflict",
			Message: fmt.Sprintf("there is already a folder called %q there; "+
				"rename one of them first", existing.Name),
		}
	}
	return nil
}

// SameParent reports whether two parent references point at the same place, nil
// meaning the top level.
func SameParent(a, b *uuid.UUID) bool { return sameParent(a, b) }

func sameParent(a, b *uuid.UUID) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// ValidateFolderName checks a name on its own, before anything is looked up.
func ValidateFolderName(name string) (string, ValidationErrors) {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return "", ValidationErrors{{
			Field: "name", Code: "required", Message: "a folder needs a name",
		}}
	case utf8.RuneCountInString(trimmed) > MaxFolderNameLength:
		return "", ValidationErrors{{
			Field: "name", Code: "too_long",
			Message: fmt.Sprintf("a folder name may be at most %d characters",
				MaxFolderNameLength),
		}}
	case strings.ContainsFunc(trimmed, isControl):
		// A name carrying a newline or a tab renders as a broken row in the tree
		// and as a broken option in every folder select. Refused at the door
		// rather than escaped at six render sites.
		return "", ValidationErrors{{
			Field: "name", Code: "invalid",
			Message: "a folder name may not contain control characters",
		}}
	}
	return trimmed, nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// SortFoldersByName orders folders for assembly. Exported for the service,
// which reads them from one query and hands them straight to NewFolderTree.
func SortFoldersByName(folders []Folder) {
	sort.SliceStable(folders, func(i, j int) bool {
		a, b := strings.ToLower(folders[i].Name), strings.ToLower(folders[j].Name)
		if a != b {
			return a < b
		}
		return folders[i].ID.String() < folders[j].ID.String()
	})
}
