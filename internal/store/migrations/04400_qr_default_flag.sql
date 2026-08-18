-- +goose Up
--
-- The default QR code becomes a property rather than an absence (M50, D183).
--
-- 03700 made the empty slug the default code's identity and said why: every
-- picture this product had ever drawn carried a payload with no code parameter
-- in it, so the code those pictures resolve to is the one with no slug. That
-- identity is exactly what made it unremovable — the owner's report, F222:
-- *"As long as there are multiple QR codes any of them should be able to be
-- removed, currently the first one cannot be removed."*
--
-- So the identity moves onto a flag, and the row it used to be gets a slug like
-- every other row.
ALTER TABLE qr_codes ADD COLUMN is_default boolean NOT NULL DEFAULT false;

-- **The flag goes where the identity was**: every row that carries the empty
-- slug today is its link's default code, which is what 03700 made it, and there
-- is at most one per link because `qr_codes_link_slug_key` is unique on the
-- pair.
UPDATE qr_codes SET is_default = true, updated_at = now() WHERE slug = '';

-- **The link that has codes but no row for its default.** `CreateQRCode` did not
-- write one for the code it was adding a second one beside: a link's default
-- exists whether or not `qr_codes` holds it (D139), so a link could carry one
-- named row and a default synthesised on every read. That is the exact state the
-- owner reported — two codes in the list and no way to remove the first — and
-- there is no row to flag, so it gets one.
--
-- The style is `{}`, which `decodeQRStyle` normalizes to the product default:
-- the same style every read of that code has already been answering with, so
-- nothing about the picture changes. What does change is `stored`, and that is
-- what `stored` has always meant — this code now has a row.
--
-- **Links with no `qr_codes` rows at all are left alone**, which is nearly every
-- link on an instance. Their default code is still synthesised, still carries no
-- slug and still draws the payload every already-printed picture carries. A row
-- per link would reverse M41's *twenty untouched links carry no rows at all* and
-- D139 with it, for a code that has nothing to be told apart from.
INSERT INTO qr_codes (id, link_id, workspace_id, slug, label, style, is_default)
SELECT gen_random_uuid(), l.id, l.workspace_id, '', '', '{}'::jsonb, true
  FROM links l
 WHERE EXISTS (SELECT 1 FROM qr_codes q WHERE q.link_id = l.id)
   AND NOT EXISTS (SELECT 1 FROM qr_codes q WHERE q.link_id = l.id AND q.slug = '');

-- **The slug the default code has been going without, for the links that carry
-- more than one code.** A code gains a tag when it stops being alone, and these
-- links passed that point before this migration existed: their default is one of
-- several, so it needs the identity that makes it removable, addressable and
-- tellable apart. A link whose only code is the default keeps the empty slug —
-- it has nobody to be distinguished from, and giving it a tag would change what
-- its picture says for no reader's benefit.
--
-- **A generated slug, and the generation is deliberately not the application's.**
-- domain.NewQRCodeSlug emits eight lowercase base32 characters; this emits eight
-- lowercase hex ones. Both satisfy domain.ValidQRCodeSlug, which is a shape test
-- over `[a-z0-9]` and never a test of how the shape was reached, and a migration
-- that had to call into Go to name a row would be a migration that could not run
-- from `goose` alone. 02600 backfilled a verification token the same way and for
-- the same reason. Thirty-two bits rather than forty, which is the same kind of
-- number for the same job: a slug is a printed handle and not a secret, and the
-- unique index is what makes a collision a failure rather than a silent merge.
--
-- **Nothing already printed changes what it means.** A picture of one of these
-- codes carries no `qrc`, records the bare `qr` it has always recorded, and is
-- counted against whichever code holds the flag — which is this one. What
-- changes is the picture downloaded next, and only that.
UPDATE qr_codes q
   SET slug       = substr(replace(gen_random_uuid()::text, '-', ''), 1, 8),
       updated_at = now()
 WHERE q.slug = ''
   AND EXISTS (SELECT 1 FROM qr_codes o WHERE o.link_id = q.link_id AND o.id <> q.id);

-- One default per link, which is what makes "the code an untagged scan resolves
-- through" a lookup rather than a choice between candidates. Partial, so it
-- constrains the flagged rows and says nothing about the rest.
--
-- **Additive, and that is not an accident.** An instance running the previous
-- release against this schema writes `is_default = false` by column default and
-- can therefore never collide with this index.
CREATE UNIQUE INDEX qr_codes_link_default_key ON qr_codes (link_id) WHERE is_default;

-- +goose Down
--
-- The slugs generated above cannot be un-generated: which row was the default is
-- recoverable from the flag, but a picture printed while this migration was
-- applied carries a slug in it. So the down path puts the identity back where
-- 03700 had it — the flagged row's slug emptied — and accepts that those
-- pictures then read as unrecognised, which attributes them to the default code
-- exactly as any retired slug is attributed.
DROP INDEX IF EXISTS qr_codes_link_default_key;
UPDATE qr_codes SET slug = '' WHERE is_default;
ALTER TABLE qr_codes DROP COLUMN IF EXISTS is_default;
