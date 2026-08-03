-- The gates a link can put in front of its destination (M35).
--
-- Three of these run on the redirect path and one does not, and the split is
-- what keeps the gates off the budget of every link that does not use them.
-- Nothing here is consulted for a link whose snapshot says it is ungated; the
-- snapshot carries the flags, the flags decide, and only then does anything
-- below execute.

-- name: GetLinkPasswordHash :one
-- The argon2id hash, read only on the password-submit path.
--
-- **The cached snapshot never carries this.** It carries a bare boolean, so a
-- Redis dump — or a snapshot payload logged by accident — cannot yield an
-- offline cracking target for every password link on the instance. The price is
-- this query, and it is paid once per submitted password rather than once per
-- visit: a GET that renders the challenge never runs it, and a link with no
-- password never reaches it.
--
-- Addressed by link id rather than by alias, because the id came out of the
-- snapshot the resolver already produced and re-deriving it from the alias would
-- be a second lookup of a row we have already identified.
SELECT password_hash
FROM links
WHERE id = $1 AND deleted_at IS NULL;

-- name: ConsumeClickBudget :one
-- Spend one click of a one-time or max-click link's durable budget.
--
-- **One statement, and that is the whole of the concurrency argument.** Two
-- requests for the last click of a one-time link arrive at the same instant on
-- different replicas; both reach here; Postgres serialises them on the row lock
-- the ON CONFLICT path takes, so the second one re-evaluates its WHERE against
-- the first one's committed value and matches nothing. A read-then-write in Go,
-- or even a SELECT ... FOR UPDATE followed by an UPDATE, would need a
-- transaction the caller could forget to open; there is no such transaction to
-- forget here because the statement is the transaction.
--
-- The insert races too: two requests for the *first* click of the same link both
-- try to INSERT, one wins, the loser takes the DO UPDATE branch and is evaluated
-- against the winner's row. That is why the limit test lives in the conflict
-- clause rather than only in the VALUES.
--
-- Returns no row when the budget is spent, which the caller reads as 410. That
-- is deliberately the same shape as "no such link": the caller has a snapshot
-- already and does not need this query to tell it the link exists.
--
-- click_limit is 1 for a one-time link, max_clicks otherwise, the smaller of the
-- two when both are set. A limit below one can never match, which is correct: a
-- link nobody may follow.
INSERT INTO link_click_budget (link_id, workspace_id, consumed, exhausted_at)
SELECT sqlc.arg(link_id)::uuid, sqlc.arg(workspace_id)::uuid, 1,
       CASE WHEN 1 >= sqlc.arg(click_limit)::bigint THEN now() END
WHERE sqlc.arg(click_limit)::bigint >= 1
ON CONFLICT (link_id) DO UPDATE
   SET consumed     = link_click_budget.consumed + 1,
       exhausted_at = CASE WHEN link_click_budget.consumed + 1 >= sqlc.arg(click_limit)::bigint
                           THEN now() ELSE link_click_budget.exhausted_at END,
       updated_at   = now()
 WHERE link_click_budget.consumed < sqlc.arg(click_limit)::bigint
RETURNING consumed, (exhausted_at IS NOT NULL)::boolean AS exhausted;

-- name: GetClickBudget :one
-- What the dashboard shows beside a gated link. Never on the redirect path.
SELECT consumed, exhausted_at
FROM link_click_budget
WHERE link_id = $1;

-- name: GetWorkspaceSigningSecret :one
-- The HMAC key for one workspace, read on the redirect path only for links whose
-- snapshot says they require a signature — and cached in process by the caller,
-- so a signed link costs one query per workspace per process rather than one per
-- request. NULL means the workspace has never minted one, which means nothing
-- in it can carry a valid signature.
SELECT signing_secret
FROM workspaces
WHERE id = $1 AND deleted_at IS NULL;

-- name: EnsureWorkspaceSigningSecret :one
-- Mint the secret if it is not there, and return whichever one is authoritative.
--
-- COALESCE rather than a read-then-write, so two people asking for a signed URL
-- at the same moment cannot end up with signatures made under different keys:
-- the second UPDATE sees the first one's committed value and keeps it. The
-- caller generates the candidate bytes, because a random source belongs in the
-- application rather than in an extension this schema does not require.
UPDATE workspaces
   SET signing_secret = COALESCE(signing_secret, sqlc.arg(candidate)::bytea),
       updated_at     = now()
 WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING signing_secret;
