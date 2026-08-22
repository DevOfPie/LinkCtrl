-- A second factor (M53): TOTP, enrolment, recovery codes, and the step between
-- a right password and a session.
--
-- Three groups of statements. The enrolment pair writes `users.mfa_secret` and
-- `users.mfa_enabled_at` — the columns `00200_identity.sql` has carried since the
-- first migration and only M52's erasure sweep has ever touched. The recovery-code
-- statements are a hashed single-use credential, the fifth thing in this schema
-- shaped that way. And the pending-login statements are the state machine that
-- makes "no session token exists until the second factor is verified" a property
-- of the database rather than of a handler's control flow.

-- name: GetUserMFA :one
-- Everything the second factor needs about one account, and nothing else.
--
-- Not `SELECT *`: the enrolment and challenge paths have no business holding a
-- password hash, and a narrow row is what keeps that true as columns are added.
SELECT id,
       email,
       name,
       status,
       mfa_secret,
       mfa_enabled_at,
       mfa_last_step
  FROM users
 WHERE id = $1
   AND deleted_at IS NULL;

-- name: LockUserMFA :one
-- The same row, locked for the rest of the transaction.
--
-- Every write below reads through this first. Enrolment, disabling and accepting
-- a code all read a column and then write it, and two of those are the difference
-- between a second factor existing and not — a check-then-act on `mfa_enabled_at`
-- is how an abandoned enrolment and a live one end up in the same account.
SELECT id,
       email,
       name,
       status,
       mfa_secret,
       mfa_enabled_at,
       mfa_last_step
  FROM users
 WHERE id = $1
   AND deleted_at IS NULL
   FOR UPDATE;

-- name: EnableUserMFA :execrows
-- Enrolment, committed.
--
-- **`mfa_enabled_at` and `mfa_secret` are written together and only together**,
-- which is m53.md's *half-enrolled is not a state this product has* stated as a
-- single UPDATE. The secret is not parked on the row while the person fetches
-- their phone: it is held by the enrolment session and reaches the database only
-- in the statement that also says the second factor is on, and only after a code
-- computed from it has verified.
--
-- `mfa_enabled_at IS NULL` in the predicate rather than checked beforehand, so
-- enrolling an account that is already enrolled affects no rows instead of
-- silently replacing a working secret with a different one. Two tabs finishing the
-- same enrolment is the ordinary way that happens.
UPDATE users
   SET mfa_secret     = @secret,
       mfa_enabled_at = now(),
       mfa_last_step  = @first_step,
       updated_at     = now()
 WHERE id = @user_id
   AND deleted_at IS NULL
   AND mfa_enabled_at IS NULL;

-- name: DisableUserMFA :execrows
-- Taking the second factor away.
--
-- Everything at once, because m53.md asks for exactly that: *clearing
-- `mfa_enabled_at` clears the secret and every unused recovery code in the same
-- transaction*. The codes are the caller's second statement — this one is the
-- account row — and both are in one transaction, which is what makes "the account
-- has no second factor" a state with no intermediate.
--
-- `mfa_last_step` goes too. It is meaningless without a secret, and leaving it
-- would mean a later enrolment inherited a replay floor from a secret that no
-- longer exists — an account that re-enrolled would find its first codes refused
-- until the clock caught up.
UPDATE users
   SET mfa_secret     = NULL,
       mfa_enabled_at = NULL,
       mfa_last_step  = NULL,
       updated_at     = now()
 WHERE id = @user_id
   AND deleted_at IS NULL
   AND mfa_enabled_at IS NOT NULL;

-- name: AcceptMFAStep :execrows
-- The replay guard, applied as a write rather than as a check.
--
-- `mfa_last_step < @step` is the whole mechanism. A code from a step already
-- accepted updates no rows, and the caller reads zero as *refused*, so two
-- requests presenting the same code race each other into the same statement and
-- exactly one wins. Doing it as a read-then-write would leave a window in which
-- both saw the old value, which for a replay guard is the only window that
-- matters.
--
-- `mfa_last_step IS NULL` is the first acceptance after an enrolment that
-- pre-seeded nothing, and is kept for completeness — `EnableUserMFA` stamps the
-- enrolling step, so in practice the column is never NULL while a secret exists.
UPDATE users
   SET mfa_last_step = @step,
       updated_at    = now()
 WHERE id = @user_id
   AND deleted_at IS NULL
   AND mfa_enabled_at IS NOT NULL
   AND (mfa_last_step IS NULL OR mfa_last_step < @step);

-- name: InsertMFARecoveryCode :exec
-- One code of a set. Called ten times inside the transaction that issues them,
-- rather than as one multi-row insert, because the hashes are computed one at a
-- time and a loop over a prepared statement is what sqlc gives without a bespoke
-- array parameter for a fixed ten rows.
INSERT INTO mfa_recovery_codes (id, user_id, code_hash)
VALUES (@id, @user_id, @code_hash);

-- name: DeleteMFARecoveryCodes :execrows
-- Every code for an account, spent ones included.
--
-- Both callers want exactly this. Regenerating voids the previous set in full, so
-- keeping the spent rows would leave a count of leftovers from a set that no
-- longer opens anything. Disabling removes the account's last credential of this
-- kind, and m53.md names it in the same breath as clearing the secret.
DELETE FROM mfa_recovery_codes WHERE user_id = @user_id;

-- name: SpendMFARecoveryCode :execrows
-- Match a presented code and spend it, in one statement.
--
-- Scoped by `user_id` as well as by the hash. The pending login already names the
-- account, and matching on the hash alone would make this table a global lookup —
-- correct today, because the hash index is unique, and one refactor away from a
-- code minted for one account opening another.
--
-- `used_at IS NULL` is in the predicate for the reason the replay guard's
-- comparison is: single use has to be decided by the statement that spends it, or
-- two simultaneous presentations of the same code both pass their check.
UPDATE mfa_recovery_codes
   SET used_at = now()
 WHERE user_id = @user_id
   AND code_hash = @code_hash
   AND used_at IS NULL;

-- name: CountUnusedMFARecoveryCodes :one
-- What the account page shows. A number somebody acts on: three left is a prompt
-- to regenerate, and zero with a lost phone is a conversation with the operator.
SELECT count(*)::bigint
  FROM mfa_recovery_codes
 WHERE user_id = $1
   AND used_at IS NULL;

-- name: CreateMFAPendingLogin :one
-- The credential the browser holds between a right password and a session.
--
-- Returned in full so the caller can assert the expiry it asked for rather than
-- recompute it from its own clock — the TTL m53.md wants a test to hold is the
-- one the database wrote.
--
-- `minted_by_addon` and `minted_by_issuer` (04600) are null for a password post
-- and set when an add-on's assertion is what stopped here. They are carried
-- through the prompt because the session this row becomes is minted by
-- `CompleteSecondFactor`, which would otherwise have no way to say who vouched.
INSERT INTO mfa_pending_logins (id, user_id, token_hash, ip_prefix, user_agent,
                                expires_at, minted_by_addon, minted_by_issuer)
VALUES (@id, @user_id, @token_hash, @ip_prefix, @user_agent,
        @expires_at, @minted_by_addon, @minted_by_issuer)
RETURNING *;

-- name: LockMFAPendingLogin :one
-- The pending login behind a presented token, locked.
--
-- Joined to `users` because every consumer needs both halves and the alternative
-- is two round trips with the account's state changing between them. `FOR UPDATE
-- OF p` locks the pending row and not the user row: the user row is locked
-- separately by the paths that write it, and locking it here would serialise every
-- second-factor attempt against every other write to the account.
--
-- Nothing is filtered out. Expired, consumed, an account that stopped being active
-- while the prompt was open — each is a refusal the caller makes, and each is the
-- same refusal to whoever is looking at the form. Filtering here would collapse
-- them into not-found, which is the same answer, and would cost the tests their
-- ability to tell the five apart.
SELECT p.id,
       p.user_id,
       p.ip_prefix,
       p.user_agent,
       p.created_at,
       p.expires_at,
       p.consumed_at,
       -- Read by CompleteSecondFactor, which is where the session an add-on's
       -- assertion produced actually comes into existence.
       p.minted_by_addon,
       p.minted_by_issuer,
       u.email,
       u.name,
       u.status,
       u.mfa_secret,
       u.mfa_enabled_at,
       u.mfa_last_step
  FROM mfa_pending_logins p
  JOIN users u ON u.id = p.user_id
 WHERE p.token_hash = @token_hash
   AND u.deleted_at IS NULL
   FOR UPDATE OF p;

-- name: ConsumeMFAPendingLogin :execrows
-- Spend it. Single use, decided by the statement rather than by the caller, for
-- the third time in this file and for the same reason.
UPDATE mfa_pending_logins
   SET consumed_at = now()
 WHERE id = @id
   AND consumed_at IS NULL;

-- name: DeleteMFAPendingLoginsFor :execrows
-- Every outstanding pending login for an account.
--
-- Two callers. A fresh password post supersedes whatever was outstanding, so there
-- is never more than one live prompt per account and somebody who abandoned a tab
-- is not sharing their window with it. And disabling the second factor takes them
-- all, because a prompt that outlives the factor it was prompting for is a
-- credential with nothing left to check.
DELETE FROM mfa_pending_logins WHERE user_id = @user_id;

-- name: PurgeFinishedMFAPendingLogins :execrows
-- The sweep. Lapsed rows and spent ones, in bounded batches, from the hourly
-- maintenance pass that already purges finished registrations and password
-- resets.
--
-- No retention window, unlike those two. A spent pending login is evidence of
-- nothing — the session it minted is the record, and the audit trail carries the
-- rest — where a spent registration and a spent reset are each the only trace that
-- an address was proven.
DELETE FROM mfa_pending_logins
 WHERE id IN (
     SELECT id
       FROM mfa_pending_logins
      WHERE expires_at < now() OR consumed_at IS NOT NULL
      ORDER BY expires_at
      LIMIT sqlc.arg(batch)::int
 );
