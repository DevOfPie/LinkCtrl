-- +goose Up
--
-- Destination blocking, low-confidence tier (M30).
--
-- One table, and it holds only the tier that is meant to change without a
-- rebuild. The other two tiers are deliberately not here:
--
--   * The unappealable tier is Phase 1's SSRF refusals. It has no row, no
--     column and no switch anywhere, because a tier that can be edited is a
--     tier that can be overruled, and the party those refusals protect is not
--     the party who would be appealing.
--   * The high-confidence tier is an embedded file compiled into the binary
--     (internal/link/blocked_hosts.txt), shipped exactly like reserved.txt.
--     Overruling it costs a rebuild on purpose.
--
-- So this table is the low-confidence tier and nothing else, which is also why
-- it is a blocklist with no allow column: there is no row anybody could write
-- here that makes a destination acceptable, only rows that make one refused.
-- M31's review queue removes rows from it; it cannot add a permission.

-- Hosts refused at the low-confidence tier, instance-wide.
--
-- Instance-wide rather than per-workspace, matching the environment variable it
-- grows out of. A hostile destination is hostile to every visitor of every
-- workspace on the instance, and a per-workspace list would mean the same
-- phishing host had to be caught N times.
CREATE TABLE blocked_destinations (
    -- The host, lowercased, no scheme and no path. Matched on a label boundary
    -- by the application, so 'evil.example' also refuses 'login.evil.example'
    -- and does not refuse 'notevil.example' — the same rule
    -- LINKCTRL_DESTINATION_BLOCKLIST has always had.
    --
    -- The primary key is the lookup: the application computes a host's parent
    -- suffixes and asks for all of them at once with = ANY, so a match is an
    -- index probe rather than a scan with a LIKE.
    host       text        PRIMARY KEY,

    -- Where the row came from. Three sources exist as of M30:
    --
    --   'env'        rewritten from LINKCTRL_DESTINATION_BLOCKLIST at every boot
    --                and reappearing after a deletion, which is the honest
    --                behaviour for a setting an operator holds in their
    --                environment.
    --   'review'     what a person added — the column default, and what M31's
    --                owner review will write.
    --   'shortener'  the known URL-shortener hosts seeded at the bottom of this
    --                migration, per D39.
    --
    -- Every reconciliation this program runs is scoped to one source, and that
    -- is what the column is for: the boot-time rewrite of the environment list
    -- deletes 'env' rows and nothing else, so neither a host an operator added
    -- by hand nor a seeded shortener can be retired by a restart.
    --
    -- No CHECK constraint, deliberately. Later milestones add sources — a feed
    -- in M32 is the obvious one — and a constraint here would make that a
    -- migration that rewrites this line rather than one that adds to it, which
    -- the additive-DDL rule exists to avoid.
    source     text        NOT NULL DEFAULT 'review',

    -- Why, in the operator's words. Shown to whoever reads the list; never
    -- shown to the person whose link was refused, who gets the reason code.
    reason     text        NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),

    -- Who added it, when a person did. NULL for 'env' rows, which no person
    -- added through the product. No foreign key, for the reason audit_logs has
    -- none: the row must stay readable after the account is gone.
    created_by uuid
);

-- The boot-time reconciliation of the environment list needs to find the rows
-- it wrote last time in order to retire the ones the operator removed.
CREATE INDEX blocked_destinations_source_idx ON blocked_destinations (source);

-- No permission is seeded here, and that is deliberate rather than an
-- omission. M30 gives the list no API: it is read on the management path, fed
-- from the environment at boot, and otherwise changed by the instance owner
-- through M31's review queue, which brings its own permission with the surface
-- that needs it. Seeding a permission now would grant something nothing can
-- yet exercise.

-- The URL-shortener hosts, as data rather than as a compiled file (D39).
--
-- These used to be internal/link/shortener_hosts.txt, embedded in the binary
-- next to the high-confidence list. The rule that separates them is what it
-- costs to be wrong: a list is compiled when overruling it *should* be hard,
-- and is runtime data otherwise. The embedded file makes structural claims
-- about metadata services and control planes, and those stay true for years. A
-- shortener host is neither structural nor authoritative — matching one raises
-- a low-confidence flag the instance owner may overrule — so compiling it
-- imposed a release cycle on data that carries no authority, and new
-- shorteners appear constantly.
--
-- Seeded here, in the migration that creates the table, rather than reconciled
-- at boot the way the environment list is. That difference is the whole point:
-- a migration runs once and never asserts these rows again, so an owner who
-- deletes one has deleted it, and no restart and no rebuild brings it back.
-- Boot-time reconciliation would have made every overrule last until the next
-- restart, which is the release cycle D39 removed wearing a different hat.
--
-- Consequences worth stating for whoever adds to this later:
--
--   * A later migration may add hosts, and must add only hosts no earlier
--     migration has seeded. Re-asserting one would undo the owner's deletion,
--     which is exactly what ON CONFLICT DO NOTHING cannot protect against once
--     the row is gone.
--   * These rows match on a label boundary like every other row in this table,
--     so 'bit.ly' also covers 'links.bit.ly'. That is wider than the exact
--     match the compiled file used, and it is the right width here: one
--     matching rule for the whole table beats a second one that has to be
--     remembered, and the tier this feeds is the one that is allowed to guess.
--
-- Being on this list is a factual claim — "this host is a URL shortener" — and
-- never an accusation. A short link whose destination is another short link
-- hides the real destination from everyone in the chain, including the person
-- creating it and the visitor following it. That is worth a second look and is
-- not worth a refusal nobody can appeal.
INSERT INTO blocked_destinations (host, source, reason) VALUES
    ('bit.ly',      'shortener', 'known URL shortener'),
    ('tinyurl.com', 'shortener', 'known URL shortener'),
    ('t.co',        'shortener', 'known URL shortener'),
    ('goo.gl',      'shortener', 'known URL shortener'),
    ('ow.ly',       'shortener', 'known URL shortener'),
    ('buff.ly',     'shortener', 'known URL shortener'),
    ('is.gd',       'shortener', 'known URL shortener'),
    ('v.gd',        'shortener', 'known URL shortener'),
    ('cutt.ly',     'shortener', 'known URL shortener'),
    ('rebrand.ly',  'shortener', 'known URL shortener'),
    ('rb.gy',       'shortener', 'known URL shortener'),
    ('shorturl.at', 'shortener', 'known URL shortener'),
    ('tiny.cc',     'shortener', 'known URL shortener'),
    ('t.ly',        'shortener', 'known URL shortener'),
    ('s.id',        'shortener', 'known URL shortener'),
    ('bl.ink',      'shortener', 'known URL shortener'),
    ('lnkd.in',     'shortener', 'known URL shortener'),
    ('trib.al',     'shortener', 'known URL shortener'),
    ('dlvr.it',     'shortener', 'known URL shortener')
ON CONFLICT (host) DO NOTHING;

-- +goose Down
DROP TABLE blocked_destinations;
