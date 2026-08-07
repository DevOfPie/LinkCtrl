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
   AND (workspace_id IS NULL OR workspace_id = @workspace_id)
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
--
-- **Both halves of the workspace predicate are load-bearing** (D102, F105).
-- `workspace_id = @workspace_id` alone hides every organization-level
-- notification, because disputes and audit-growth write NULL — the reader would
-- lose exactly the notifications that are not about any one workspace. And the
-- clause has to be identical here and in ListUnreadNotificationPreview below, or
-- the badge and the list it previews disagree while one of them stops using the
-- index.
SELECT count(*) FROM notifications
 WHERE user_id = @user_id AND read_at IS NULL
   AND (workspace_id IS NULL OR workspace_id = @workspace_id);

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
   AND (workspace_id IS NULL OR workspace_id = @workspace_id)
 ORDER BY created_at DESC, id DESC
 LIMIT @page_limit;

-- name: GetNotification :one
--
-- One row of the actor's own inbox, scoped by user_id like every statement
-- around it: somebody else's notification is "no rows" rather than a 403 that
-- confirms the id exists.
--
-- Read by the click-through (M48). Where a notification leads is computed from
-- its `kind` and its `data`, and both have to come off the row — a destination
-- carried on the request would be a redirect target the caller chose.
SELECT id, user_id, workspace_id, kind, title, body, data, read_at, created_at
  FROM notifications
 WHERE id = @id AND user_id = @user_id;

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

-- name: MarkNotificationUnread :execrows
--
-- `read_at` back to NULL, which is the whole of "unread": 00600 declared the
-- column nullable and the inbox has always used NULL for it, so putting one
-- back is an UPDATE and never a migration (M48).
--
-- **Deliberately not the mirror image of MarkNotificationRead.** That statement
-- refuses to touch an already-read row so that "when did you first see this"
-- survives a double click. This one carries no such guard: it exists because the
-- click-through M48 adds marks a notification read as a side effect of opening
-- it, and somebody undoing that is saying they have not dealt with it — which is
-- as true of a row read last week as of one read by accident a second ago. The
-- first-seen timestamp is what is being discarded, on purpose, by the person it
-- belongs to.
UPDATE notifications
   SET read_at = NULL
 WHERE id = @id AND user_id = @user_id AND read_at IS NOT NULL;

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications
   SET read_at = now()
 WHERE user_id = @user_id AND read_at IS NULL;

-- name: ListUsersWithRoleInOrg :many
--
-- Who to tell about something that concerns the organization rather than a
-- person. Active users only: a deactivated account cannot sign in to read it.
--
-- **Scoped, because a membership is** (D44). The sentence LockOrganizationOwners
-- states in members.sql applies here word for word: a workspace-scoped owner
-- membership grants ownership of one workspace, not of the organization. So the
-- recipients are the organization-wide rows, plus — when the news belongs to a
-- workspace — the rows scoped to *that* workspace, because news about their
-- workspace is theirs to hear.
--
-- Two arms rather than one predicate, and that is the whole correction. Adding
-- `m.workspace_id IS NULL` alone is the smaller diff and the wrong recipient
-- set: it silences a workspace-scoped owner about their own workspace, which is
-- exactly what a caller passing a workspace id means to tell them about. A NULL
-- @workspace_id is news that belongs to no workspace, and the second arm
-- matches nothing then, which is what the organization-wide callers want.
--
-- r.organization_id IS NULL for the reason LockOrganizationOwners carries it:
-- 'owner' names a built-in role, and a tenant's custom role of the same slug is
-- a different role.
--
-- DISTINCT because the arms overlap. One person may hold both an
-- organization-wide owner membership and an owner membership scoped to this
-- workspace — that pair is precisely how the defect was reachable — and a
-- recipient list naming them twice would write two inbox rows and send two
-- mails.
--
-- The address comes back with the id because both deliveries address the same
-- person: the inbox row is keyed by user, the mail by address, and looking the
-- second one up separately would mean a query per recipient.
SELECT DISTINCT u.id, u.email, u.name
  FROM memberships m
  JOIN roles r ON r.id = m.role_id
  JOIN users u ON u.id = m.user_id
 WHERE m.organization_id = @organization_id
   AND (m.workspace_id IS NULL OR m.workspace_id = sqlc.narg(workspace_id))
   AND r.slug = @role_slug
   AND r.organization_id IS NULL
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
