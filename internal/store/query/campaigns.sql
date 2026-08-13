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

-- **`q.*` is gone from the four reads below, and that is M50.5 rather than
-- style.** *(Three when M50.5 wrote this; `GetDefaultQRCode` is D183's, and it
-- carries the same explicit list for the same reason.)* `qr_codes` now carries a `logo bytea` (03800, D134) bounded at
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
-- One code of a link's, by slug. No rows means the code does not exist, and the
-- service reports that.
--
-- **The default code is not reachable here and that is the point** (D183). It
-- used to be `slug = ''`; it is now whichever row carries `is_default`, which is
-- GetDefaultQRCode's job, because a caller that wanted "the default" and passed
-- the empty string would silently match nothing at all now that no row holds it.
SELECT q.id, q.link_id, q.workspace_id, q.style, q.created_at, q.updated_at,
       q.label, q.slug, q.is_default, (q.logo IS NOT NULL)::boolean AS has_logo
  FROM qr_codes q
JOIN links l ON l.id = q.link_id
WHERE q.link_id = $1 AND q.workspace_id = $2 AND q.slug = $3 AND l.deleted_at IS NULL;

-- name: GetDefaultQRCode :one
-- The code an untagged scan resolves through (M50's reopening, D183).
--
-- One flagged row at most, which `qr_codes_link_default_key` (04400) is what
-- makes true: a partial unique index over `link_id WHERE is_default`. No rows
-- means the link's default code has never been written down — the synthesised
-- default D139 describes — and the service answers for it at the product style
-- rather than reporting an absence.
--
-- **The empty slug is a fallback rather than the answer, and it is the second
-- half of what makes this migration safe.** 03700's identity was `slug = ''`,
-- and a row can still arrive carrying it and not the flag: written by the
-- previous release during a rolling deploy, when `is_default` is a column it
-- does not know about, or written by hand. Reading the flag alone would report
-- such a link as having no default at all, and the next style write would then
-- insert a second unnamed row against `qr_codes_link_slug_key`. Preferring the
-- flag and falling back to the empty slug costs one ORDER BY and makes both
-- spellings of the same fact resolve to the same row. `LIMIT 1` because the two
-- can name different rows only on a link that has more than one code, where the
-- empty slug does not occur at all.
SELECT q.id, q.link_id, q.workspace_id, q.style, q.created_at, q.updated_at,
       q.label, q.slug, q.is_default, (q.logo IS NOT NULL)::boolean AS has_logo
  FROM qr_codes q
JOIN links l ON l.id = q.link_id
WHERE q.link_id = $1 AND q.workspace_id = $2 AND l.deleted_at IS NULL
  AND (q.is_default OR q.slug = '')
ORDER BY q.is_default DESC
LIMIT 1;

-- name: ListQRCodes :many
-- Every code a link carries (M50), default first.
--
-- Unpaginated, and bounded instead by domain.MaxQRCodesPerLink, which is the
-- same trade ListCampaigns makes: the cap is small enough that a page of them is
-- the whole set, and a pager over a list that cannot exceed it would be a
-- control nobody ever operates.
--
-- `NOT (is_default OR slug = '')` sorts false before true, so the default code
-- leads whatever order the rest were created in — it is the one every untagged
-- scan attributes to, and a list that buried it would bury the answer to "which
-- of these is the one my existing posters land on". The sort key used to be
-- `q.slug <> ''` alone, and it moved with the identity (D183): the flag can now
-- be set on any row, so the list re-orders when the reader moves it, which is
-- the visible half of what setting a default does. The empty slug stays in the
-- key as GetDefaultQRCode's fallback, and for the same reason.
SELECT q.id, q.link_id, q.workspace_id, q.style, q.created_at, q.updated_at,
       q.label, q.slug, q.is_default, (q.logo IS NOT NULL)::boolean AS has_logo
  FROM qr_codes q
JOIN links l ON l.id = q.link_id
WHERE q.link_id = $1 AND q.workspace_id = $2 AND l.deleted_at IS NULL
ORDER BY (NOT (q.is_default OR q.slug = '')), q.created_at, q.id;

-- name: OldestQRCode :one
-- The code that has existed longest, which is what a removed default promotes to
-- (M50's reopening).
--
-- `created_at, id` is the order ListQRCodes and ResolveAliasForRedirect already
-- enumerate codes in, so the promoted code is the one at the top of the list the
-- reader is looking at. Excluding a row by id rather than filtering on
-- `is_default`, because the caller runs this *after* deleting the flag-holder in
-- the same transaction and inside it that row is already gone — the exclusion is
-- what makes the statement correct if it is ever called before one.
SELECT q.id, q.slug
  FROM qr_codes q
WHERE q.link_id = $1 AND q.workspace_id = $2 AND q.id <> $3
ORDER BY q.created_at, q.id
LIMIT 1;

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
-- `is_default` is not in the DO UPDATE list either, and for the strongest of the
-- three reasons: which code an untagged scan resolves through is not something a
-- style write may move. ClearDefaultQRCode and MarkDefaultQRCode are the pair
-- that moves it, and they are the only pair that does.
INSERT INTO qr_codes (id, link_id, workspace_id, slug, label, style, is_default)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (link_id, slug) DO UPDATE
   SET style = EXCLUDED.style, updated_at = now()
RETURNING id, link_id, workspace_id, style, created_at, updated_at,
          label, slug, is_default, (logo IS NOT NULL)::boolean AS has_logo;

-- name: NameQRCode :execrows
-- Gives a slug to the one code that may not have one (M50's reopening, D183).
--
-- A link's only code carries no slug: there is nothing to tell it apart from,
-- and handing one out while writing a style would change what a picture says.
-- When a second code appears the first one needs a tag, and this is the
-- statement that writes it.
--
-- **`AND slug = ''` is what makes it structurally incapable of a rename.** A
-- slug is printed, so moving one breaks every copy already in the world —
-- UpdateQRCodeLabel says so above and this is the same rule enforced by the
-- WHERE clause rather than by the caller. Naming a code that has no name is not
-- moving anything: nothing printed carries the value being replaced, because
-- there was no value.
--
-- **`is_default = true` goes with the slug, and it is the one statement in this
-- file that sets the flag without clearing another.** The row this reaches is
-- whichever row GetDefaultQRCode answered with, and that read falls back to the
-- empty slug for a row the flag never reached — one written by the previous
-- release during a rolling deploy, where `is_default` is a column it does not
-- know about. Taking the empty slug off such a row without putting the flag on
-- it would leave the link matching neither half of `(is_default OR slug = '')`:
-- no default at all, a phantom code synthesised into every list and breakdown,
-- and the untagged `qr` bucket no longer folding onto the code every already-
-- printed picture of this link resolves through. The empty slug and the flag are
-- two spellings of the same fact, so the statement that removes one writes the
-- other.
--
-- Against `qr_codes_link_default_key` this is safe by the same read: it runs
-- only on a row carrying the empty slug, and GetDefaultQRCode orders the flag
-- first, so a link with some *other* row flagged never returns this row to be
-- named. What the read cannot rule out is the flag moving between it and this
-- write, which is a unique violation and is the caller's to answer — CreateQRCode
-- re-reads the winner rather than failing over it.
UPDATE qr_codes SET slug = $3, is_default = true, updated_at = now()
 WHERE id = $1 AND workspace_id = $2 AND slug = '';

-- name: UpdateQRCodeLabel :execrows
-- Renames one code. The slug is untouched on purpose: it is printed, and a
-- rename that moved it would break every copy already in the world.
UPDATE qr_codes SET label = $3, updated_at = now()
 WHERE id = $1 AND workspace_id = $2;

-- Moving the flag an untagged scan resolves through takes two statements and one
-- transaction (M50's reopening, D183).
--
-- **Two rather than one, and the reason is the index rather than taste.**
-- `UPDATE … SET is_default = (id = $3)` over the whole link reads as the obvious
-- single statement, and `qr_codes_link_default_key` is a plain unique index,
-- which Postgres checks as each row version is written rather than at the end of
-- the statement. Such an update collides with itself whenever the scan reaches
-- the incoming default before the outgoing one — the same failure
-- `UPDATE t SET n = n + 1` has on a unique column. A partial index cannot be
-- declared DEFERRABLE, because only a constraint can and a constraint cannot be
-- partial, so the ordering is made explicit instead: clear, then set, inside the
-- transaction the service opens.
--
-- The window between them holds a link with no default at all. It is invisible:
-- the transaction has not committed, so no reader outside it sees either write,
-- and inside it the only reader is the second statement.

-- name: ClearDefaultQRCode :execrows
-- The first half. Takes the flag off whichever row holds it, or off nothing.
UPDATE qr_codes SET is_default = false, updated_at = now()
 WHERE link_id = $1 AND workspace_id = $2 AND is_default;

-- name: MarkDefaultQRCode :execrows
-- The second half, and never run on its own: without the clear before it, it is
-- the collision the comment above describes.
UPDATE qr_codes SET is_default = true, updated_at = now()
 WHERE id = $1 AND workspace_id = $2;

-- name: DeleteQRCodeByID :execrows
-- Removes one code. Scoped by workspace rather than by link, because the id is
-- already unique and the service has resolved the link before it gets here; the
-- workspace column is the tenancy check.
--
-- **`AND slug <> ''` is gone** (D183). It was what refused to delete the default
-- code, back when the default *was* the empty slug; the refusal that replaces it
-- is the service's, and it is about arithmetic rather than identity — a link's
-- last code cannot be removed, whichever one it is.
--
-- The logo goes with the row, which is the whole of what D134 bought: no second
-- statement, and no way for the two to come apart.
DELETE FROM qr_codes WHERE id = $1 AND workspace_id = $2;

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
