-- Self-serve signup: the registrations waiting on an address to be proven
-- (M29). The mode itself is `LINKCTRL_SIGNUP_MODE` and is never read from the
-- database (D38), so nothing here answers what the instance admits.

-- name: CreatePendingRegistration :one
INSERT INTO pending_registrations (
    id, email, name, password_hash, token_hash, expires_at
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: DeleteOutstandingRegistration :execrows
-- Clears whatever is outstanding for an address so a fresh attempt can take the
-- slot.
--
-- Superseding rather than refusing, because the ordinary reason somebody
-- registers twice is that the first mail never arrived. The old token stops
-- working at the same moment, which is what makes this safe: there is never
-- more than one live link per address.
DELETE FROM pending_registrations
 WHERE email_lower = lower(sqlc.arg(email)::text)
   AND consumed_at IS NULL;

-- name: GetPendingRegistrationByTokenHash :one
-- Verification's lookup, inside the transaction that spends the row.
--
-- FOR UPDATE, so two clicks on the same link serialize and the second sees the
-- row the first consumed.
SELECT * FROM pending_registrations
 WHERE token_hash = $1
   FOR UPDATE;

-- name: ConsumePendingRegistration :execrows
-- Spends a registration. Conditional on it still being unspent, so this could
-- not succeed twice even without the lock above; zero rows rolls the
-- transaction back.
UPDATE pending_registrations
   SET consumed_at = now()
 WHERE id = $1
   AND consumed_at IS NULL;

-- name: PurgeLapsedRegistrations :execrows
-- The sweep. Removes registrations nobody completed, and consumed rows whose
-- account has long since been created.
--
-- Both, because neither is a record of anything: an account that exists is
-- evidence enough that its address was proven, and the audit log carries what
-- happened. This table is a waiting room, not an archive — the one shape it
-- must not have is the unbounded growth D5 and M21 exist to stop repeating.
DELETE FROM pending_registrations
 WHERE (consumed_at IS NULL AND expires_at <= now())
    OR (consumed_at IS NOT NULL AND consumed_at <= now() - make_interval(days => sqlc.arg(keep_days)::int));

-- name: CountUsersByEmail :one
-- Whether an address already has an account, for the signup form.
--
-- Counted rather than selected: the caller needs the answer and nothing else,
-- and returning the row would put somebody else's name and hash in a variable
-- that only ever gets compared against zero.
SELECT count(*) FROM users WHERE email_lower = lower(sqlc.arg(email)::text) AND deleted_at IS NULL;
