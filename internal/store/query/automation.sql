-- Automation rules and their evaluation (M43).
--
-- Two halves that never meet in one statement, the shape webhooks.sql already
-- has. The rule half is workspace-scoped and reached from the dashboard and the
-- API; the evaluation half is reached only by the scheduler and deliberately
-- carries no workspace parameter in its first query — a run that filtered by
-- tenant would evaluate in tenant order, and the fairness this needs is
-- oldest-watermark-first across the instance.
--
-- **`last_fired_at` is a watermark, not a diagnostic.** Every match query below
-- takes a half-open window `(@after, @until]`, and `@after` is that watermark.
-- That is what makes a rule unable to trigger itself: a subject is matched once,
-- the watermark moves past it, and no later run can see it again. Removing the
-- advance turns every rule into a runaway that fires on the same subject on
-- every tick.

-- name: CreateAutomationRule :one
--
-- `last_fired_at` is set at creation rather than left NULL. A NULL watermark on
-- a rule created today would mean "every link that ever expired", so the first
-- run of a brand-new rule would fire for the entire history of the workspace.
INSERT INTO automation_rules
    (id, workspace_id, name, trigger, trigger_config, actions, enabled, last_fired_at)
VALUES (@id, @workspace_id, @name, @trigger, @trigger_config, @actions, @enabled, @last_fired_at)
RETURNING id, workspace_id, name, trigger, trigger_config, actions, enabled,
          last_fired_at, created_at, updated_at;

-- name: ListAutomationRules :many
SELECT id, workspace_id, name, trigger, trigger_config, actions, enabled,
       last_fired_at, created_at, updated_at
  FROM automation_rules
 WHERE workspace_id = @workspace_id
 ORDER BY created_at, id;

-- name: GetAutomationRule :one
SELECT id, workspace_id, name, trigger, trigger_config, actions, enabled,
       last_fired_at, created_at, updated_at
  FROM automation_rules
 WHERE id = @id AND workspace_id = @workspace_id;

-- name: CountAutomationRules :one
SELECT count(*) FROM automation_rules WHERE workspace_id = @workspace_id;

-- name: UpdateAutomationRule :one
--
-- Partial: a NULL parameter leaves its column alone, the shape every other
-- update in this schema has.
--
-- **Re-arming is in this statement and not a second one.** A rule switched from
-- disabled to enabled has its watermark moved to the arming instant, so a rule
-- that was off for a month does not fire for a month of backlog the moment
-- somebody flips the switch. Switching one *off* leaves the watermark where it
-- is, because a disabled rule is not evaluated at all and the value is what a
-- reader is shown.
UPDATE automation_rules
   SET name           = coalesce(sqlc.narg(name), name),
       trigger        = coalesce(sqlc.narg(trigger), trigger),
       trigger_config = coalesce(sqlc.narg(trigger_config), trigger_config),
       actions        = coalesce(sqlc.narg(actions), actions),
       enabled        = coalesce(sqlc.narg(enabled), enabled),
       last_fired_at  = CASE
                          WHEN coalesce(sqlc.narg(enabled), enabled) AND NOT enabled
                          THEN sqlc.arg(rearmed_at)
                          ELSE last_fired_at
                        END,
       updated_at     = now()
 WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
RETURNING id, workspace_id, name, trigger, trigger_config, actions, enabled,
          last_fired_at, created_at, updated_at;

-- name: DeleteAutomationRule :execrows
DELETE FROM automation_rules WHERE id = @id AND workspace_id = @workspace_id;

-- --- evaluation --------------------------------------------------------------

-- name: ListDueAutomationRules :many
--
-- The rules one run considers, oldest watermark first.
--
-- Bounded by the caller at domain.AutomationRulesPerRun, and ordered so the cap
-- starves nobody: the rules a capped run skipped hold the oldest watermarks on
-- the next run and go first. A cap without this order would evaluate whichever
-- workspace happened to sort first, forever.
--
-- The organization is joined in here rather than fetched per rule, because every
-- rule that fires with a `notify` action needs it and N+1 lookups to assemble a
-- batch is the wrong shape. Walks automation_rules_due_idx (02900).
SELECT r.id, r.workspace_id, w.organization_id, r.name, r.trigger,
       r.trigger_config, r.actions, r.last_fired_at, r.created_at
  FROM automation_rules r
  JOIN workspaces w ON w.id = r.workspace_id
 WHERE r.enabled AND w.deleted_at IS NULL
 ORDER BY r.last_fired_at NULLS FIRST, r.id
 LIMIT sqlc.arg(row_limit);

-- name: ClaimAutomationRule :execrows
--
-- The compare-and-set that fires a rule. **This is the loop guard.**
--
-- The watermark is advanced *before* the actions run, and only if it is still
-- exactly where the match query saw it. Two things follow, and both are
-- deliberate:
--
--   * A rule cannot fire twice for one subject. The window the next run reads
--     starts after the subject that was just handled, so the match set that
--     produced this firing can never be produced again.
--   * A second replica that briefly believes it is the leader loses the race
--     rather than duplicating the firing — the same reasoning D77 gives for
--     claiming a webhook delivery with FOR UPDATE SKIP LOCKED under the advisory
--     lock, and the same reason: an advisory lock is released the instant its
--     holder dies, so a moment of overlap is possible and has to cost nothing.
--
-- The trade this direction makes is that a process killed between the claim and
-- the actions loses that firing rather than repeating it. That is the right way
-- round for an instruction that archives links and sends events: a missed
-- notification is recoverable by looking, and a rule that archived the same
-- links twice would be one nobody could trust to run unattended.
--
-- `IS NOT DISTINCT FROM` rather than `=`, so a rule whose watermark is NULL —
-- only reachable by a row written outside this product — compares correctly
-- instead of never matching.
UPDATE automation_rules
   SET last_fired_at = sqlc.arg(watermark)
 WHERE id = sqlc.arg(id)
   AND enabled
   AND last_fired_at IS NOT DISTINCT FROM sqlc.narg(expected);

-- name: MatchExpiredLinks :many
--
-- Links whose expiry fell inside this rule's window.
--
-- **No status filter, on purpose.** `archive_link` is one of the actions a rule
-- may take, so a trigger that excluded archived links would shrink its own match
-- set when it fired — a rule feeding off its own effect, which is exactly what
-- domain/automation.go's TriggerReads/ActionWrites declaration says cannot
-- happen. `links_expiry_idx` (00300) is unusable here for that reason: its
-- predicate carries `status = 'active'`. 02900 adds automation_links_expiry_idx,
-- which does not.
--
-- Soft-deleted links are excluded, and that exclusion is safe for the opposite
-- reason: nothing an automation does writes `deleted_at`.
SELECT id, alias, expires_at
  FROM links
 WHERE workspace_id = @workspace_id
   AND deleted_at IS NULL
   AND expires_at IS NOT NULL
   AND expires_at > sqlc.arg(after)
   AND expires_at <= sqlc.arg(until)
 ORDER BY expires_at, id
 LIMIT sqlc.arg(row_limit);

-- name: MatchExhaustedBudgets :many
--
-- Links whose durable click budget ran out inside the window (M35).
--
-- `link_click_budget.exhausted_at` is stamped in the same transaction that
-- spends the last click, so it is an exact event time. `links.click_count` is
-- not, and 02100 says out loud that it must never be an authorization input;
-- it is not one here either.
--
-- Walks link_click_budget_exhausted_idx (02100) — declared DESC, read forward,
-- which Postgres serves from the same index backwards.
SELECT b.link_id, l.alias, b.exhausted_at
  FROM link_click_budget b
  JOIN links l ON l.id = b.link_id
 WHERE b.workspace_id = @workspace_id
   AND l.deleted_at IS NULL
   AND b.exhausted_at IS NOT NULL
   AND b.exhausted_at > sqlc.arg(after)
   AND b.exhausted_at <= sqlc.arg(until)
 ORDER BY b.exhausted_at, b.link_id
 LIMIT sqlc.arg(row_limit);

-- name: MatchWorkspaceAuditEvents :many
--
-- Administrative events of one action inside the window.
--
-- The blocked-attempt trigger's source, and it reads the audit log because that
-- is the only durable trace a refusal leaves — `blocked_destinations` (01500) is
-- the operator's blocklist, not a record of attempts against it.
--
-- The action is a parameter with exactly one caller, which passes
-- domain.TriggerDestinationBlocked. The three names for that event — the
-- trigger, the audit action and the webhook event — are the same string, and
-- TestTheBlockedVocabularyIsOneWord holds them together.
--
-- Walks audit_logs_workspace_action_idx (02900).
SELECT id, occurred_at, metadata
  FROM audit_logs
 WHERE workspace_id = @workspace_id
   AND action = sqlc.arg(action)
   AND occurred_at > sqlc.arg(after)
   AND occurred_at <= sqlc.arg(until)
 ORDER BY occurred_at, id
 LIMIT sqlc.arg(row_limit);

-- name: ArchiveLinkByAutomation :one
--
-- The archive an automation performs. Idempotent: a link already archived comes
-- back unchanged rather than erroring, so a rule whose window overlapped an
-- interactive archive does not fail its whole firing over it.
--
-- Not `ArchiveLink`, and the difference is the missing actor. Every interactive
-- archive is authorized against a signed-in identity in internal/link; this one
-- is authorized by the rule, which was itself created by somebody holding
-- `automation.write`. Scoped to the workspace in the statement, so a rule can
-- only ever reach its own tenant's links.
--
-- **`expires_at` is not touched**, and that is load-bearing rather than
-- incidental: moving it would make the link-expired trigger match this link
-- again on the next run, which is the self-feeding cycle the vocabulary is
-- arranged to make impossible.
UPDATE links
   SET status = 'archived',
       archived_at = coalesce(archived_at, now()),
       updated_at = now()
 WHERE id = @id AND workspace_id = @workspace_id AND deleted_at IS NULL
RETURNING id, workspace_id, domain_id, alias, primary_url, title, status,
          expires_at, archived_at;
