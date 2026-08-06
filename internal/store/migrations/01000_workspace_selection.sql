-- +goose Up
--
-- Where "which workspace am I in?" is remembered.
--
-- Three pieces of state, deliberately kept apart rather than folded into one
-- column, because they answer three different questions:
--
--   sessions.workspace_id       where this browser is right now
--   users.default_workspace_id  where a new session starts, when it is pinned
--   users.last_workspace_id     where a new session starts otherwise
--
-- Collapsing "current" into the user row would make the pin unusable: a pinned
-- user who switched would have their switch overruled on the next request, or
-- their pin quietly overwritten by the switch. Keeping the current workspace on
-- the session also means two browsers signed in as one person can sit in
-- different workspaces, which is what a person with two of them actually does.
--
-- Every column is nullable and every one is NULL after this migration runs, so
-- resolution falls through to the ordering it used before — the oldest workspace
-- the user is a member of. That is what makes this a no-op for the
-- single-membership instances that exist today.

ALTER TABLE users
    ADD COLUMN default_workspace_id uuid REFERENCES workspaces(id) ON DELETE SET NULL,
    ADD COLUMN last_workspace_id    uuid REFERENCES workspaces(id) ON DELETE SET NULL;

-- ON DELETE SET NULL rather than CASCADE on both: a workspace going away must
-- not take the user with it. Resolution re-checks membership anyway, so a
-- dangling preference degrades to "no preference" rather than to an error.
ALTER TABLE sessions
    ADD COLUMN workspace_id uuid REFERENCES workspaces(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE sessions DROP COLUMN IF EXISTS workspace_id;
ALTER TABLE users DROP COLUMN IF EXISTS last_workspace_id;
ALTER TABLE users DROP COLUMN IF EXISTS default_workspace_id;
