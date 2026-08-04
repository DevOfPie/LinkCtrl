-- +goose Up
--
-- The instance-level principal (M45, findings F15, F31 and F36; decision D98).
--
-- D38 recorded that this product had no instance-level principal, so "the
-- instance owner" was not a thing the permission system could name. Everything
-- instance-wide was therefore guarded by a permission granted to the *owner
-- role*, which is per-organization — and since M27/M28 registration provisions
-- every self-registered account an organization it owns. On an instance running
-- LINKCTRL_SIGNUP_MODE=open, "owner" is one registration away from meaning
-- everybody.
--
-- D98 introduces the principal. It is a **user**, not an organization: binding
-- instance-wide authority to a founding organization was weighed and refused in
-- D38, because it invents a root tenant the plan does not have and every later
-- instance-wide setting inherits the concept. A person who administers the box
-- is not a tenant, and this table says so directly.
--
-- Its scopes are enumerated, never implied (D98). Nothing inherits from holding
-- one, and adding a fourth later is a decision rather than a consequence.

-- Instance-level grants: a permission held by a person over the whole instance,
-- outside every organization.
--
-- Deliberately not a role. Roles are reached through `memberships`, which is
-- keyed on an organization, so any role-shaped answer here would have had to
-- pick an organization to hang the reach off — which is the shape D38 refused.
-- A row per (user, permission) also makes "who can decide a dispute on this
-- instance" a SELECT rather than a graph walk.
CREATE TABLE instance_grants (
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    permission_id uuid NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,

    -- Who conferred it. NULL for the two paths nobody performs interactively:
    -- this migration's bootstrap, and the setup flow that claims a fresh
    -- instance. A named grantor means a person chose it, which is the fact
    -- worth keeping.
    --
    -- ON DELETE SET NULL rather than CASCADE: the grant outliving the account
    -- that made it is correct, and cascading would revoke a live reviewer
    -- because whoever appointed them left.
    granted_by uuid REFERENCES users(id) ON DELETE SET NULL,
    granted_at timestamptz NOT NULL DEFAULT now(),

    -- Real foreign keys, unlike audit_logs and destination_disputes. Those are
    -- records of the past and must stay readable after their subject is gone;
    -- this is a live authorization fact, and a grant naming a user who does not
    -- exist is not a record worth keeping, it is a permission nobody can hold.
    PRIMARY KEY (user_id, permission_id)
);

-- Who holds a given instance permission. The reviewer list reads this, and it
-- is the only direction with more than a handful of rows on either side.
CREATE INDEX instance_grants_permission_idx ON instance_grants (permission_id);

-- The three permissions D98 enumerates, plus the split of the one that existed.
--
--   instance.admin        — the principal itself. Holding it confers instance-
--                           level review on somebody else, and nothing else: it
--                           does not imply the review scopes, which is what
--                           "enumerated, not implied" means in practice.
--   destinations.decide   — act on a dispute. Split out of destinations.review,
--                           which keeps the reading half. D98's second
--                           constraint, "API access is read-only for disputes;
--                           a change requires a person", is implemented as this
--                           split with the decide half in
--                           auth.NonDelegableScopes — NOT as a branch on
--                           credential type, which the inherited Permissions
--                           rule forbids and which F104 already convicts seven
--                           places for.
--   audit.read.instance   — read the audit records of acts that belong to the
--                           instance rather than to any tenant (F36). Named to
--                           sort beside audit.read, because a reader comparing
--                           the two is the reader this permission is for.
INSERT INTO permissions (id, slug, description) VALUES
    ('00000000-0000-4000-8000-000000000217', 'instance.admin',
     'Confer and withdraw instance-level review of disputed destinations'),
    ('00000000-0000-4000-8000-000000000218', 'destinations.decide',
     'Decide a disputed destination, lifting or upholding the refusal'),
    ('00000000-0000-4000-8000-000000000219', 'audit.read.instance',
     'Read the audit log of instance-wide actions, which belong to no organization');

-- +goose StatementBegin
DO $$
DECLARE
    owner_role  uuid := '00000000-0000-4000-8000-000000000101';
    perm_review uuid := '00000000-0000-4000-8000-000000000212';
    perm_admin  uuid := '00000000-0000-4000-8000-000000000217';
    perm_decide uuid := '00000000-0000-4000-8000-000000000218';
    perm_audit  uuid := '00000000-0000-4000-8000-000000000219';
    principal   uuid;
BEGIN
    -- None of the three new permissions is granted to any role. That is the
    -- whole point of them: a role grant is what F15 is. They reach a person
    -- only through instance_grants, and only because somebody put them there.

    -- The move F15 asks for, and the only destructive statement in this file.
    --
    -- destinations.review has been granted to the owner role since 01600. It is
    -- taken away here, because "every organization owner reviews every dispute
    -- on the instance" is the finding rather than an implementation of it.
    --
    -- The direction is safe because it only ever *narrows*: the permission
    -- leaves a set that grows with every registration and lands on exactly one
    -- account. Conferring it on every current owner instead would have
    -- preserved F15 verbatim under a new mechanism, and leaving the role grant
    -- in place beside the new table would have been additive and useless.
    DELETE FROM role_permissions
     WHERE role_id = owner_role AND permission_id = perm_review;

    -- Who becomes the principal on an instance that already exists.
    --
    -- The earliest surviving account. On any instance that went through
    -- POST /api/v1/auth/setup that is the setup account, which is the only
    -- account in this product with a claim to the box rather than to a tenant:
    -- internal/auth's Register says it plainly — the first user "is trusted by
    -- construction: they had filesystem or deploy access to reach the setup
    -- page". Every other candidate is a tenant, and picking a tenant is what
    -- D38 refused.
    --
    -- If that account is not the operator's any more, the operator has
    -- filesystem access to this database and can move the row. That is a worse
    -- answer than asking, and asking is not available inside a migration; it is
    -- documented in docs/operations.md instead.
    SELECT id INTO principal
      FROM users
     WHERE deleted_at IS NULL
     ORDER BY created_at, id
     LIMIT 1;

    -- No users at all is the normal case, not an error: a fresh database is
    -- migrated before anybody has claimed it. The setup flow confers the
    -- principal in the same transaction that creates the first account, so an
    -- instance is never both claimed and unadministered.
    IF principal IS NOT NULL THEN
        INSERT INTO instance_grants (user_id, permission_id, granted_by)
        VALUES (principal, perm_admin,  NULL),
               (principal, perm_review, NULL),
               (principal, perm_decide, NULL),
               (principal, perm_audit,  NULL)
        ON CONFLICT DO NOTHING;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
--
-- Restores the role grant before dropping the table, so an instance rolled back
-- is never left with nobody able to reach the queue at all.
-- +goose StatementBegin
DO $$
BEGIN
    INSERT INTO role_permissions (role_id, permission_id)
    VALUES ('00000000-0000-4000-8000-000000000101',
            '00000000-0000-4000-8000-000000000212')
    ON CONFLICT DO NOTHING;
END
$$;
-- +goose StatementEnd

DROP TABLE instance_grants;
DELETE FROM permissions WHERE id IN (
    '00000000-0000-4000-8000-000000000217',
    '00000000-0000-4000-8000-000000000218',
    '00000000-0000-4000-8000-000000000219');
