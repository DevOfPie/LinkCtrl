-- +goose Up
--
-- A second factor (M53): TOTP, enrolment, and recovery codes.
--
-- **The two columns this milestone was waiting for already exist.**
-- `users.mfa_secret` and `users.mfa_enabled_at` have been in `00200_identity.sql`
-- since the first migration, marked `-- Phase 3.` at the time, and until M52 nothing wrote
-- them at all. M52's erasure sweep gave them their first writer and it is the one
-- that clears them; this milestone gives them the writer that sets them. So what
-- is left for a migration is the state TOTP needs and the columns cannot hold:
-- the replay guard, the recovery codes, and the step between a right password and
-- a session.
--
-- **Numbering.** `04000` was M52's, so this is the next free number rather than a
-- `002xx` that reads as related to the identity schema. That is the rule M51
-- settled after reserving `037xx` and finding it spent — take the next free
-- number, never reserve one.

-- The replay guard.
--
-- RFC 6238 divides time into 30-second steps and a code is valid for the step it
-- was computed in, which means a code observed on the wire — over the shoulder, in
-- a screenshot, in a proxy log — is usable again until that step ends. m53.md asks
-- for the refusal by name: *a code that has just succeeded cannot succeed again
-- inside its own window*.
--
-- One column rather than a table of spent codes, because the codes are ordered:
-- steps only ever increase, so "has this step already been accepted" is
-- `step <= mfa_last_step`, and a single bigint answers it for every code that will
-- ever exist. It also refuses the whole skew window below an accepted step rather
-- than only the exact code, which is stricter than replay and is the right
-- direction: after accepting step N nothing earlier than N is a fresh factor.
--
-- Nullable because an account that has never completed a second factor has no
-- last step, and zero is a real step (the Unix epoch). NULL is the absence.
ALTER TABLE users ADD COLUMN mfa_last_step bigint;

-- Recovery codes.
--
-- Ten single-use codes issued at enrolment, and the answer to the question a
-- second factor creates: the phone is gone, now what. m53.md makes recovery the
-- hard edge of this milestone — *recovery lands first or this does not land* —
-- because a second factor multiplies a lockout that M51 had only just stopped
-- being permanent.
--
-- Stored as SHA-256 and never as the code, the same `HashOpaqueToken` pattern
-- sessions, invitations, registrations and password resets use. A database leak
-- hands over no usable code, which matters more here than for most of those: this
-- one is a standing credential with no expiry, held until it is spent.
CREATE TABLE mfa_recovery_codes (
    id          uuid        PRIMARY KEY,

    -- ON DELETE CASCADE covers a hard `DELETE FROM users`, which nothing in this
    -- product performs, and that is deliberately not the mechanism relied on.
    -- M52's deletion is an `UPDATE` and fires no foreign key, so
    -- `DeleteAccountDependents` removes these rows in the deleting transaction —
    -- a recovery code is a credential that admits somebody to an account, and
    -- leaving one behind a deleted account is the same defect `password_resets`
    -- had before M52 enumerated it.
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    code_hash   bytea       NOT NULL,

    created_at  timestamptz NOT NULL DEFAULT now(),

    -- Single use. Set when the code is spent, and a spent row is kept rather than
    -- deleted: the account page counts what is left, and "you have three codes
    -- remaining" is a number somebody acts on. Regenerating deletes the set
    -- outright, spent rows included, because the previous set is then void in
    -- full and a count of leftovers from a void set would be a lie.
    used_at     timestamptz
);

-- Globally unique rather than unique per account, and the difference is not
-- cosmetic. A code is presented at the second-factor prompt alongside a pending
-- login that already names the account, so the lookup is always scoped — but a
-- collision across two accounts would mean two people holding the same secret,
-- and the index is what makes that impossible rather than improbable.
CREATE UNIQUE INDEX mfa_recovery_codes_hash_key ON mfa_recovery_codes (code_hash);

-- The unspent set for one account: what the prompt matches against and what the
-- account page counts. Partial, because a spent code is never looked up again.
CREATE INDEX mfa_recovery_codes_unused_idx
    ON mfa_recovery_codes (user_id) WHERE used_at IS NULL;

-- The step between a right password and a session.
--
-- **A table rather than a signed cookie**, which is the decision this row exists
-- to record. The credential has to be single-use and revocable inside seconds,
-- and a self-describing token is neither without a server-side record of whether
-- it has been spent — at which point the table is back and the cookie is an
-- optimisation. It is also the one place in this product where getting a state
-- machine wrong turns a login page into an authentication bypass, so the state is
-- in the database where a transaction can hold it.
--
-- Shape follows `password_resets` (03900), which follows `pending_registrations`
-- (01400), which follows `invitations` (01200): a bearer-shaped secret stored only
-- as its SHA-256, with an expiry and a single-use marker. The fourth instance of
-- one pattern, modelled rather than invented.
CREATE TABLE mfa_pending_logins (
    id          uuid        PRIMARY KEY,

    -- Same reasoning as `mfa_recovery_codes.user_id`, and shorter-lived: these
    -- rows lapse in minutes. `DeleteAccountDependents` still takes them, because
    -- "it expires soon" is not a property a deletion path should depend on.
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    token_hash  bytea       NOT NULL,

    -- Carried from the request that verified the password, so the session the
    -- second factor mints records where the sign-in came from rather than where
    -- the second post came from. In practice they are the same browser; recording
    -- the first is what makes that a fact rather than an assumption.
    --
    -- `ip_prefix`, never an address. The privacy stance is inherited and this is
    -- not where it bends: /24 for IPv4 and /48 for IPv6, exactly as `sessions`
    -- stores it, because these rows become a session's columns verbatim.
    ip_prefix   text,
    user_agent  text,

    created_at  timestamptz NOT NULL DEFAULT now(),

    -- Minutes, not hours. m53.md asks for the TTL to be a number a test asserts,
    -- and the number is short because the window is a person reading six digits
    -- off a phone that is already in their hand.
    expires_at  timestamptz NOT NULL,

    -- Single-use, and consumed on success only. A wrong code does not spend the
    -- pending login — a typo would otherwise cost a full re-authentication — and
    -- what bounds guessing instead is the account's own lockout counter, which
    -- failed second-factor attempts increment exactly as failed passwords do.
    consumed_at timestamptz
);

CREATE UNIQUE INDEX mfa_pending_logins_token_key ON mfa_pending_logins (token_hash);

-- The purge reads this: lapsed and spent rows, oldest first. The same sweep shape
-- `password_resets` and `pending_registrations` have, for the same reason — a
-- waiting room with no sweep is a table that grows forever with nothing watching
-- it.
CREATE INDEX mfa_pending_logins_expiry_idx ON mfa_pending_logins (expires_at);

-- +goose Down
DROP TABLE mfa_pending_logins;
DROP TABLE mfa_recovery_codes;
ALTER TABLE users DROP COLUMN mfa_last_step;
