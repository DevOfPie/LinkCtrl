-- +goose Up
--
-- QR codes and campaigns wake up (M41).
--
-- Both tables have existed since 00600, dormant, with the shape this milestone
-- needs. What is left is two corrections and one thing deliberately not done.

-- **`qr_codes.scan_count` is dropped, not wired.**
--
-- m41.md required this to be decided rather than hedged, and the decision is to
-- remove it. Three reasons, in the order they matter, and D73 carries them in
-- full:
--
--   - Incrementing it costs a write per scan. The redirect path's whole budget
--     is 20ms and every milestone this phase inherits a rule against putting a
--     database write on it; doing the increment in the rollup instead stacks new
--     per-click work on the job M37 has just fixed, which is precisely the load
--     this milestone refuses to add for campaign analytics.
--   - The number would be worse than the one the product already has. A scan is
--     an ordinary click carrying `?src=qr`, so it is already counted per day,
--     deduplicated by visitor, filtered for bots and broken down by device and
--     country. `scan_count` is a monotonic integer with none of that, and two
--     numbers for one quantity means whichever is read first is believed.
--   - Nothing ever wrote it. Every row in every instance holds 0, so the column
--     carries no history and dropping it loses nothing.
--
-- A non-additive DDL statement, and the one this phase makes. The inherited rule
-- is that DDL is additive *within a minor version*; this lands before 0.2.0 on a
-- column that has held one value since it was created and that no released code
-- reads or writes. A dormant column nothing increments is a trap for whoever
-- writes the next feature against it — the 00600 comment says as much in the
-- line above its definition — and leaving it in place to satisfy the letter of
-- the rule would keep the trap for the sake of a value that does not exist.
ALTER TABLE qr_codes DROP COLUMN scan_count;

-- One stored style per link.
--
-- 00600 gave qr_codes a non-unique index on link_id, which is the shape for a
-- link with several codes — different campaigns, different print runs. That is
-- not what this milestone built: the QR endpoint answers for a link, and the
-- style is the one that link's code is drawn with, so two rows for one link
-- would make "the link's style" a question with no answer. The unique index is
-- what makes the upsert in query/campaigns.sql an upsert rather than a race.
--
-- Additive, and it replaces the old index rather than sitting beside it: a
-- unique index on the same single column serves every lookup the other did.
DROP INDEX qr_codes_link_idx;
CREATE UNIQUE INDEX qr_codes_link_key ON qr_codes (link_id);

-- **No new permission, and campaigns need no DDL at all.**
--
-- The permission call is D75: a campaign is a label a link carries and a QR code
-- is a picture of the link's own URL, so both are guarded by the link
-- permissions the workspace already has — `links.read` to see either,
-- `links.create`/`update`/`delete` to change them. That is the call M34 made for
-- routing rules, M36 for split arms and M38 for folders, and D18's delegability
-- question does not arise because there is no new slug to classify.
--
-- campaigns (00600) already carries every column and constraint the CRUD needs,
-- including `campaigns_workspace_slug_key`, which is what makes slug uniqueness
-- per workspace true under concurrency rather than merely checked. `links`
-- already has `campaign_id` and `links_campaign_idx`. Listing a workspace's
-- campaigns reads the unique index on its leading column, so no second index is
-- worth its write cost yet.
--
-- **Campaign analytics is not started**, and no rollup table appears here for
-- that reason: it stays Phase 2+ until M37's rollup fix has proved itself at
-- scale. See Plan.md's Not in Phase 2 table.

-- +goose Down
DROP INDEX qr_codes_link_key;
CREATE INDEX qr_codes_link_idx ON qr_codes (link_id);
ALTER TABLE qr_codes ADD COLUMN scan_count bigint NOT NULL DEFAULT 0;
