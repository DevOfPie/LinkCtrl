-- +goose Up
--
-- The bridge an add-on's authentication assertion crosses (M65).
--
-- **This table is the whole of "account linking is explicit, never guessed".**
-- An add-on that holds `session.mint` tells the host *somebody with this external
-- subject authenticated*; the host answers by looking the subject up here, and a
-- subject with no row mints nothing. There is no fallback. In particular there is
-- **no match on the email address the assertion carries**, which is the classic
-- account-takeover shape: an identity provider an operator installed for one
-- purpose could otherwise assert `email: owner@instance.example` and be believed,
-- and every published version of that attack begins with a product that matched
-- on the address because it was the field both sides happened to have.
--
-- m65.md names that absence so it is a decision rather than an accident, and this
-- table is where the decision is enforced rather than promised: the lookup takes
-- (addon, issuer, subject) and there is no statement in this product that resolves
-- an assertion by any other column.
--
-- **Numbering.** `04400` is the last number in this directory, so this is the next
-- free one — the rule M51 settled after reserving `037xx` and finding it spent.
CREATE TABLE addon_identity_links (
    id          uuid        PRIMARY KEY,

    -- ON DELETE CASCADE covers a hard `DELETE FROM users`, which nothing in this
    -- product performs. The mechanism that actually runs is
    -- `DeleteAccountDependents` in internal/store/query/accounts.sql, which M65 adds
    -- a ninth CTE to: M52's deletion is an `UPDATE`, so no foreign key fires, and a
    -- link left behind a deleted account is a standing credential that admits
    -- somebody to it with no password — exactly the `password_resets` defect that
    -- statement was written out for, and exactly why M53 put its two tables in the
    -- statement in the milestone that created them rather than deferring it.
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Which add-on vouched. Part of the key rather than context, because two
    -- add-ons are two authorities: an add-on installed to authenticate contractors
    -- must not be able to assert a subject that another add-on linked, even if
    -- both name the same issuer string. The name is the directory name, which the
    -- host proves against the manifest at load.
    addon       text        NOT NULL,

    -- The provider, as the provider names itself — an OIDC `iss`. Part of the key
    -- because a subject identifier is only unique within an issuer; `sub` is a
    -- small integer at more than one well-known provider.
    issuer      text        NOT NULL,

    -- The provider's stable identifier for the person. Never the email address:
    -- an address is a display fact that a provider lets people change, and a
    -- mapping keyed on one silently re-points when they do.
    subject     text        NOT NULL,

    created_at  timestamptz NOT NULL DEFAULT now(),

    -- When this link last minted a session. Read by nobody in this milestone and
    -- written on every mint, because the question an operator asks about a
    -- credential they are considering removing is when it was last used, and a
    -- column that starts being written later cannot answer it for the past.
    last_used_at timestamptz
);

-- One account per (addon, issuer, subject), enforced rather than assumed. Without
-- it two rows could name two accounts for one external identity and the lookup
-- would sign somebody in as whichever row Postgres returned first.
CREATE UNIQUE INDEX addon_identity_links_subject_key
    ON addon_identity_links (addon, issuer, subject);

-- The other direction: every provider one account has connected. What a person
-- sees on their own account page, and what the deletion statement removes.
CREATE INDEX addon_identity_links_user_idx ON addon_identity_links (user_id);

-- +goose Down
DROP TABLE addon_identity_links;
