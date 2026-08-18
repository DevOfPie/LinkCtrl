-- Account recovery: the reset tokens a forgotten password is repaired with
-- (M51). The account's own row is written through query/auth.sql's
-- UpdateUserPassword, so there is one password-writing statement in the product
-- and not two.

-- name: CreatePasswordReset :one
INSERT INTO password_resets (
    id, user_id, token_hash, expires_at
) VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetPasswordResetByTokenHash :one
-- The reset lookup, inside the transaction that spends the row.
--
-- FOR UPDATE, so two submissions of the same link serialize and the second sees
-- the row the first consumed. The join is what makes the account's own state
-- reachable in one round trip: `status` and `password_hash` are both refusals
-- this path has to make, and reading them separately would leave a gap between
-- the check and the write.
SELECT pr.*, u.email, u.name, u.status, u.password_hash
  FROM password_resets pr
  JOIN users u ON u.id = pr.user_id
 WHERE pr.token_hash = $1
   AND u.deleted_at IS NULL
   FOR UPDATE OF pr;

-- name: ConsumePasswordResets :execrows
-- Spends every unconsumed token for one account.
--
-- **One statement, called from both ends of the flow**, because the two needs
-- are the same statement and writing it twice would be two places for the
-- predicate to drift.
--
-- Requesting a reset calls it to supersede whatever was outstanding, so a fresh
-- request takes the slot and the previous link stops working at the same
-- moment — the shape DeleteOutstandingRegistration has for registrations,
-- except consumed rather than deleted: a superseded reset is evidence somebody
-- asked to recover this account twice, and the purge is what removes it later.
--
-- Completing a reset calls it to spend the token just used *and its siblings*,
-- because a recovery that leaves a second live token behind has recovered
-- nothing: whoever else requested one — including whoever the person is
-- recovering from — would still hold a working link to the account whose
-- password just changed.
UPDATE password_resets
   SET consumed_at = now()
 WHERE user_id = $1
   AND consumed_at IS NULL;

-- name: PurgeFinishedPasswordResets :execrows
-- The sweep. Removes tokens nobody used past their expiry, and spent rows past
-- the same short window a spent registration gets.
--
-- Both, because neither is a record of anything a reader needs: the audit log
-- carries that the reset happened, and the password itself is the durable
-- evidence. This table is a waiting room, not an archive.
DELETE FROM password_resets
 WHERE id IN (
     SELECT id FROM password_resets
      WHERE (consumed_at IS NULL AND expires_at <= now())
         OR (consumed_at IS NOT NULL
             AND consumed_at <= now() - make_interval(days => sqlc.arg(keep_days)::int))
      LIMIT sqlc.arg(batch)::int
 );
