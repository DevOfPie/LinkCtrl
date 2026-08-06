-- Folders (M38).
--
-- **There is no recursive SQL here, and that is a decision rather than an
-- omission.** A folder tree is naturally a WITH RECURSIVE walk, and the walk was
-- written first; it was replaced by ListFolders, which reads the workspace's
-- folders flat and lets internal/link assemble the tree in Go. Three reasons,
-- in the order they matter:
--
--   - The interesting rules are not "who are my children". They are "may this
--     move happen" — a folder may never become its own descendant — and "how
--     deep would the result be". Both are walks over the same set, and running
--     them as three more recursive CTEs would put the milestone's two named
--     failure modes in three places where only integration tests can reach them.
--     In Go they are one function with a unit test that does not need Postgres.
--   - The set is small by construction. The depth cap is
--     domain.MaxFolderDepth, the sibling-name rule stops a tree fanning out by
--     accident, and this is a structure a person curates by hand.
--   - A recursive CTE over a table with no cycle constraint runs forever if the
--     data ever holds one. The Go walk carries a visited set and stops.
--
-- Deleting a folder is a real DELETE (see migration 02400): `parent_id ON DELETE
-- CASCADE` takes the subtree and `links.folder_id ON DELETE SET NULL` unfiles
-- every link in any part of it. Every statement below still filters on
-- `deleted_at IS NULL`, so the partial indexes serve them and a later decision
-- to soft-delete does not silently resurrect rows.

-- name: CreateFolder :one
INSERT INTO folders (id, workspace_id, parent_id, name)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetFolder :one
-- Workspace-scoped like GetLink, and for the same reason: the wrong workspace
-- returns no rows rather than a row the caller must remember to reject. This is
-- also the only check that a link is being filed into a folder of its own
-- workspace — `links.folder_id` has a foreign key to `folders(id)` and nothing
-- in it mentions tenancy.
SELECT * FROM folders
WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL;

-- name: ListFolders :many
-- Every folder in the workspace, with how many links are filed directly in it.
--
-- Flat and unordered by structure: the caller builds the tree. Sorted by name so
-- that the assembled tree's sibling order is stable, and by id after it so two
-- names that fold to the same string under a collation cannot swap between page
-- loads.
--
-- The count is a grouped scan of links_folder_idx rather than a correlated
-- subquery per folder, so a workspace with many folders costs one pass instead
-- of one index probe each. It counts links filed *directly* here — a parent does
-- not report its children's links, because the number beside a folder has to
-- mean the same thing as the number of rows the list shows when you click it.
SELECT f.*, COALESCE(c.n, 0)::bigint AS link_count
FROM folders f
LEFT JOIN (
    SELECT l.folder_id, count(*) AS n
      FROM links l
     WHERE l.workspace_id = sqlc.arg(workspace_id)
       AND l.deleted_at IS NULL
       AND l.folder_id IS NOT NULL
     GROUP BY l.folder_id
) c ON c.folder_id = f.id
WHERE f.workspace_id = sqlc.arg(workspace_id) AND f.deleted_at IS NULL
ORDER BY lower(f.name), f.id;

-- name: CountFolders :one
SELECT count(*) FROM folders
WHERE workspace_id = $1 AND deleted_at IS NULL;

-- name: RenameFolder :one
UPDATE folders
   SET name = $3, updated_at = now()
 WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: MoveFolder :one
-- The parent is set outright rather than COALESCEd, because NULL is a
-- destination here: it means the root. A partial-update idiom would make
-- "move to the top level" unexpressible.
UPDATE folders
   SET parent_id = sqlc.narg(parent_id), updated_at = now()
 WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id) AND deleted_at IS NULL
RETURNING *;

-- name: DeleteFolder :execrows
-- A real DELETE. The two foreign keys 00300 wrote are what make it safe:
-- descendants cascade, and every link in the subtree has its folder_id set to
-- NULL. Nothing here touches `links`, and nothing here may: the moment this
-- statement grows a link delete, deleting a container starts deleting content.
DELETE FROM folders WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL;

-- name: LockWorkspaceFolders :many
--
-- Every folder in a workspace, locked, for a decision made over the whole tree.
--
-- `MoveFolder`'s refusals are computed in Go from a tree read a moment earlier —
-- *is the new parent inside the subtree being moved* cannot be written as a
-- column check — so the read and the write have to be one transaction or two
-- concurrent moves each decide against a tree the other is changing. Moving A
-- under B while B moves under A passes both checks and produces the cycle M38
-- says can never exist (F108).
--
-- The whole workspace rather than the two rows involved, because the predicate
-- is over the whole tree: a cycle can run through folders neither move names.
-- Workspaces are small enough for that to be one indexed read.
--
-- `ORDER BY id` is load-bearing rather than cosmetic, and it is
-- `LockOrganizations`' reasoning: two transactions taking the same set of row
-- locks in different orders deadlock, and a fixed order makes it a wait instead.
--
-- Returns ids alone. The caller reads the tree it decides on through
-- ListFolders inside the same transaction; this exists for its lock.
SELECT id FROM folders
WHERE workspace_id = $1 AND deleted_at IS NULL
ORDER BY id
FOR UPDATE;
