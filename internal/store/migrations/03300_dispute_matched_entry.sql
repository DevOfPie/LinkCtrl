-- +goose Up
--
-- The blocklist row a dispute is actually about (M45, finding F33).
--
-- 01600 describes `host` as "the host the refusal actually matched rather than
-- the one that was typed", and the service has never written that: it stores
-- `verdict.Host`, which is the host somebody typed, while a decision re-runs the
-- label-boundary walk at decision time and deletes the longest *parent* match.
-- Two things fall out of that gap, and this column closes both.
--
--   * **The queue showed one host and Allow acted on another.** A dispute
--     rendered as `login[.]evil[.]example` deleted `evil.example`, instance-wide,
--     with nothing on the row naming what would go. Worse than a display bug,
--     because the walk runs against the list *as it is when the button is
--     clicked*: a more specific entry added between filing and deciding silently
--     retargets a decision an owner believed they were making about the row they
--     were shown.
--   * **The one-open-dispute-per-host bound at 01600:67-73 counted the wrong
--     thing.** One blocked row admitted one open dispute per distinct subdomain
--     of it, each notifying every owner on the instance, so "a caller who wants a
--     thousand rows in front of the owner needs a thousand distinct blocked
--     hosts" was false by a prefix.
--
-- Additive, and additive is enough: `destination_disputes` does not exist in
-- 0.1.0 at all, so no released instance has a row here, and nothing is
-- backfilled. A dispute filed by an older build carries '' and internal/dispute
-- refuses to guess for it rather than resurrecting the walk this exists to
-- retire — see entryToLift.
-- Empty means no row produced the refusal, which is a real state rather than a
-- missing value: a punycode homograph, credentials in the URL and a third-party
-- feed verdict are all computed on every judgement and hold nothing to delete.
-- Text with a default rather than a nullable column, following
-- decided_by_label above it, so "no entry" has one spelling.
ALTER TABLE destination_disputes
    ADD COLUMN blocked_host text NOT NULL DEFAULT '';

-- One open dispute per blocklist entry, which is what 01600:67-73 always meant.
--
-- Partial twice over, and both predicates carry weight. `status = 'open'` is
-- 01600's: a decided dispute must not block a later one. `blocked_host <> ''` is
-- this migration's, and it is what keeps the two kinds of refusal apart — every
-- computed rule stores '', so a shared index key would collapse every homograph
-- and every credentials dispute on the instance into a single open row and tell
-- the second filer their unrelated destination was already queued.
--
-- 01600's index on (host) stays, unchanged and not superseded. For a list-backed
-- refusal it is subsumed — the same typed host always matches the same row — and
-- for a computed one it is the only bound there is. What it cannot do, and never
-- could, is bound a rule that fires on URL *shape* rather than on the host:
-- `url_credentials` matches on userinfo with the host ignored, so distinct typed
-- hosts are still distinct open disputes. That residue is finding F137, recorded
-- rather than fixed here, because both repairs for it — narrowing what M30
-- refuses, or changing who a filing notifies — are decisions about the product
-- rather than about this table.
CREATE UNIQUE INDEX destination_disputes_open_entry_idx
    ON destination_disputes (blocked_host)
 WHERE status = 'open' AND blocked_host <> '';

-- +goose Down
DROP INDEX IF EXISTS destination_disputes_open_entry_idx;
ALTER TABLE destination_disputes DROP COLUMN blocked_host;
