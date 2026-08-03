-- +goose Up
--
-- Webhooks get behaviour (M42).
--
-- Both tables have shipped since 00600 with nothing reading or writing them.
-- This migration is the additive half of turning them on: no table is created,
-- no column is dropped, and every column added has a default so the existing
-- (empty) rows stay legal.
--
-- **The queue is Postgres, and this is where that is settled.** `webhook_deliveries`
-- already has the shape a queue needs — status, attempts, next_attempt_at, a
-- partial index on the pending set — and `mail_outbox` (01100) copied it
-- deliberately rather than inventing a second spelling. Riding it means Redis
-- stays what Plan.md says it is, a cache: the Redis Streams idea in Plan.md's
-- upgrade paths is left *unexercised* rather than adopted, because nothing here
-- needs a second durable store and a queue that lives in the cache is a queue
-- that disappears when the cache is flushed.

-- The subscription, typed.
--
-- 00600 stored the whole subscription as jsonb with a comment saying the filter
-- shape is the part most likely to change. That is still true, and it is why
-- `subscription` is left exactly where it is: this milestone builds no filtering.
-- What it does build is a fixed, deliberately small event vocabulary, and a
-- fixed vocabulary is a list of strings — so the list of strings gets a column
-- and the part nobody has designed yet stays in jsonb.
--
-- text[] rather than a jsonb array because the emit query asks `event = ANY(...)`
-- once per write, and an array is a GIN-indexable, type-checked answer to that
-- question. Empty is legal and means the webhook is subscribed to nothing, which
-- is the state a row is in for the instant between insert and its first update.
ALTER TABLE webhooks
    ADD COLUMN events text[] NOT NULL DEFAULT '{}'::text[];

-- What an operator calls this webhook in a list. Not an identifier: two
-- webhooks may share a description, and nothing keys on it.
ALTER TABLE webhooks
    ADD COLUMN description text NOT NULL DEFAULT '';

-- The failure, verbatim and bounded, on the delivery it belongs to.
--
-- `response_code` (00600) says what the receiver answered; this says what
-- happened when there was no answer at all — a refused connection, a timeout, a
-- TLS failure, or this instance refusing to connect because the name resolved
-- somewhere private. Without it "attempts: 6, response_code: null" is the whole
-- story an operator gets, and the two most interesting failures are
-- indistinguishable from each other.
--
-- Scoped to one workspace's own webhook, which is why storing the message is
-- acceptable here and logging it is not: the process log is shared by every
-- tenant on the instance, and a transport error carries the URL inside it.
ALTER TABLE webhook_deliveries
    ADD COLUMN last_error text NOT NULL DEFAULT '';

-- `next_attempt_at` is nullable in 00600 and the claim query needs it not to be
-- for a pending row, or the partial index below is a range scan with a hole in
-- it. Backfilled (the table is empty on every existing instance) and defaulted,
-- rather than made NOT NULL: a delivered row has no next attempt and NULL is the
-- honest value there.
UPDATE webhook_deliveries SET next_attempt_at = now()
 WHERE status = 'pending' AND next_attempt_at IS NULL;

ALTER TABLE webhook_deliveries
    ALTER COLUMN next_attempt_at SET DEFAULT now();

-- Pruning finished deliveries by age walks this one. Separate from
-- webhook_deliveries_pending_idx (00600) because the two predicates never
-- overlap, so neither pays for the other's rows — the same pair mail_outbox has.
CREATE INDEX webhook_deliveries_finished_idx
    ON webhook_deliveries (created_at) WHERE status <> 'pending';

-- One webhook's recent deliveries, newest first: the API listing and the panel
-- on the webhooks page. Nothing indexed this, and without it showing the last
-- twenty attempts is a scan of every attempt ever made.
CREATE INDEX webhook_deliveries_webhook_idx
    ON webhook_deliveries (webhook_id, created_at DESC);

-- The two permissions that guard them.
--
-- Their own, rather than reusing `links.*` the way QR codes and campaigns do
-- (D75). A QR code and a campaign are properties of a link, so somebody who may
-- edit the link may edit them. A webhook is not: it is a standing instruction to
-- make *this server* connect to an address somebody chose, on every link write
-- in the workspace, forever. That is the distinction this whole milestone turns
-- on — the redirect path sends a visitor's browser somewhere, and a webhook
-- sends the server — and a capability that different from editing a link should
-- not arrive free with `links.update`.
--
-- The pair mirrors `apikeys.read` / `apikeys.write`, which is the closest thing
-- in the schema: a workspace integration with a secret, managed by the people
-- accountable for the workspace rather than by everybody who can write a link.
INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-000000000213', 'webhooks.read',
     'See a workspace''s webhooks and what their recent deliveries did'),
    ('00000000-0000-4000-8000-000000000214', 'webhooks.write',
     'Register, edit and remove webhooks: where this server sends workspace events');

-- Granted explicitly, for the reason 00800 and 00900 spell their grants out: the
-- seed migration's "owner gets everything" ran once, at its own version, against
-- the permissions that existed then. A permission added later is held by nobody
-- unless it says so here.
--
-- Owner and admin only. An editor can write links, and every link they write
-- produces an event a webhook would carry off the instance; deciding *where* it
-- goes is the administrative half of that, not the editing half.
-- +goose StatementBegin
DO $$
DECLARE
    read_id  uuid := '00000000-0000-4000-8000-000000000213';
    write_id uuid := '00000000-0000-4000-8000-000000000214';
    owner_id uuid := '00000000-0000-4000-8000-000000000101';
    admin_id uuid := '00000000-0000-4000-8000-000000000102';
BEGIN
    INSERT INTO role_permissions (role_id, permission_id)
    VALUES (owner_id, read_id), (admin_id, read_id),
           (owner_id, write_id), (admin_id, write_id)
    ON CONFLICT DO NOTHING;
END
$$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM role_permissions WHERE permission_id IN (
    '00000000-0000-4000-8000-000000000213',
    '00000000-0000-4000-8000-000000000214');
DELETE FROM permissions WHERE slug IN ('webhooks.read', 'webhooks.write');
DROP INDEX IF EXISTS webhook_deliveries_webhook_idx;
DROP INDEX IF EXISTS webhook_deliveries_finished_idx;
ALTER TABLE webhook_deliveries ALTER COLUMN next_attempt_at DROP DEFAULT;
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS last_error;
ALTER TABLE webhooks DROP COLUMN IF EXISTS description;
ALTER TABLE webhooks DROP COLUMN IF EXISTS events;
