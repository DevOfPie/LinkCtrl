-- +goose Up
--
-- The watermark becomes a position, not just an instant (M43, reopened).
--
-- Every match query orders by (event time, id) and fetches at most
-- domain.AutomationMatchesPerRule rows, and the claim that fires a rule used to
-- record only the time half of where the run stopped. When the cap landed inside
-- a group of subjects sharing one timestamp — routine for `link.expired`,
-- because bulk-created links share one `expires_at` — the next window opened
-- strictly after that timestamp, and every tied subject past the cap fell out of
-- all future windows. Not deferred, as the surrounding comments promised:
-- dropped, silently, with the rule's page showing nothing wrong.
--
-- The subject id is the missing half. With it stored, the next run resumes
-- strictly after the pair `(last_fired_at, last_fired_subject_id)` — a keyset
-- cursor over exactly the order the match queries emit — so a tie group split by
-- the cap is re-entered where the previous run stopped instead of skipped.
--
-- One uuid column serves all three triggers because all three subject sources
-- key on uuid: `links.id` (00300), `link_click_budget.link_id` (02100) and
-- `audit_logs.id` (00600). Mixed key types would have forced a different fix.
--
-- NULL means "no subject recorded at this watermark": every row that exists
-- before this migration, every freshly created rule, and every rule re-armed on
-- disabled→enabled. The evaluator reads NULL as "everything at the watermark
-- instant is already spent" — the strict `>` those rows were written under —
-- because the looser guess would re-fire the boundary subjects of every rule
-- that fired before this column existed.
ALTER TABLE automation_rules
    ADD COLUMN last_fired_subject_id uuid;

-- No index. The column is never ordered on and never filtered on: it travels
-- with `last_fired_at` through the due-rule projection and the claim's
-- compare-and-set, both of which already reach the row by primary key.

-- +goose Down
ALTER TABLE automation_rules DROP COLUMN IF EXISTS last_fired_subject_id;
