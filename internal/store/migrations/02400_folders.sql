-- +goose Up
--
-- Folders wake up (M38).
--
-- The table has existed since 00300 with a comment saying "table only in Phase
-- 1. No API, no UI." — a container nothing could create and nothing could put a
-- link into. This migration is the only DDL that turning it on needs, because
-- the two foreign keys that carry the behaviour were already written the way
-- this milestone requires:
--
--   folders.parent_id  REFERENCES folders(id) ON DELETE CASCADE
--   links.folder_id    REFERENCES folders(id) ON DELETE SET NULL
--
-- Read together they say what deleting a folder means: the subtree beneath it
-- goes, and every link in any part of that subtree becomes unfiled rather than
-- deleted. **Losing links because a container was deleted is the failure M38
-- names**, and the schema already refuses it — so the milestone's obligation is
-- a test that proves it, which test/integration/folders_test.go carries.
--
-- That is also why folder deletion is a real DELETE rather than the soft delete
-- `folders.deleted_at` invites. Neither FK clause fires on an UPDATE, so a soft
-- delete would leave every link in the folder pointing at a row the tree no
-- longer renders — the links would be intact in the table and invisible in the
-- product, which is the same outcome as losing them for anybody using it. A
-- folder holds no data of its own: there is nothing to restore but a name.
-- `deleted_at` therefore stays unwritten, and every folder query still filters
-- on it so that the partial indexes below and `folders_workspace_idx` above
-- remain usable if that ever changes. See decisions.md, D66.

-- Two folders in one place may not share a name.
--
-- Enforced in internal/link as well, and this index is not a duplicate of that
-- check: the service check is what produces the 422 a person can read, and this
-- is what makes the rule true under concurrency, exactly as
-- `links_domain_alias_key` backs the alias availability check in
-- link.Service.Create. Two requests naming the same new folder interleave
-- between the check and the insert; without the index one of them wins silently
-- and the tree grows two entries a reader cannot tell apart.
--
-- COALESCE, because the roots are the case a naive index misses. `parent_id IS
-- NULL` for every top-level folder, and NULL is never equal to NULL, so
-- (workspace_id, parent_id, lower(name)) would constrain every level of the
-- tree except the one everybody starts on. The sentinel is the nil UUID, which
-- no folder can hold: ids are uuid v7 and v7 has neither an all-zero timestamp
-- nor version 0.
--
-- lower(name), so "Campaigns" and "campaigns" are one name. A tree where two
-- entries differ only in case is a tree whose reader has to guess which one
-- they filed something in.
CREATE UNIQUE INDEX folders_sibling_name_key
    ON folders (workspace_id, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), lower(name))
    WHERE deleted_at IS NULL;

-- The tree walk reads children by parent, which nothing indexed.
--
-- folders_workspace_idx (00300) serves "every folder in this workspace" and the
-- recursive CTE's non-recursive term uses it. The recursive term joins children
-- to their parent once per level, and on that side workspace_id is not the
-- selective column — parent_id is.
CREATE INDEX folders_parent_idx ON folders (parent_id) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS folders_parent_idx;
DROP INDEX IF EXISTS folders_sibling_name_key;
