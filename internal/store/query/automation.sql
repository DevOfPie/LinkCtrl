-- Automation rules and their evaluation (M43).
--
-- Two halves that never meet in one statement, the shape webhooks.sql already
-- has. The rule half is workspace-scoped and reached from the dashboard and the
-- API; the evaluation half is reached only by the scheduler and deliberately
-- carries no workspace parameter in its first query — a run that filtered by
-- tenant would evaluate in tenant order, and the fairness this needs is
-- least-recently-looked-at first across the instance.
--
-- **`last_fired_at` is a watermark, not a diagnostic.** Every match query below
-- takes a half-open window `(@after, @until]`, and `@after` is that watermark.
-- That is what makes a rule unable to trigger itself: a subject is matched once,
-- the watermark moves past it, and no later run can see it again. Removing the
-- advance turns every rule into a runaway that fires on the same subject on
-- every tick.
--
-- **`last_checked_at` is the scheduler's cursor, and it is a different fact.**
-- When a rule was last *looked at*, whether or not it fired. It orders the due
-- query and bounds no window; the watermark bounds every window and orders
-- nothing. Ordering on the watermark is what F83 was — it moves only when a rule
-- fires, so an idle rule held the head of the queue permanently and the
-- hundred-and-first enabled rule on an instance was never evaluated at all
-- (03100).

-- name: CreateAutomationRule :one
--
-- `last_fired_at` is set at creation rather than left NULL. A NULL watermark on
-- a rule created today would mean "every link that ever expired", so the first
-- run of a brand-new rule would fire for the entire history of the workspace.
--
-- `last_checked_at` is left NULL and travels back with the row, as it does in
-- every statement below. NULL sorts first in the due query, so a rule somebody
-- just wrote is looked at on the next tick instead of waiting for its turn — and
-- carrying the column in the projection is what keeps these four statements
-- returning the table's own row type rather than four near-identical structs.
INSERT INTO automation_rules
    (id, workspace_id, name, trigger, trigger_config, actions, enabled, last_fired_at)
VALUES (@id, @workspace_id, @name, @trigger, @trigger_config, @actions, @enabled, @last_fired_at)
RETURNING id, workspace_id, name, trigger, trigger_config, actions, enabled,
          last_fired_at, created_at, updated_at, last_checked_at;

-- name: ListAutomationRules :many
SELECT id, workspace_id, name, trigger, trigger_config, actions, enabled,
       last_fired_at, created_at, updated_at, last_checked_at
  FROM automation_rules
 WHERE workspace_id = @workspace_id
 ORDER BY created_at, id;

-- name: GetAutomationRule :one
SELECT id, workspace_id, name, trigger, trigger_config, actions, enabled,
       last_fired_at, created_at, updated_at, last_checked_at
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
          last_fired_at, created_at, updated_at, last_checked_at;

-- name: DeleteAutomationRule :execrows
DELETE FROM automation_rules WHERE id = @id AND workspace_id = @workspace_id;

-- --- evaluation --------------------------------------------------------------

-- name: ListDueAutomationRules :many
--
-- The rules one run considers, least recently looked at first.
--
-- Bounded by the caller at domain.AutomationRulesPerRun, and ordered so the cap
-- starves nobody — which is a claim this statement can now make, because
-- `last_checked_at` moves for **every** rule a run reached and not only for the
-- ones that fired. The rules a capped run skipped keep the older cursor and go
-- first next time. A cap without this order would evaluate whichever workspace
-- happened to sort first, forever.
--
-- **Ordering on `last_fired_at` is what F83 was**, and the difference is not
-- cosmetic: that column moves only on a firing, idle is exactly what keeps it
-- old, and so the hundred oldest were a fixed set and rule 101 was never
-- evaluated on any run. The two columns are separate facts and 03100 separates
-- them.
--
-- The organization is joined in here rather than fetched per rule, because every
-- rule that fires with a `notify` action needs it and N+1 lookups to assemble a
-- batch is the wrong shape. Walks automation_rules_due_idx, as rebuilt by 03100.
SELECT r.id, r.workspace_id, w.organization_id, r.name, r.trigger,
       r.trigger_config, r.actions, r.last_fired_at, r.created_at
  FROM automation_rules r
  JOIN workspaces w ON w.id = r.workspace_id
 WHERE r.enabled AND w.deleted_at IS NULL
 ORDER BY r.last_checked_at NULLS FIRST, r.id
 LIMIT sqlc.arg(row_limit);

-- name: MarkAutomationRulesChecked :exec
--
-- The cursor advance, for the rules one run actually looked at.
--
-- One statement per run rather than one per rule, so the whole of the fairness
-- mechanism costs a single indexed update however many rules were considered —
-- the per-run bound m43.md asks for is a product of four constants plus this.
--
-- **It writes `last_checked_at` and nothing else.** Not `last_fired_at`: a rule
-- that matched nothing, or matched less than its threshold, has not fired, and
-- moving its watermark would discard the subjects already inside its window.
-- Not `updated_at` either — this is the scheduler's bookkeeping rather than an
-- edit somebody made, and touching it would make every rule on the instance look
-- edited once a minute.
--
-- Rules whose evaluation *failed* are in the list on purpose. A rule with a
-- corrupt `actions` column errors on every pass; leaving its cursor where it was
-- would park it at the head of the queue forever, which is F83 again with a
-- different cause.
UPDATE automation_rules
   SET last_checked_at = sqlc.arg(checked_at)
 WHERE id = ANY(sqlc.arg(ids)::uuid[]);

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
