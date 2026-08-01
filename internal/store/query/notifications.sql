-- Notifications. The table shipped dormant in 00600; nothing here adds a
-- column, per the rule that a dormant table's structure goes in its jsonb until
-- the feature that needs a column arrives. `data` carries whatever a kind needs.

-- name: InsertNotification :exec
INSERT INTO notifications (id, user_id, workspace_id, kind, title, body, data)
VALUES (@id, @user_id, @workspace_id, @kind, @title, @body, @data);

-- name: ListNotifications :many
--
-- Newest first, keyset on (created_at, id). Same shape as the audit log and the
-- link list: an offset shifts under a notification arriving mid-page, and a new
-- notification arriving is the normal case here rather than the rare one.
SELECT id, user_id, workspace_id, kind, title, body, data, read_at, created_at
  FROM notifications
 WHERE user_id = @user_id
   AND (NOT @unread_only::boolean OR read_at IS NULL)
   AND (
        sqlc.narg('cursor_created')::timestamptz IS NULL
     OR (created_at, id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid)
   )
 ORDER BY created_at DESC, id DESC
 LIMIT @page_limit;

-- name: CountUnreadNotifications :one
--
-- Served by notifications_user_unread_idx, the partial index the table already
-- ships with: the WHERE clause here has to match the index's predicate exactly
-- or this becomes a sequential scan on every page render in the dashboard.
SELECT count(*) FROM notifications
 WHERE user_id = @user_id AND read_at IS NULL;

-- name: ListUnreadNotificationPreview :many
--
-- The whole of the header's notification lookup: the newest unread rows the bell
-- previews, and the unread total the badge shows, in one round trip.
--
-- Two shapes already in this file, composed rather than a third one. The
-- predicate is CountUnreadNotifications' predicate character for character, so
-- notifications_user_unread_idx still serves it; the ordering is
-- ListNotifications' ordering, so the preview is the same "newest first" the
-- page shows.
--
-- `count(*) OVER ()` is what makes it one query instead of two. Window functions
-- are evaluated before LIMIT, so the count is every unread row rather than the
-- handful returned — which is the only reason the badge can keep being exact
-- while the preview stays bounded. A page render costs one notification query
-- here, as it did when it cost a bare count.
SELECT id, user_id, workspace_id, kind, title, body, data, read_at, created_at,
       count(*) OVER () AS unread_total
  FROM notifications
 WHERE user_id = @user_id AND read_at IS NULL
 ORDER BY created_at DESC, id DESC
 LIMIT @page_limit;

-- name: MarkNotificationRead :execrows
--
-- Scoped by user_id as well as id, so someone else's notification is a
-- zero-row update rather than a 403 that confirms the id exists.
--
-- read_at is only set once. Marking an already-read notification is a no-op
-- rather than a fresh timestamp, so "when did you first see this" survives a
-- double click.
UPDATE notifications
   SET read_at = now()
 WHERE id = @id AND user_id = @user_id AND read_at IS NULL;

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications
   SET read_at = now()
 WHERE user_id = @user_id AND read_at IS NULL;

-- name: ListUsersWithRoleInOrg :many
--
-- Who to tell about something that concerns the organization rather than a
-- person. Active users only: a deactivated account cannot sign in to read it.
--
-- The address comes back with the id because both deliveries address the same
-- person: the inbox row is keyed by user, the mail by address, and looking the
-- second one up separately would mean a query per recipient.
SELECT u.id, u.email, u.name
  FROM memberships m
  JOIN roles r ON r.id = m.role_id
  JOIN users u ON u.id = m.user_id
 WHERE m.organization_id = @organization_id
   AND r.slug = @role_slug
   AND u.status = 'active'
 ORDER BY u.id;

-- name: CountRecentNotificationsOfKind :one
--
-- The re-notify guard. A threshold that is still crossed is still crossed on
-- the next run an hour later, and a notification per hour forever is how an
-- inbox becomes something people stop reading — which would cost exactly the
-- warning D5 depends on.
SELECT count(*) FROM notifications
 WHERE user_id = @user_id
   AND kind = @kind
   AND created_at > @since;

-- name: ListOrganizationIDs :many
--
-- Every organization on the instance. Phase 1 has exactly one; this is written
-- as a list because M28 makes that untrue and a job that notified only the
-- first organization would then be silently wrong for every other.
SELECT id FROM organizations WHERE deleted_at IS NULL ORDER BY id;
