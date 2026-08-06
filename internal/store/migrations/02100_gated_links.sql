-- +goose Up
--
-- Gated links wake up (M35).
--
-- Three of the four gates need nothing here. `links.password_hash`,
-- `links.max_clicks` and `links.one_time` have existed since 00300 and the
-- redirect query has always selected them; what changes in M35 is that
-- something writes them and something reads them. This migration adds only what
-- becomes true the moment that happens.

-- Signed URLs need a secret, and the secret is per workspace.
--
-- Per workspace rather than per instance, because a signature is a statement
-- about *this* workspace's link and the blast radius of a leaked secret should
-- stop at the tenant it belongs to. Per workspace rather than per link, because
-- a per-link secret cannot be rotated without invalidating one link at a time,
-- and because the hot path would then have to fetch a secret it could not cache
-- across aliases.
--
-- bytea rather than text: this is key material, not an identifier, and storing
-- it encoded would invite somebody to log it as a string. Nullable, because
-- every workspace that exists predates the column and the secret is minted
-- lazily the first time somebody asks for a signature — a migration that
-- generated one per row would need a random source inside a DDL transaction and
-- would produce secrets for workspaces that will never sign anything.
ALTER TABLE workspaces ADD COLUMN signing_secret bytea;

-- Whether this link refuses an unsigned request.
--
-- Off by default, and the default is the whole compatibility story: every link
-- that exists keeps answering exactly as it did. A column rather than a
-- derivation from "a secret exists", because minting a workspace secret must not
-- silently gate every link in the workspace.
ALTER TABLE links ADD COLUMN require_signature boolean NOT NULL DEFAULT false;

-- The durable click budget for one-time and max-click links (M35), reused by
-- split testing (M36).
--
-- A table of its own rather than a column on `links`, and the separation is
-- load-bearing in both directions.
--
-- `links.click_count` stays what it has always been: approximate, written by the
-- analytics pipeline after the fact, and never consulted to decide whether a
-- redirect happens. Gating on it would make an asynchronous, lossy counter into
-- an authorization boundary — the exact mistake this table exists to avoid — and
-- would put a synchronous write on the hot path of every link on the instance
-- rather than only the ones that asked for it.
--
-- Redis cannot hold this. The cache is optional by design: a link that may be
-- followed once must be followed once even with Redis absent, and a counter
-- whose disappearance re-opens a spent link is not a counter. So it is Postgres,
-- consumed transactionally, and a row exists only for a link that has actually
-- been clicked at least once while gated.
--
-- `consumed` is monotonic and never reset. Raising `max_clicks` on a link that
-- has already spent its budget therefore re-opens it, which is the behaviour
-- somebody raising a limit is asking for; clearing the gate entirely stops the
-- counter being read at all.
CREATE TABLE link_click_budget (
    link_id      uuid        PRIMARY KEY REFERENCES links(id) ON DELETE CASCADE,
    workspace_id uuid        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    consumed     bigint      NOT NULL DEFAULT 0 CHECK (consumed >= 0),
    -- When the budget ran out, for the owner to read. Not a decision input: the
    -- decision is `consumed` against the link's current limit, so an operator
    -- raising the limit does not have to clear a timestamp as well.
    exhausted_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- The dashboard reads a workspace's exhausted links; the redirect path never
-- does — it addresses one row by primary key.
CREATE INDEX link_click_budget_exhausted_idx
    ON link_click_budget (workspace_id, exhausted_at DESC)
    WHERE exhausted_at IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS link_click_budget;
ALTER TABLE links DROP COLUMN IF EXISTS require_signature;
ALTER TABLE workspaces DROP COLUMN IF EXISTS signing_secret;
