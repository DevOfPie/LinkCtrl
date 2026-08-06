package link

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/store/pgerr"
)

// Folders (M38).
//
// **No permission of their own.** A folder is where a link lives, in exactly the
// sense a routing rule is where a link points, and both are guarded by the
// permissions the link already has: reading the tree is `links.read`, creating a
// folder is `links.create`, renaming and moving one is `links.update`, deleting
// one is `links.delete`. That is the same call M34 made for rules and M36 made
// for split arms, and the reason is the same — the alternative is four more
// permission slugs whose grants would land on exactly the roles that hold these,
// producing a vocabulary two entries longer and a product no different. The
// decision, and what would have to change for a `folders.*` set to be worth
// minting, is recorded in decisions.md as D67.
//
// Four rules are enforced here and nowhere else:
//
//   - **A folder may never become its own descendant.** The move operation is
//     where that bites, and it is checked against the assembled tree before
//     anything is written. A cycle is not a strange state that renders oddly: it
//     is a set of folders that vanishes from every page, because a tree walk
//     from the roots never reaches them.
//   - **Siblings may not share a name.** Backed by a unique index (02400), which
//     is what makes it true when two requests interleave; the check here is what
//     makes the refusal a sentence rather than a constraint violation.
//   - **The tree may not go deeper than domain.MaxFolderDepth.** Checked on
//     create for the new folder, and on move for the whole subtree being moved —
//     a two-level subtree dropped under a folder at the cap would push its
//     leaves past it, which is the case a per-folder check misses. No index can
//     state "a chain of parents is at most this long", so what makes it true
//     when requests interleave is `LockWorkspaceFolders`: create and move both
//     read, decide and write inside the transaction holding every folder row
//     FOR UPDATE, and neither can decide against a tree the other is reshaping.
//   - **A workspace may hold at most domain.MaxFoldersPerWorkspace folders.**
//     Checked on create, the one operation that raises the count, and held
//     under interleaving by the same lock — no constraint counts rows, and two
//     creates arriving at one below the cap would otherwise each count the
//     tree before the other's insert and both pass.
//
// Nothing here invalidates a cached snapshot, and that is a fact about the
// redirect path rather than an oversight: `folder_id` is not in the snapshot,
// because nothing about where a link is filed changes where it sends anybody.

// CreateFolderInput is a new folder.
type CreateFolderInput struct {
	Name string
	// ParentID is the folder this one goes inside, or nil for the top level.
	ParentID *uuid.UUID
}

// Folders returns the workspace's folder tree.
func (s *Service) Folders(ctx context.Context, actor *auth.Identity) (domain.FolderTree, error) {
	if !actor.Can(PermRead) {
		return domain.FolderTree{}, domain.ErrForbidden
	}
	return s.loadFolderTree(ctx, actor.WorkspaceID)
}

// loadFolderTree reads the workspace's folders and assembles them, for callers
// that have already authorized.
func (s *Service) loadFolderTree(ctx context.Context, workspaceID uuid.UUID) (domain.FolderTree, error) {
	return s.loadFolderTreeWith(ctx, s.q, workspaceID)
}

// loadFolderTreeWith is loadFolderTree against a caller-supplied handle, so the
// tree a move decides on can be read inside the transaction holding its lock
// (F108).
func (s *Service) loadFolderTreeWith(
	ctx context.Context, q *dbgen.Queries, workspaceID uuid.UUID,
) (domain.FolderTree, error) {
	rows, err := q.ListFolders(ctx, workspaceID)
	if err != nil {
		return domain.FolderTree{}, fmt.Errorf("list folders: %w", err)
	}
	folders := make([]domain.Folder, 0, len(rows))
	for _, r := range rows {
		folders = append(folders, domain.Folder{
			ID: r.ID, WorkspaceID: r.WorkspaceID, ParentID: r.ParentID,
			Name: r.Name, LinkCount: r.LinkCount,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return domain.NewFolderTree(folders), nil
}

// CreateFolder adds a folder, optionally inside another.
func (s *Service) CreateFolder(
	ctx context.Context, actor *auth.Identity, in CreateFolderInput,
) (*domain.Folder, error) {
	if !actor.Can(PermCreate) {
		return nil, fmt.Errorf("%w: creating folders requires %s", domain.ErrForbidden, PermCreate)
	}

	name, errs := domain.ValidateFolderName(in.Name)

	// The read, the decision and the write in one transaction, over a locked
	// tree, for the reason MoveFolder gives (F108): the depth and count caps
	// are questions about the whole tree, and neither can be written as a
	// column check. Decided from a tree read on its own connection, a create
	// under a leaf and a move of that leaf deeper each pass a check the other
	// invalidates — create asks about the parent's depth, move about the
	// subtree's reach, so their composition can land a folder past the cap
	// while neither refusal fires. Blocking is not the missing piece: a nested
	// insert already waits on its parent's row behind a move's FOR UPDATE and
	// then proceeds on the decision it made before waiting, and a top-level
	// insert touches no parent row and does not wait at all. What repairs it
	// is re-reading inside the transaction that writes.
	//
	// An empty workspace gives LockWorkspaceFolders nothing to lock, and needs
	// nothing: the depth cap needs a parent, which is a lockable row, the
	// count cap needs hundreds of them, and the sibling name falls to the
	// unique index either way.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if _, err := q.LockWorkspaceFolders(ctx, actor.WorkspaceID); err != nil {
		return nil, fmt.Errorf("lock folders: %w", err)
	}

	tree, err := s.loadFolderTreeWith(ctx, q, actor.WorkspaceID)
	if err != nil {
		return nil, err
	}

	if in.ParentID != nil {
		if _, ok := tree.Get(*in.ParentID); !ok {
			// A 422 rather than a 404, because the thing being created is the
			// folder and the parent is a field on it. The workspace scoping is
			// the tree's: a folder in another workspace is not in it.
			errs = append(errs, domain.FieldError{
				Field: "parent_id", Code: "not_found",
				Message: "no folder with that id in this workspace",
			})
		}
	}
	if depth := tree.Depth(in.ParentID) + 1; depth > domain.MaxFolderDepth {
		errs = append(errs, folderTooDeep())
	}
	if tree.Len() >= domain.MaxFoldersPerWorkspace {
		errs = append(errs, domain.FieldError{
			Field: "name", Code: "too_many",
			Message: fmt.Sprintf("a workspace may have at most %d folders; past that "+
				"a tree is not how anybody is finding a link, and tags are",
				domain.MaxFoldersPerWorkspace),
		})
	}
	if name != "" {
		if existing, taken := tree.SiblingNamed(in.ParentID, name); taken {
			errs = append(errs, folderNameTaken(existing.Name))
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	row, err := q.CreateFolder(ctx, dbgen.CreateFolderParams{
		ID: uuid.Must(uuid.NewV7()), WorkspaceID: actor.WorkspaceID,
		ParentID: in.ParentID, Name: name,
	})
	if err != nil {
		// The index is the real guarantee for the name: two creates into an
		// empty spot of the tree hold no locks against each other, and one of
		// them lands here. The check above only makes this rare.
		if pgerr.IsUniqueViolation(err) {
			return nil, domain.ValidationErrors{folderNameTaken(name)}
		}
		return nil, fmt.Errorf("create folder: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return folderFromRow(row, tree.Depth(in.ParentID)+1, 0), nil
}

// RenameFolder changes a folder's name, leaving it where it is.
func (s *Service) RenameFolder(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, newName string,
) (*domain.Folder, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf("%w: renaming folders requires %s", domain.ErrForbidden, PermUpdate)
	}

	name, errs := domain.ValidateFolderName(newName)
	tree, err := s.loadFolderTree(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, err
	}
	current, ok := tree.Get(id)
	if !ok {
		return nil, domain.ErrNotFound
	}
	if name != "" {
		// A folder does not collide with itself, so renaming "Ads" to "ads" is a
		// change of case rather than a conflict.
		if existing, taken := tree.SiblingNamed(current.ParentID, name); taken && existing.ID != id {
			errs = append(errs, folderNameTaken(existing.Name))
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	row, err := s.q.RenameFolder(ctx, dbgen.RenameFolderParams{
		ID: id, WorkspaceID: actor.WorkspaceID, Name: name,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		if pgerr.IsUniqueViolation(err) {
			return nil, domain.ValidationErrors{folderNameTaken(name)}
		}
		return nil, fmt.Errorf("rename folder: %w", err)
	}
	return folderFromRow(row, current.Depth, current.LinkCount), nil
}

// MoveFolder re-parents a folder, subtree and all. A nil parent moves it to the
// top level.
//
// **This is where the cycle rule bites.** Everything else in this file is a name
// and a number; a move is the one operation that can make the tree stop being
// one, and the check is against the tree as it is now rather than against an
// assumption about how it got that way.
func (s *Service) MoveFolder(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, parentID *uuid.UUID,
) (*domain.Folder, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf("%w: moving folders requires %s", domain.ErrForbidden, PermUpdate)
	}

	// The read, the decision and the write in one transaction, over a locked
	// tree (F108). MoveRefusal answers "is the new parent inside the subtree
	// being moved", which is a question about the whole tree and cannot be
	// written as a column check — so decided against a tree read on its own
	// connection, two concurrent moves each pass a check the other invalidates.
	// A under B while B moves under A satisfies both and produces the cycle M38
	// says can never exist.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// The whole workspace, in a fixed id order. The whole workspace because a
	// cycle can run through folders neither move names; the fixed order because
	// two transactions taking the same locks in different orders deadlock where
	// a shared order makes them queue.
	if _, err := q.LockWorkspaceFolders(ctx, actor.WorkspaceID); err != nil {
		return nil, fmt.Errorf("lock folders: %w", err)
	}

	tree, err := s.loadFolderTreeWith(ctx, q, actor.WorkspaceID)
	if err != nil {
		return nil, err
	}
	current, ok := tree.Get(id)
	if !ok {
		return nil, domain.ErrNotFound
	}

	// Every refusal in one call, so the dashboard's tree can ask the same
	// question before it draws a button and the two answers cannot drift.
	if refusal := tree.MoveRefusal(id, parentID); refusal != nil {
		return nil, domain.ValidationErrors{*refusal}
	}

	row, err := q.MoveFolder(ctx, dbgen.MoveFolderParams{
		ID: id, WorkspaceID: actor.WorkspaceID, ParentID: parentID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		if pgerr.IsUniqueViolation(err) {
			return nil, domain.ValidationErrors{folderNameTaken(current.Name)}
		}
		return nil, fmt.Errorf("move folder: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return folderFromRow(row, tree.Depth(parentID)+1, current.LinkCount), nil
}

// DeleteFolder removes a folder and the folders inside it.
//
// **The links survive.** `links.folder_id` has been `ON DELETE SET NULL` since
// 00300 and `folders.parent_id` has been `ON DELETE CASCADE` for as long, so one
// DELETE removes the branch and unfiles every link anywhere in it. Nothing in
// this method touches `links`, and nothing in it may: the moment deleting a
// container starts deleting content, somebody loses a campaign's worth of links
// by tidying up a tree. test/integration/folders_test.go is where that is
// asserted rather than assumed.
func (s *Service) DeleteFolder(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	if !actor.Can(PermDelete) {
		return fmt.Errorf("%w: deleting folders requires %s", domain.ErrForbidden, PermDelete)
	}
	n, err := s.q.DeleteFolder(ctx, dbgen.DeleteFolderParams{ID: id, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		return fmt.Errorf("delete folder: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// resolveFolder checks that a folder id names a folder of this workspace, for
// the link create and update paths.
//
// The foreign key on `links.folder_id` points at `folders(id)` and says nothing
// about tenancy, so without this a caller could file their link into another
// workspace's folder — and the folder's own link count would then report a link
// its readers cannot see.
func (s *Service) resolveFolder(
	ctx context.Context, workspaceID uuid.UUID, folderID *uuid.UUID,
) domain.ValidationErrors {
	if folderID == nil {
		return nil
	}
	if _, err := s.q.GetFolder(ctx, dbgen.GetFolderParams{
		ID: *folderID, WorkspaceID: workspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ValidationErrors{{
				Field: "folder_id", Code: "not_found",
				Message: "no folder with that id in this workspace",
			}}
		}
		return domain.ValidationErrors{{
			Field: "folder_id", Code: "unavailable",
			Message: "the folder could not be read: " + err.Error(),
		}}
	}
	return nil
}

func folderFromRow(row dbgen.Folder, depth int, linkCount int64) *domain.Folder {
	return &domain.Folder{
		ID: row.ID, WorkspaceID: row.WorkspaceID, ParentID: row.ParentID,
		Name: row.Name, Depth: depth, LinkCount: linkCount,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func folderNameTaken(name string) domain.FieldError {
	return domain.FieldError{
		Field: "name", Code: "conflict",
		Message: fmt.Sprintf("there is already a folder called %q in the same place; "+
			"two folders side by side with one name is a tree nobody can file into",
			name),
	}
}

func folderTooDeep() domain.FieldError {
	return domain.FieldError{
		Field: "parent_id", Code: "too_deep",
		Message: fmt.Sprintf("folders may be nested %d levels deep; a link filed "+
			"below that is a link nobody finds again", domain.MaxFolderDepth),
	}
}
