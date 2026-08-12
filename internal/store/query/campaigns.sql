-- Campaigns and QR codes (M41).
--
-- Both tables were created dormant by 00600 and woken by 02700. They share this
-- file because they share a milestone and neither is large enough to be worth
-- its own; nothing else connects them.
--
-- **A campaign is soft-deleted and a QR style is not.** The asymmetry is
-- deliberate. A campaign names a body of work whose links keep their history
-- after it ends, and `links.campaign_id` is ON DELETE SET NULL, so a hard delete
-- would unlabel every link the moment somebody tidied up a finished campaign —
-- exactly the failure the folder migration (02400) argues against. `deleted_at`
-- keeps the row, and every statement here filters on it. A QR style is a
-- rendering preference with nothing to restore: deleting it returns the link's
-- code to the default style, which is the state every link starts in.

-- name: CreateCampaign :one
INSERT INTO campaigns (id, workspace_id, name, slug, description, starts_at, ends_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetCampaign :one
-- Workspace-scoped like GetLink and GetFolder, and for the same reason: the
-- wrong workspace returns no rows rather than a row the caller must remember to
-- reject.
SELECT * FROM campaigns
WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL;

-- name: ListCampaigns :many
-- Every campaign in the workspace, with how many live links carry it.
--
-- The count is a grouped scan of links_campaign_idx rather than a correlated
-- subquery per campaign, exactly as ListFolders counts filed links: a workspace
-- with many campaigns costs one pass instead of one index probe each.
--
-- Unpaginated, and bounded instead by domain.MaxCampaignsPerWorkspace. A
-- campaign list is a picker as much as it is a page — the link form offers it —
-- and a picker that paginates is a picker nobody can choose from.
SELECT c.*, COALESCE(k.n, 0)::bigint AS link_count
FROM campaigns c
LEFT JOIN (
    SELECT l.campaign_id, count(*) AS n
      FROM links l
     WHERE l.workspace_id = sqlc.arg(workspace_id)
       AND l.deleted_at IS NULL
       AND l.campaign_id IS NOT NULL
     GROUP BY l.campaign_id
) k ON k.campaign_id = c.id
WHERE c.workspace_id = sqlc.arg(workspace_id) AND c.deleted_at IS NULL
ORDER BY lower(c.name), c.id;

-- name: CountCampaigns :one
SELECT count(*) FROM campaigns
WHERE workspace_id = $1 AND deleted_at IS NULL;

-- name: UpdateCampaign :one
-- Partial update through COALESCE, like UpdateLink. The two schedule bounds are
-- three-valued through their own clear flags, because "leave the end date alone"
-- and "this campaign no longer ends" are different requests and one nullable
-- parameter cannot express both.
UPDATE campaigns
   SET name        = COALESCE(sqlc.narg(name), name),
       slug        = COALESCE(sqlc.narg(slug), slug),
       description = COALESCE(sqlc.narg(description), description),
       starts_at   = CASE WHEN sqlc.arg(clear_starts_at)::boolean THEN NULL
                          ELSE COALESCE(sqlc.narg(starts_at), starts_at) END,
       ends_at     = CASE WHEN sqlc.arg(clear_ends_at)::boolean THEN NULL
                          ELSE COALESCE(sqlc.narg(ends_at), ends_at) END,
       updated_at  = now()
 WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id) AND deleted_at IS NULL
RETURNING *;

-- name: DeleteCampaign :execrows
-- Soft. The links keep their history; `links.campaign_id` is cleared by the
-- statement below rather than by a cascade, because a cascade would fire on a
-- hard delete this statement never performs.
UPDATE campaigns SET deleted_at = now(), updated_at = now()
 WHERE id = $1 AND workspace_id = $2 AND deleted_at IS NULL;

-- name: UnassignCampaignLinks :execrows
-- Takes every link out of a deleted campaign.
--
-- Run in the same transaction as DeleteCampaign. Without it the links keep an id
-- pointing at a row no query returns, which is a link filtered by a campaign
-- that is not in the campaign list — the invisible-rows failure 02400 describes
-- for folders, in the one place the schema does not prevent it.
UPDATE links SET campaign_id = NULL, updated_at = now()
 WHERE workspace_id = $2 AND campaign_id = $1;

-- **`q.*` is gone from the three reads below, and that is M50.5 rather than
-- style.** `qr_codes` now carries a `logo bytea` (03800, D134) bounded at
-- qr.MaxLogoStoredBytes — a little over a megabyte a row — and a link may hold
-- domain.MaxQRCodesPerLink of them. A star projection would fetch every one of
-- those bytes to draw a list of names, so the reads carry an explicit column
-- list and report the logo as the one fact a reader of the list needs:
-- **whether there is one**. `logo IS NOT NULL` is answered from the row's TOAST
-- pointer without detoasting the value, so asking costs nothing.
--
-- The bytes themselves are read by nothing here. Nothing in M50.5 serves a
-- stored logo back — the two operations are set and clear — and M50.6, which
-- composites one into a picture, is where a query that reads them belongs.

-- name: GetQRCode :one
-- One code of a link's, by slug. No rows is not an error for the default code
-- (slug ''): it means the default style, which is what every link's code is
-- drawn with until somebody changes it. For any other slug no rows means the
-- code does not exist, and the service reports that.
SELECT q.id, q.link_id, q.workspace_id, q.style, q.created_at, q.updated_at,
       q.label, q.slug, (q.logo IS NOT NULL)::boolean AS has_logo
  FROM qr_codes q
JOIN links l ON l.id = q.link_id
WHERE q.link_id = $1 AND q.workspace_id = $2 AND q.slug = $3 AND l.deleted_at IS NULL;

-- name: ListQRCodes :many
-- Every code a link carries (M50), default first.
--
-- Unpaginated, and bounded instead by domain.MaxQRCodesPerLink, which is the
-- same trade ListCampaigns makes: the cap is small enough that a page of them is
-- the whole set, and a pager over a list that cannot exceed it would be a
-- control nobody ever operates.
--
-- `q.slug <> ''` sorts false before true, so the default code leads whatever
-- order the rest were created in — it is the one every already-printed code
-- attributes to, and a list that buried it would bury the answer to "which of
-- these is the one on my existing posters".
SELECT q.id, q.link_id, q.workspace_id, q.style, q.created_at, q.updated_at,
       q.label, q.slug, (q.logo IS NOT NULL)::boolean AS has_logo
  FROM qr_codes q
JOIN links l ON l.id = q.link_id
WHERE q.link_id = $1 AND q.workspace_id = $2 AND l.deleted_at IS NULL
ORDER BY (q.slug <> ''), q.created_at, q.id;

-- name: CountQRCodes :one
-- What domain.MaxQRCodesPerLink is checked against, the way campaign creation
-- checks CountCampaigns.
SELECT count(*) FROM qr_codes WHERE link_id = $1 AND workspace_id = $2;

-- name: UpsertQRCode :one
-- One row per (link, slug), which qr_codes_link_slug_key (03700) is what makes
-- true. Without the unique index this is two concurrent inserts and a link with
-- two codes answering to one name.
--
-- The label is not in the DO UPDATE list. This statement is how a *style* is
-- written, and a style write must not silently rename the code it is drawn for;
-- UpdateQRCodeLabel is the operation that renames one. **Nor is the logo**, for
-- the same reason and with more at stake: restyling a code must not throw away
-- the image somebody uploaded to it, and the insert branch leaves the column at
-- its NULL default because a code that has just come into being has no logo.
INSERT INTO qr_codes (id, link_id, workspace_id, slug, label, style)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (link_id, slug) DO UPDATE
   SET style = EXCLUDED.style, updated_at = now()
RETURNING id, link_id, workspace_id, style, created_at, updated_at,
          label, slug, (logo IS NOT NULL)::boolean AS has_logo;

-- name: UpdateQRCodeLabel :execrows
-- Renames one code. The slug is untouched on purpose: it is printed, and a
-- rename that moved it would break every copy already in the world.
UPDATE qr_codes SET label = $3, updated_at = now()
 WHERE id = $1 AND workspace_id = $2;

-- name: DeleteQRCode :execrows
-- Returns the link's default code to the default style. A hard delete, because
-- the row holds nothing but the preference being withdrawn.
DELETE FROM qr_codes WHERE link_id = $1 AND workspace_id = $2 AND slug = '';

-- name: DeleteQRCodeByID :execrows
-- Removes one named code. Scoped by workspace rather than by link, because the
-- id is already unique and the service has resolved the link before it gets
-- here; the workspace column is the tenancy check.
--
-- The logo goes with the row, which is the whole of what D134 bought: no second
-- statement, and no way for the two to come apart.
DELETE FROM qr_codes WHERE id = $1 AND workspace_id = $2 AND slug <> '';

-- --- QR logos (M50.5) --------------------------------------------------------

-- name: SetQRCodeLogo :execrows
-- Stores the re-encoded image against one code.
--
-- **One statement, so setting and replacing are the same operation and neither
-- has a gap in it.** A logo that replaced another leaves nothing behind to
-- collect: the previous value is overwritten by this write rather than deleted
-- by a second one, which is the property the storage decision was chosen for
-- (D134) and the reason "replacing removes the artefact it replaced" needs no
-- code of its own.
--
-- The bytes are already bounded — the request body by http.MaxBytesReader, the
-- decode by qr.MaxDecodedLogoPixels, and this value by qr.MaxLogoStoredBytes,
-- which internal/qr enforces rather than assumes. Since D180, qr.MaxLogoPixels
-- bounds the *stored* artefact alone and refuses nothing: an image above it is
-- resampled down to it before it reaches this statement.
UPDATE qr_codes SET logo = $3, updated_at = now()
 WHERE id = $1 AND workspace_id = $2;

-- name: ClearQRCodeLogo :execrows
-- Removes the image, and the row stays: a code without a logo is a code.
--
-- NULL rather than an empty bytea, because the schema has one spelling for "no
-- logo" and two would disagree the first time somebody wrote a zero-length one.
UPDATE qr_codes SET logo = NULL, updated_at = now()
 WHERE id = $1 AND workspace_id = $2;

-- name: GetQRCodeLogo :one
-- The bytes, for the one thing they are for (M50.6).
--
-- **The only read in this file that projects the column**, and it is separate
-- from GetQRCode rather than folded into it for exactly the reason the three
-- reads above stopped saying `q.*`: drawing a list of twenty names must not
-- fetch twenty images. This is called once, by a surface that is about to
-- composite one code's logo into one picture, and only for a code whose
-- `has_logo` already said there is something to fetch.
--
-- NULL comes back for a code with no logo, which the service reads as "nothing
-- to draw" rather than as an error: `has_logo` and this can disagree by exactly
-- one concurrent clear, and the honest answer to that race is the picture
-- without the logo.
SELECT q.logo
  FROM qr_codes q
JOIN links l ON l.id = q.link_id
WHERE q.link_id = $1 AND q.workspace_id = $2 AND q.slug = $3 AND l.deleted_at IS NULL;

-- name: ClearOrphanedQRCodeLogos :execrows
-- The orphan sweep, run hourly by the maintenance pass.
--
-- **What is orphaned under a column, and what is not.** Removing a code, a
-- workspace or an organization takes its logos by cascade, and replacing one is
-- the single UPDATE above, so none of those can leave bytes behind. Deleting a
-- *link* can, and does: a link is soft-deleted with a purge deadline, so its
-- `qr_codes` rows survive the whole trash window while every read in this file
-- filters them out with `l.deleted_at IS NULL`. Those bytes are unreachable
-- through the product — the endpoint that would clear them answers 404 for a
-- deleted link — and they sit in the row and in every `pg_dump` until the purge
-- fires, which for a large backlog is several hourly runs away and for a row
-- the purge skips is longer still.
--
-- This is what makes m50.5.md's claim *deleting the link removes its artefacts*
-- true rather than merely intended. The row itself is left alone: the trash
-- window exists so a link can be brought back by hand, and the artefact is the
-- thing deletion was asked to remove.
--
-- Idempotent by construction — a second run matches nothing, because `logo IS
-- NOT NULL` is the predicate. Bounded like every other pass in that job, and
-- SKIP LOCKED so it can never block, or be blocked by, a concurrent write to
-- the same code.
WITH doomed AS (
    SELECT q.id
      FROM qr_codes q
      JOIN links l ON l.id = q.link_id
     WHERE q.logo IS NOT NULL
       AND l.deleted_at IS NOT NULL
     ORDER BY q.id
     LIMIT sqlc.arg(batch_size)::int
       FOR UPDATE OF q SKIP LOCKED
)
UPDATE qr_codes SET logo = NULL, updated_at = now()
 WHERE id IN (SELECT id FROM doomed);
