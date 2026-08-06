-- Audit log. Append-only: there is no update and no delete here, and that is the
-- point of the table. Rows leave only when retention drops a whole partition.

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (
    id, occurred_at, organization_id, workspace_id,
    actor_user_id, actor_label, actor_api_key_id,
    action, target_type, target_id, metadata, ip_prefix
) VALUES (
    @id, @occurred_at, @organization_id, @workspace_id,
    @actor_user_id, @actor_label, @actor_api_key_id,
    @action, @target_type, @target_id, @metadata, @ip_prefix
);

-- name: ListAuditLogs :many
--
-- Newest first, keyed on (occurred_at, id) so the cursor is a position rather
-- than an offset: an event written while a reader is paginating shifts every
-- offset by one, and a keyset cursor is unaffected by it.
--
-- The row-comparison predicate is what makes that hold. Comparing the columns
-- separately -- occurred_at < c OR (occurred_at = c AND id < i) -- is the same
-- logic, and the planner does not always recognise it as a range scan on the
-- (organization_id, occurred_at DESC) index.
--
-- Scoped by organization, never by the workspace the reader happens to be
-- *acting in*: an audit log that narrowed itself to the current workspace would
-- hide exactly the actions worth reviewing. That is M21's argument and it still
-- holds; what it never said is that the reader's own authority does not bound
-- the rows either.
--
-- It does now (F31). `org_wide` is true when the reader holds audit.read from an
-- organization-wide membership, which is the only membership that reaches the
-- organization-wide scope (auth.MembershipAuthority, D44) — such a reader sees
-- every row, exactly as before. A reader whose audit.read comes only from
-- workspace-scoped memberships sees the rows of those workspaces and nothing
-- else, because a workspace-scoped membership grants authority over its own
-- workspace and not over the organization.
--
-- Rows with a NULL workspace_id are organization-level acts, and `= ANY` is
-- false against NULL, so a workspace-scoped reader does not see them. That is
-- the same asymmetry MembershipAuthority.In(nil) enforces for writes, arriving
-- here for reads.
SELECT id, occurred_at, organization_id, workspace_id,
       actor_user_id, actor_label, actor_api_key_id,
       action, target_type, target_id, metadata, ip_prefix
  FROM audit_logs
 WHERE organization_id = @organization_id
   AND (sqlc.arg(org_wide)::bool OR workspace_id = ANY(sqlc.arg(workspace_ids)::uuid[]))
   AND (
        sqlc.narg('cursor_occurred')::timestamptz IS NULL
     OR (occurred_at, id) < (sqlc.narg('cursor_occurred')::timestamptz, sqlc.narg('cursor_id')::uuid)
   )
 ORDER BY occurred_at DESC, id DESC
 LIMIT @page_limit;

-- name: ListInstanceAuditLogs :many
--
-- The instance-wide audit surface (F36, D98). Rows with no organization at all:
-- an act that changed every tenant and belongs to none of them.
--
-- A separate statement rather than a predicate bolted onto the one above, for
-- two reasons that point the same way. The query above rides
-- audit_logs_org_time_idx as a range scan; an OR reaching NULL organizations
-- would turn it into a bitmap scan and a sort on a table designed to grow
-- forever. And the surface is genuinely separate: it is read by the instance
-- principal under audit.read.instance, not by whoever happens to hold audit.read
-- in some organization, so merging the two would mean deciding per row which
-- permission had authorized it.
--
-- Same keyset shape, so a client that paginates the organization log paginates
-- this one.
SELECT id, occurred_at, organization_id, workspace_id,
       actor_user_id, actor_label, actor_api_key_id,
       action, target_type, target_id, metadata, ip_prefix
  FROM audit_logs
 WHERE organization_id IS NULL
   AND (
        sqlc.narg('cursor_occurred')::timestamptz IS NULL
     OR (occurred_at, id) < (sqlc.narg('cursor_occurred')::timestamptz, sqlc.narg('cursor_id')::uuid)
   )
 ORDER BY occurred_at DESC, id DESC
 LIMIT @page_limit;
