-- +goose Up
--
-- Bot blocking, per domain and per link (M32.5).
--
-- Columns only. There is no table here because there is no state to keep: the
-- decision is a switch read on the redirect path, and everything a blocked
-- attempt produces already has a home — a click event with is_bot true, and a
-- metric. Nothing is written per refusal, which is the whole point of the
-- milestone's "counted, not audited" rule.
--
-- These columns are also the reason the decision costs no I/O. They sit on the
-- two rows the redirect path already reads, so the cached snapshot carries the
-- policy along with the destination and the hot path reads a struct field
-- rather than asking anything.

-- The link's own setting.
--
-- Three states, as text rather than a nullable boolean. NULL-means-inherit
-- would be shorter and is the trap: every reader would have to remember that
-- NULL is not "off", and the one that forgot would silently stop blocking. A
-- named state cannot be misread, and the CHECK is what keeps the vocabulary
-- from growing by typo.
--
-- 'inherit' is the default, and 'inherit' under a domain that blocks nothing is
-- off — so every link that exists when this migration runs behaves exactly as
-- it did the moment before. That default is deliberate rather than cautious:
-- the classifier this feature is built on has never had its false-positive rate
-- measured, because until now nothing depended on it, and a person it
-- misclassifies has no way past the refusal until Phase 3. Whoever switches
-- blocking on takes that cost knowingly.
ALTER TABLE links
    ADD COLUMN bot_blocking text NOT NULL DEFAULT 'inherit'
        CHECK (bot_blocking IN ('inherit', 'on', 'off'));

-- The domain's two settings.
--
-- Two booleans rather than one three-state column, because they answer two
-- different questions: whether the domain blocks bots, and whether a link
-- underneath it may decide otherwise. Enforcement is administrative policy —
-- one hostname serves every workspace on this instance, so it is guarded by
-- domains.write like the root redirect, and not by links.update.
ALTER TABLE domains
    ADD COLUMN block_bots boolean NOT NULL DEFAULT false;

ALTER TABLE domains
    ADD COLUMN block_bots_enforced boolean NOT NULL DEFAULT false;

-- Enforcement without blocking is not a state, it is a question nobody asked.
--
-- The constraint is what makes the domain genuinely three-valued — off, on,
-- enforced — which is what lets precedence be a nine-cell table instead of a
-- twelve-cell one with three cells whose meaning has to be invented. Without
-- it, `enforced = true, block_bots = false` reaches the precedence function and
-- somebody has to decide there whether it means "force off" or "off"; with it,
-- the row cannot exist and the question never arrives at the hot path.
ALTER TABLE domains
    ADD CONSTRAINT domains_enforced_requires_blocking
        CHECK (NOT block_bots_enforced OR block_bots);

-- No index on either column. Nothing queries by them: the redirect path reads
-- them from the row it was already fetching, and the management surfaces read
-- the single default domain by its own key.

-- +goose Down
ALTER TABLE domains DROP CONSTRAINT IF EXISTS domains_enforced_requires_blocking;
ALTER TABLE domains DROP COLUMN IF EXISTS block_bots_enforced;
ALTER TABLE domains DROP COLUMN IF EXISTS block_bots;
ALTER TABLE links DROP COLUMN IF EXISTS bot_blocking;
