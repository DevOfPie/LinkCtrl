-- +goose Up
--
-- Invitations (M27), and the first table Phase 2 adds that a person's account
-- can be created by.
--
-- Live and typed, not a dormant 00600 jsonb table: the feature is arriving in
-- the same commit, so the rule those tables follow — structure stays in jsonb
-- until something reads it — no longer applies. Every column here is read by
-- redemption on the request that decides whether somebody joins.
--
-- Shape follows sessions in 00200, because an invite is the same kind of object:
-- a bearer-shaped secret that must not be recoverable from a database dump, with
-- an expiry and a revocation. Only the SHA-256 of the token is stored, and the
-- raw value exists exactly once — in the response that created it.

CREATE TABLE invitations (
    id              uuid        PRIMARY KEY,
    organization_id uuid        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    -- The address this invite is bound to (decision D27). Redemption compares
    -- the redeeming account's address against it, so an invite is not a bearer
    -- credential: a forwarded link cannot add whoever picked it up.
    --
    -- Generated lowercase column for the same reason users.email_lower exists —
    -- comparison never depends on a caller remembering to fold case.
    email           text        NOT NULL,
    email_lower     text        GENERATED ALWAYS AS (lower(email)) STORED,

    -- The role the membership is created with. At or below the inviter's own
    -- rank, enforced in the service (decision D28); the foreign key is what
    -- stops the id naming something that is not a role at all.
    role_id         uuid        NOT NULL REFERENCES roles(id),

    -- SHA-256 of the token, exactly like sessions.token_hash. A database leak
    -- hands over no redeemable invites *from this table*.
    --
    -- That qualifier is finding F32, corrected in place rather than left as a
    -- sentence a reader would have believed. The token also reaches mail_outbox,
    -- rendered into the message body, because that is how it gets to the person
    -- invited. Since 03200 the outbox blanks a body the moment the row reaches a
    -- terminal state, so the plaintext lives from enqueue to delivery instead of
    -- for the outbox's 30-day retention window — but "from enqueue to delivery"
    -- is not "never", and an instance whose relay is down holds it for as long as
    -- the invitation itself is redeemable. docs/SECURITY.md states that bound.
    token_hash      bytea       NOT NULL,

    -- Who sent it. Nullable so deleting a user does not delete the invitations
    -- they sent, which would take the audit trail's counterpart with it.
    invited_by      uuid        REFERENCES users(id) ON DELETE SET NULL,

    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Set at creation from LINKCTRL_INVITE_TTL (decision D29). The clock starts
    -- here rather than at send: mail leaves through the outbox on the
    -- scheduler's tick (D23), so there is no send moment to start it from.
    expires_at      timestamptz NOT NULL,

    -- Single-use and revocable are these two columns. Both NULL is the only
    -- state in which an invite can be redeemed, and redemption sets the first
    -- inside the transaction that creates the membership.
    revoked_at      timestamptz,
    redeemed_at     timestamptz,
    redeemed_by     uuid        REFERENCES users(id) ON DELETE SET NULL
);

-- Redemption's only lookup. Unique because a collision would mean two invites
-- one token redeems, and the index is what makes that unrepresentable rather
-- than unlikely.
CREATE UNIQUE INDEX invitations_token_key ON invitations (token_hash);

-- The list an administrator reads, newest first.
CREATE INDEX invitations_org_idx ON invitations (organization_id, created_at DESC);

-- At most one outstanding invite per address per organization.
--
-- Not a nicety. Without it, inviting the same address twice leaves two
-- redeemable tokens, and revoking the one the administrator can see leaves the
-- other one live — a revocation that does not revoke. The index makes that
-- unrepresentable rather than unlikely.
--
-- The predicate cannot mention expiry, because `now()` is not immutable and
-- Postgres will not index on it. So an expired invite still occupies the slot,
-- and the service revokes it on the way to issuing a replacement; re-inviting
-- after a lapse therefore works, while re-inviting over a *live* invite is
-- refused rather than silently superseding a link somebody is holding.
CREATE UNIQUE INDEX invitations_outstanding_email_key
    ON invitations (organization_id, email_lower)
    WHERE revoked_at IS NULL AND redeemed_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS invitations;
