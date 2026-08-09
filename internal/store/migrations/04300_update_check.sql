-- +goose Up
--
-- Instance-level settings, and the first one (M55).
--
-- **The first table in this schema that belongs to the box rather than to a
-- tenant in it.** `instance_grants` (03400) is about people; this is about the
-- instance's own configuration — the answers an operator gives that are neither
-- an environment variable nor anybody's property. D149 forced it: the update
-- checker *asks* rather than assuming, and there was nowhere for the answer to
-- go. D164 is why the answer can also be absent — see the column below.
--
-- **Exactly one row, enforced by the primary key rather than by convention.**
-- `id boolean PRIMARY KEY DEFAULT true CHECK (id)` admits `true` and nothing
-- else, so a second INSERT conflicts instead of producing a second instance's
-- worth of settings for one instance. The alternative shapes were both worse: a
-- key/value table is a schema that cannot be typed or defaulted, and a table
-- with no constraint is one where "the settings" means "whichever row the query
-- happened to read first".
--
-- DDL here is additive within a minor version like everywhere else, so a later
-- instance-level setting is a column rather than a second table.
CREATE TABLE instance_settings (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),

    -- Whether this instance asks GitHub, once a day, whether a newer LinkCtrl
    -- has been published (M55).
    --
    -- **Nullable, and NULL is the point.** D164 corrects D159: the operator is
    -- asked, and until they answer this instance does not ask GitHub anything.
    -- So the column carries three states and not two —
    --
    --   NULL   nobody has been asked yet, and the check is **off**
    --   true   somebody was asked and said yes
    --   false  somebody was asked and said no
    --
    -- — and `NOT NULL DEFAULT true` is exactly the shape that cannot express the
    -- first. It would have this migration answer, on the operator's behalf, a
    -- question D149 bought the right to have put to them. What D149 bought was
    -- *the operator decides knowingly*, not *on*; applying a default to the
    -- population the first-run prompt cannot reach — every instance that already
    -- exists — spends that guarantee on the smaller half.
    --
    -- Two surfaces write it and neither is this file. A fresh instance answers on
    -- the setup form, which is the first-run prompt D149 named. An instance
    -- **upgrading** into 0.3.0 arrives here NULL and is asked at the first
    -- administrative sign-in after the upgrade; `LINKCTRL_UPDATE_CHECK=false`
    -- overrides both downwards and never upwards (D160).
    --
    -- **The bound, stated here as well as in docs/deployment.md:** an instance
    -- nobody signs into stays NULL, and therefore quiet, forever. That is the
    -- case the feature exists for and it is the price of not answering for
    -- somebody. It is written down rather than discovered.
    update_check_enabled boolean,

    -- When the check last completed, successfully or not.
    --
    -- What makes "at most once a day" a property of the instance rather than of
    -- one process's uptime: a replica restarted every ten minutes reads this and
    -- declines, where a bare ticker would ask GitHub every time it came up.
    -- Written on failure too, for the same reason — a check that fails and then
    -- retries on the next tick is the retry storm the milestone forbids.
    --
    -- NULL means never checked, which is a fresh instance and an upgraded one
    -- alike. Both check on their first tick *after* the question above is
    -- answered with a yes, and neither before it.
    update_checked_at timestamptz,

    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The row exists from the migration on, so every read is an UPDATE or a SELECT
-- of a row that is there. A settings table whose row is created lazily has two
-- states for "nobody has changed anything", and the code then has to decide
-- which one the defaults live in — which is the same mistake as a NOT NULL
-- default one column up, made in the other direction.
INSERT INTO instance_settings (id) VALUES (true);

-- +goose Down
DROP TABLE IF EXISTS instance_settings;
