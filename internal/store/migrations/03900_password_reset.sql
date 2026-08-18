-- +goose Up
--
-- Account recovery (M51): a forgotten password stops being permanent.
--
-- Until this table existed, the only route back into an account whose password
-- was lost was an operator rewriting an argon2 hash by hand — true of every
-- account on every instance, including the one that administers the box
-- (finding F141). This is the missing mechanism, and it is small because every
-- part it needs already shipped: an opaque-token-plus-hash pattern used twice,
-- an outbox, a renderer, and argon2 rehashing on `POST /account/password`.
--
-- Shape follows `pending_registrations` (01400), which follows `invitations`
-- (01200), because it is the same kind of object for the third time: a
-- bearer-shaped secret stored only as its SHA-256, with an expiry and a
-- single-use marker. Modelled rather than invented, so "hashed like a session
-- token" stays a fact about the code and not a claim in a comment.
--
-- **Numbering.** The identity schema is `00200`; that is an area marker and not
-- a free band, so recovery takes the next free number rather than a `002xx`
-- that reads as related. m51.md reserved `037xx` when it was written and M50
-- and M50.5 spent `03700` and `03800` before it was read — the amendment is in
-- decisions.md under M51, and the rule it settled on is *do not reserve a
-- number; take the next free one*.
CREATE TABLE password_resets (
    id          uuid        PRIMARY KEY,

    -- The account being recovered. An outstanding token cannot outlive its
    -- user: there is no route by which a reset for a deleted account could still
    -- be consumed, because the row goes with the account.
    --
    -- **Two mechanisms, and this comment named only the first until M52.** ON
    -- DELETE CASCADE covers a hard `DELETE FROM users`, which nothing in this
    -- product performs. What M52 added is a *soft* delete — `deleted_at` and
    -- `status = 'deleted'` on a row that stays — and a soft delete fires no
    -- foreign key, so the cascade alone would have left a live password-setting
    -- token behind an account nobody can sign into. `DeleteAccountDependents`
    -- removes these rows in the deleting transaction for exactly that reason.
    --
    -- The address is deliberately *not* copied here, unlike
    -- `pending_registrations.email`. That table has no user to point at — it
    -- exists precisely because no account does yet — where this one always has
    -- one, and duplicating the address would create a second copy of personal
    -- data with its own way of going stale.
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- SHA-256 of the token in the emailed link, exactly like invitations,
    -- registrations and sessions. A database leak hands over no usable reset
    -- *from this table* — the same qualifier the other two carry, and for the
    -- same reason (F32): the token is also rendered into the mail body, which
    -- since 03200 is blanked when the outbox row reaches a terminal status.
    --
    -- What a token pulled out of a still-pending row buys here is larger than
    -- for a registration and is worth saying plainly: it sets the password. The
    -- window is the delivery rather than the retention window, and the row's own
    -- expiry bounds the rest.
    token_hash  bytea       NOT NULL,

    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,

    -- Single-use. Set inside the transaction that writes the new password, and
    -- the write that spends it is conditional on it still being unspent.
    --
    -- Also set in bulk by a *successful* reset, which consumes every other
    -- outstanding token for the account: recovery that leaves a second live
    -- token behind has recovered nothing.
    consumed_at timestamptz
);

-- The reset lookup, and the only one that takes a token. Unique because a
-- collision would mean two accounts one token recovers.
CREATE UNIQUE INDEX password_resets_token_key
    ON password_resets (token_hash);

-- Every unconsumed token for one account. Read by the request path, which
-- supersedes whatever was outstanding, and by the consume path, which spends
-- the siblings of the token it just used.
--
-- Not unique, unlike `pending_registrations_email_key`. Superseding is done by
-- an explicit statement rather than by a constraint, because the bulk consume a
-- successful reset performs has to be able to touch several rows at once — a
-- unique index would make the state it passes through illegal.
CREATE INDEX password_resets_user_idx
    ON password_resets (user_id) WHERE consumed_at IS NULL;

-- The purge's index. Partial on unconsumed rows so the hourly sweep is an index
-- range and not a sequential scan, which is what the milestone asked for by
-- name. A consumed row is swept by `consumed_at` and is rare enough that the
-- user index above carries that half.
CREATE INDEX password_resets_expiry_idx
    ON password_resets (expires_at) WHERE consumed_at IS NULL;

-- No permission is added here, and that is not an omission. Recovery is
-- performed by somebody holding no credential at all — that is the whole point
-- of it — so there is no principal for a grant to attach to and nothing for the
-- 00800 insert-and-grant pattern to insert. The bound is the mailbox, and the
-- limiter shared with `POST /login`.

-- +goose Down
DROP TABLE password_resets;
