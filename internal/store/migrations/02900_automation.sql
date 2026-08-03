-- +goose Up
--
-- Automation rules wake up (M43).
--
-- `automation_rules` has shipped since 00600 with nothing reading or writing it,
-- and this migration adds **no column to it at all**. m43.md asks that the
-- structure stay in the existing jsonb columns, and it does: the trigger is a
-- text name from a closed vocabulary, the threshold lives in `trigger_config`,
-- the ordered action list lives in `actions`, and `last_fired_at` is already
-- there. What is added here is what the *scheduler* needs to find work in
-- bounded time, plus the two permissions that guard the rules.

-- The due-rule index.
--
-- One evaluation run asks a single question of this table: which enabled rules
-- have the oldest watermarks. `automation_rules_workspace_idx` (00600) is on
-- (workspace_id, trigger) and answers the *page's* question — which rules does
-- this workspace have — so the scheduler cannot use it and would sort every
-- enabled rule on the instance on every tick.
--
-- Oldest watermark first is also the fairness property. A run is capped at
-- AutomationRulesPerRun rules, and taking them in watermark order means the
-- rules a capped run skipped are the ones with the oldest watermarks next time.
-- A cap without an order would starve whichever workspace sorted last.
CREATE INDEX automation_rules_due_idx
    ON automation_rules (last_fired_at NULLS FIRST) WHERE enabled;

-- Links that expired in a window, per workspace.
--
-- `links_expiry_idx` (00300) exists and **cannot be used for this**, for a reason
-- worth writing down rather than rediscovering: its predicate includes
-- `status = 'active'`. The link-expired trigger deliberately does not filter on
-- status, because `archive_link` is one of the actions a rule may take and a
-- trigger whose match set shrank when an action ran would be a rule that feeds
-- off its own effect. domain/automation.go declares that `links.expires_at` and
-- `links.status` are separate sources and that no action writes a source any
-- trigger reads; this index is where that declaration is kept honest in SQL.
--
-- Soft-deleted links are excluded, and that is not the same kind of exclusion:
-- nothing an automation does sets `deleted_at`, so the trigger's match set does
-- not move when a rule fires. A link in the trash is on its way out and firing
-- about it is noise.
CREATE INDEX automation_links_expiry_idx
    ON links (workspace_id, expires_at)
    WHERE expires_at IS NOT NULL AND deleted_at IS NULL;

-- Administrative events in a workspace, by action, in time order.
--
-- The blocked-attempt trigger reads `audit_logs` because that is the only
-- durable trace a refusal leaves — `blocked_destinations` (01500) is the
-- operator's blocklist, not a record of attempts against it. Neither of 00600's
-- indexes is workspace-scoped: one is by organization, one by actor, and both
-- are DESC on a log this reads forward from a watermark.
--
-- Keyed on action rather than partial on `destination.blocked`, so a later
-- trigger that reads a different action gets an index instead of a migration.
-- Created on the partitioned parent, so every partition the scheduler creates
-- afterwards inherits it.
CREATE INDEX audit_logs_workspace_action_idx
    ON audit_logs (workspace_id, action, occurred_at);

-- The two permissions that guard the rules.
--
-- Their own, rather than reusing `links.*`, and this is the same fork D75 and
-- D80 have already been at. A QR code is a property of a link, so whoever may
-- edit the link may edit it. An automation rule is not a property of anything:
-- it is a standing instruction that runs on the scheduler, unattended, and can
-- archive links and make this server connect to an address the workspace chose.
--
-- Owner and admin only, matching webhooks.*. An editor can archive a link
-- themselves; what they cannot do is leave behind an instruction that keeps
-- archiving links after they have stopped looking.
INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-000000000215', 'automation.read',
     'See a workspace''s automation rules and when each last fired'),
    ('00000000-0000-4000-8000-000000000216', 'automation.write',
     'Create, edit and remove automation rules: standing instructions the scheduler runs unattended');

-- Granted explicitly, for the reason 00800, 00900 and 02800 spell their grants
-- out: the seed migration's "owner gets everything" ran once, at its own
-- version, against the permissions that existed then. A permission added later
-- is held by nobody unless it says so here.
-- +goose StatementBegin
DO $$
DECLARE
    read_id  uuid := '00000000-0000-4000-8000-000000000215';
    write_id uuid := '00000000-0000-4000-8000-000000000216';
    owner_id uuid := '00000000-0000-4000-8000-000000000101';
    admin_id uuid := '00000000-0000-4000-8000-000000000102';
BEGIN
    INSERT INTO role_permissions (role_id, permission_id)
    VALUES (owner_id, read_id), (admin_id, read_id),
           (owner_id, write_id), (admin_id, write_id)
    ON CONFLICT DO NOTHING;
END
$$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM role_permissions WHERE permission_id IN (
    '00000000-0000-4000-8000-000000000215',
    '00000000-0000-4000-8000-000000000216');
DELETE FROM permissions WHERE slug IN ('automation.read', 'automation.write');
DROP INDEX IF EXISTS audit_logs_workspace_action_idx;
DROP INDEX IF EXISTS automation_links_expiry_idx;
DROP INDEX IF EXISTS automation_rules_due_idx;
