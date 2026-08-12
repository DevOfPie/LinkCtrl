-- +goose Up
--
-- The first file this product accepts (M50.5).
--
-- **A column, and that is D134 rather than this migration's call.** The
-- question — column, filesystem path, or object store — was filed in
-- upcoming-decisions.md on 2026-08-06 and answered by the owner on 2026-08-07,
-- before any of this was built. Two of the three options add something
-- 03700's successor cannot: a filesystem path needs a volume
-- `docker-compose.yml` does not mount, and an object store is a new required
-- dependency that M57's conformance test is written to forbid. The accepted
-- cost is named there and is real — binary lives in the row and therefore in
-- every `pg_dump`.
--
-- **Nullable, with no default, and NULL means no logo.** An empty `bytea` would
-- be a second spelling of the same fact, and the two would disagree the first
-- time somebody wrote a zero-length upload.
--
-- **The bytes are bounded before they are written, not by a CHECK here.** The
-- request body stops at qr.MaxLogoUploadBytes, the decoded pixels at
-- qr.MaxDecodedLogoPixels, and the *stored* artefact is re-encoded by this
-- product, fitted to qr.MaxLogoPixels — which since D180 resamples rather than
-- refuses — and refused above qr.MaxLogoStoredBytes. So the worst case a row
-- can carry is 1,049,600 bytes, and the arithmetic behind that number is in
-- internal/qr's logo.go. A constraint here would be a fourth place to keep that
-- number, and the one furthest from the code that computes it.
--
-- **What deletion this buys, and what it does not.** `qr_codes.link_id` and
-- `qr_codes.workspace_id` are both ON DELETE CASCADE (00600), so removing a
-- code, a workspace or an organization takes its logos with it and needs no new
-- statement. A *link* is soft-deleted, so that one cascade does not fire until
-- the trash window closes — which is the orphan the hourly sweep in
-- cmd/linkctrl/jobs.go collects, and the reason that sweep exists at all under
-- a column.
ALTER TABLE qr_codes ADD COLUMN logo bytea;

-- What the orphan sweep reads. Partial, because the sweep is looking for the
-- rare row rather than scanning the table: almost no `qr_codes` row carries a
-- logo, and none of the other queries in query/campaigns.sql ever filters on
-- this column — they read `logo IS NOT NULL` as a projection and never as a
-- predicate.
CREATE INDEX qr_codes_logo_idx ON qr_codes (link_id) WHERE logo IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS qr_codes_logo_idx;
ALTER TABLE qr_codes DROP COLUMN IF EXISTS logo;
