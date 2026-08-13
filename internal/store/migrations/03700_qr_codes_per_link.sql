-- +goose Up
--
-- More than one QR code per link (M50).
--
-- 02700 replaced 00600's non-unique index on `link_id` with `qr_codes_link_key`
-- and said in as many words what it was giving up: *"00600 gave qr_codes a
-- non-unique index on link_id, which is the shape for a link with several codes
-- — different campaigns, different print runs. That is not what this milestone
-- built."* This is that milestone, so the index goes back to the shape 00600
-- gave it.
--
-- **No data changes, and the reason is arithmetic rather than care.** Every row
-- in the table today is unique per link, because the unique index being dropped
-- is what made it so. Relaxing a uniqueness constraint cannot invalidate a row
-- that already satisfies it, so nothing is rewritten, backfilled or migrated
-- here — the two new columns are the whole of the write.
DROP INDEX qr_codes_link_key;
CREATE INDEX qr_codes_link_idx ON qr_codes (link_id);

-- The two things a code needs once there can be more than one of it.
--
-- **label** is what a person reads. It is workspace-controlled free text and it
-- is never in a URL, never in a header and never in the picture: it is how
-- somebody tells *the poster* from *the shop window* in a list, and nothing on
-- the redirect path ever sees it.
--
-- **slug** is what the redirect sees. It travels in the code's payload, in a
-- parameter of its own beside `?src=qr` — never inside `src`, whose vocabulary
-- is closed against user-controlled values on purpose (internal/domain/
-- attribution.go) — and it is resolved against the slugs this link actually has
-- before anything is recorded. Short, because it is printed: the generator emits
-- eight characters from an unambiguous alphabet.
--
-- **The empty slug was the link's default code, and that was load-bearing** —
-- until `04400` moved the identity onto `is_default` (D183), because being the
-- code with no slug is exactly what made the default unremovable. What this
-- paragraph was protecting is protected there instead, and by the same
-- mechanism: a payload with no code parameter records the bare `qr` that M41 and
-- D76 describe, and the breakdown counts that bucket against whichever code
-- holds the flag. So every QR code already printed still attributes to the same
-- code, and no recorded scan was rewritten to give today's default a slug — the
-- split this paragraph refused to cause is still not caused.
--
-- Every row that exists when *this* migration runs gets the empty slug, and it
-- is still what a link's only code carries: a code with nobody to be told apart
-- from has no use for a tag, and gains one when a second code appears beside it.
ALTER TABLE qr_codes ADD COLUMN label text NOT NULL DEFAULT '';
ALTER TABLE qr_codes ADD COLUMN slug  text NOT NULL DEFAULT '';

-- One slug per link, which is what makes resolution on the redirect path a
-- decision rather than a guess. Unique on the pair rather than globally: a slug
-- is only ever looked up in the context of the link that was already resolved,
-- so two links may hold the same one and eight characters buy their full
-- collision resistance per link instead of across the instance.
--
-- It is also what kept the default code single: `('', link_id)` can appear once,
-- so a link cannot acquire a second unnamed code and leave the redirect path
-- with two answers to "which code has no parameter". Since `04400` the single
-- default is `qr_codes_link_default_key`'s to enforce, and this index bounds the
-- other thing it always bounded — one slug per link, so resolving one on the
-- redirect path is a decision rather than a guess.
CREATE UNIQUE INDEX qr_codes_link_slug_key ON qr_codes (link_id, slug);

-- +goose Down
DROP INDEX IF EXISTS qr_codes_link_slug_key;
ALTER TABLE qr_codes DROP COLUMN IF EXISTS slug;
ALTER TABLE qr_codes DROP COLUMN IF EXISTS label;
DROP INDEX IF EXISTS qr_codes_link_idx;
CREATE UNIQUE INDEX qr_codes_link_key ON qr_codes (link_id);
