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
-- Scoped by organization, never by workspace: an audit log that can be narrowed
-- to the workspace the reader happens to be in would hide exactly the actions
-- worth reviewing.
SELECT id, occurred_at, organization_id, workspace_id,
       actor_user_id, actor_label, actor_api_key_id,
       action, target_type, target_id, metadata, ip_prefix
  FROM audit_logs
 WHERE organization_id = @organization_id
   AND (
        sqlc.narg('cursor_occurred')::timestamptz IS NULL
     OR (occurred_at, id) < (sqlc.narg('cursor_occurred')::timestamptz, sqlc.narg('cursor_id')::uuid)
   )
 ORDER BY occurred_at DESC, id DESC
 LIMIT @page_limit;
