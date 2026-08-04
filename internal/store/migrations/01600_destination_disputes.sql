-- +goose Up
--
-- Blocked-attempt disputes, and the permission that guards reviewing them (M31).
--
-- The queue this table feeds exists to hand an instance owner a URL a stranger
-- wants them to look at. It is designed as an attack surface rather than
-- decorated as one afterwards, and three of the choices below are that design
-- rather than schema convenience:
--
--   * the attempted URL is stored **defanged**, once, on the way in — the same
--     rule audit_logs.metadata follows since M30, and for the same reason. A
--     value that is inert in the column is inert in every consumer, including
--     the ones written after this migration, which are the ones that will
--     forget.
--   * there is no free-text field the filer controls. A dispute says "look at
--     this host", and the host is the whole payload. A note would be a second
--     stranger-controlled string rendered to the owner, buying context that the
--     defanged URL and the reason code already carry.
--   * `host` is stored plainly beside it, because it is the key the low-confidence
--     list matches on and a decision has to be able to act on it. It is defanged
--     at the point of display, never stored that way.

CREATE TABLE destination_disputes (
    id uuid PRIMARY KEY,

    -- The destination host, lowercased and bare, exactly as blocked_destinations
    -- stores one — the host that was **typed**.
    --
    -- Corrected 2026-08-04 (M45, finding F33). This comment used to claim the
    -- opposite: that the column held "the host the refusal actually matched
    -- rather than the one that was typed". No build ever wrote that. The service
    -- has always stored the typed host here, and the row an `allow` removes is
    -- named by destination_disputes.blocked_host, added by 03300 for exactly
    -- this reason. Blocking 'evil.example' refuses 'login.evil.example': this
    -- column says 'login.evil.example' and blocked_host says 'evil.example'.
    host text NOT NULL,

    -- The attempted destination, stored inert. Never a live URL, in this column
    -- or anywhere downstream of it.
    url_defanged text NOT NULL,

    -- The refusal being disputed, as "<tier>.<rule>" — M30's vocabulary, the
    -- same string the 422 and the audit record carry.
    --
    -- Always a low-confidence code, because the service refuses to file a
    -- dispute about anything else. No CHECK constraint: the rule vocabulary
    -- grows (M32's feeds are the obvious next), and a constraint here would make
    -- that a migration rewriting this line instead of one adding to it.
    reason_code text NOT NULL,

    -- open | allowed | upheld. Derived from nothing; it is the decision itself.
    status text NOT NULL DEFAULT 'open',

    -- Where it was filed. No foreign keys, for the reason audit_logs has none:
    -- the row must stay readable after the organization or the account is gone,
    -- and a dispute outliving its filer is the normal case for a queue somebody
    -- works through slowly.
    organization_id uuid,
    workspace_id    uuid,
    created_by      uuid,
    -- A snapshot taken at write time, like audit_logs.actor_label. It is what a
    -- reviewer actually sees, and it stays meaningful after the account is
    -- deleted.
    created_by_label text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),

    decided_by       uuid,
    decided_by_label text NOT NULL DEFAULT '',
    decided_at       timestamptz
);

-- One open dispute per host, instance-wide.
--
-- Instance-wide because the list it argues with is instance-wide (01500): the
-- same host refused for one workspace is refused for all of them, so two open
-- disputes about it would be two people asking one question.
--
-- Partial, so a decided dispute does not block a later one. A host that is
-- upheld today and re-listed after an argument can be disputed again.
--
-- Corrected 2026-08-04 (M45, finding F33). This comment used to add that the
-- index was "the cheapest bound on somebody filling the queue — a caller who
-- wants a thousand rows in front of the owner needs a thousand distinct blocked
-- hosts". It never bounded that. Keyed on the typed host, one blocked row admits
-- one open dispute per distinct subdomain of it; and `url_credentials` fires on
-- userinfo with the host ignored, so a filer needs no blocked host at all, only
-- distinct strings. 03300's index on blocked_host repairs the first half by
-- counting rows instead of spellings. The second half is not a bound this table
-- can express and is not claimed here any more.
CREATE UNIQUE INDEX destination_disputes_open_host_idx
    ON destination_disputes (host) WHERE status = 'open';

-- The queue's ordering, newest first, matching the keyset pagination every other
-- list in this schema uses.
CREATE INDEX destination_disputes_queue_idx
    ON destination_disputes (created_at DESC, id DESC);

-- The permission that guards reading the queue and deciding what is in it.
--
-- New rather than reusing domains.write, and the reason is what the two grants
-- reach: domains.write changes where one hostname's root sends stray visitors,
-- and this one decides which destinations every workspace on the instance may
-- point at. They are both instance-wide, which is exactly why sharing a slug
-- would make "who may moderate destinations" unanswerable without reading code.
INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-000000000212', 'destinations.review',
     'Review disputed destinations and decide what the low-confidence blocklist holds');

-- Granted explicitly, for the reason 00800, 00900 and 01300 spell their grants
-- out: the seed migration's "owner gets everything" ran once, at its own
-- version, against the permissions that existed then. A permission added later
-- is held by nobody unless it says so here.
--
-- Owner only, and admin deliberately excluded — 01300's reasoning rather than
-- 00900's. An admin arrives by invitation on a default instance, and this
-- decides what *every* organization on the instance may link to; the two
-- administrative roles are the right set for reading one organization's audit
-- log and the wrong set for moderating a list that crosses all of them.
-- +goose StatementBegin
DO $$
DECLARE
    perm_id  uuid := '00000000-0000-4000-8000-000000000212';
    owner_id uuid := '00000000-0000-4000-8000-000000000101';
BEGIN
    INSERT INTO role_permissions (role_id, permission_id)
    VALUES (owner_id, perm_id)
    ON CONFLICT DO NOTHING;
END
$$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM role_permissions WHERE permission_id = '00000000-0000-4000-8000-000000000212';
DELETE FROM permissions WHERE slug = 'destinations.review';
DROP TABLE destination_disputes;
