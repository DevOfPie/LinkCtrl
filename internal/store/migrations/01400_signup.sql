-- +goose Up
--
-- Self-serve signup (M29): the registrations waiting on an address to be
-- proven.
--
-- One table, and no settings row. `LINKCTRL_SIGNUP_MODE` is the mode and the
-- operator is the only one who sets it (D38), so there is nothing here for a
-- session to change and nothing for a migration to seed. The mode lives in the
-- environment; this table holds only the half-finished registrations that mode
-- admits.

-- Registrations waiting on their address to be proven.
--
-- Open signup does not create an account. It creates one of these, mails a
-- link, and the user, organization and workspace are written only when the link
-- is followed — which is what makes "the account is not usable until the
-- address is verified" (D1) true in its strongest form: there is no account.
--
-- The alternative, creating a disabled user, needs a fourth `users.status`, and
-- leaves an organization and a workspace behind for every address anybody ever
-- typed. This table lapses instead.
--
-- Shape follows invitations in 01200, because it is the same kind of object: a
-- bearer-shaped secret stored only as its SHA-256, with an expiry and a
-- single-use marker.
CREATE TABLE pending_registrations (
    id            uuid        PRIMARY KEY,

    -- The address being proven. Generated lowercase column for the reason
    -- users.email_lower exists: comparison never depends on a caller
    -- remembering to fold case.
    email         text        NOT NULL,
    email_lower   text        GENERATED ALWAYS AS (lower(email)) STORED,
    name          text        NOT NULL DEFAULT '',

    -- The argon2 hash of the password chosen at the signup form, at the cost
    -- parameters the operator configured. The plaintext is never stored and
    -- never leaves the request that supplied it; this row is deleted the moment
    -- it is spent, so the hash outlives the request only until the link is
    -- followed or the window lapses.
    password_hash text        NOT NULL,

    -- SHA-256 of the token in the emailed link, exactly like invitations and
    -- sessions. A database leak hands over no verifiable registrations.
    token_hash    bytea       NOT NULL,

    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    -- Single-use. Set inside the transaction that creates the account, and the
    -- write that spends it is conditional on it still being unspent.
    consumed_at   timestamptz
);

-- Verification's only lookup. Unique because a collision would mean two
-- registrations one token completes.
CREATE UNIQUE INDEX pending_registrations_token_key
    ON pending_registrations (token_hash);

-- One outstanding registration per address. Registering again supersedes the
-- previous attempt rather than being refused: the common reason to retry is a
-- mail that never arrived, and refusing there would leave the address stuck
-- until the window lapsed.
CREATE UNIQUE INDEX pending_registrations_email_key
    ON pending_registrations (email_lower) WHERE consumed_at IS NULL;

-- The lapse sweep's index. Partial, because a consumed row is never swept by
-- age — it is deleted when it is spent.
CREATE INDEX pending_registrations_expiry_idx
    ON pending_registrations (expires_at) WHERE consumed_at IS NULL;

-- No permission is added here, and that is the milestone's decision rather than
-- an omission (D38). Who may admit accounts to this instance is not a role in
-- any organization: `owner` is per-organization, and registration provisions
-- every self-registered account an organization it owns, so a permission on the
-- owner role would have been held by every stranger who signed up. The operator
-- holds this one, through the environment, and there is nothing to grant.

-- +goose Down
DROP TABLE pending_registrations;
