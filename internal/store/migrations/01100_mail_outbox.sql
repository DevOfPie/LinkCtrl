-- +goose Up
--
-- The mail outbox (decision D23).
--
-- Mail is queued here and delivered by a job on the scheduler that already runs
-- partition maintenance, rather than sent inline. Two properties follow, and
-- both are the reason the table exists rather than an in-memory retry loop:
--
--   * A send survives a restart. An invitation that vanished because a deploy
--     landed mid-retry would be invisible on both ends — nobody receives it and
--     nobody knows one was attempted.
--   * What was attempted is inspectable. `last_error` and `attempts` are the
--     record an operator reads when someone says they never got the mail.
--
-- Deliberately not a dormant Phase 1 table: nothing anticipated this shape, and
-- the rule for those tables is that structure goes in jsonb until the feature
-- arrives. The feature has arrived, so the columns are typed.
--
-- Shape follows webhook_deliveries in 00600 — status, attempts,
-- next_attempt_at, a partial index on the pending set — because it is the same
-- problem and a second spelling of it would be one more thing to learn.

CREATE TABLE mail_outbox (
    id          uuid        PRIMARY KEY,
    -- The rendered message, not a reference to what would render it. A template
    -- change must not silently rewrite a mail that was already queued, and a
    -- row has to stay readable after the code that produced it is gone.
    recipient   text        NOT NULL,
    subject     text        NOT NULL,
    body        text        NOT NULL,
    -- Which template produced it, for the operator reading the table. Not used
    -- to re-render.
    kind        text        NOT NULL,

    status      text        NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'sent', 'failed')),
    attempts    int         NOT NULL DEFAULT 0,
    -- Set on every failure to now() plus the backoff. Never null while pending,
    -- so the claim query is a plain range scan on the partial index below.
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    -- The last failure, verbatim. Kept on a row that later succeeds too: "it
    -- arrived on the fourth attempt" is a different operational story from "it
    -- arrived", and only this column tells them apart.
    last_error  text        NOT NULL DEFAULT '',

    created_at  timestamptz NOT NULL DEFAULT now(),
    sent_at     timestamptz
);

-- The drain query's index. Predicate matches the query exactly: a pending row
-- whose next attempt is due. Sent and failed rows are never scanned by it,
-- which is what lets the table keep its history without the job paying for it.
CREATE INDEX mail_outbox_pending_idx
    ON mail_outbox (next_attempt_at) WHERE status = 'pending';

-- Purging finished rows walks this one. Separate from the pending index
-- because the two never overlap.
CREATE INDEX mail_outbox_finished_idx
    ON mail_outbox (created_at) WHERE status <> 'pending';

-- +goose Down
DROP TABLE IF EXISTS mail_outbox;
