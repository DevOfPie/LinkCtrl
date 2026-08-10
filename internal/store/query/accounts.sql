-- Account deletion and subject erasure (M52).
--
-- Two operations that look like one and are deliberately kept apart. **Deletion**
-- is interactive, immediate and one transaction: it ends every route into the
-- account and releases the address. **Erasure** is the hourly sweep that scrubs
-- what deletion could not reach — every record *about* this person that ending
-- the account does not remove. *No foreign key to `users`* was that criterion
-- while the sweep touched two tables and is not the criterion now: it reaches
-- four, two of which do carry one. The enumeration lives on
-- `EraseDeletedAccounts` below, beside the statement, and nowhere else.
--
-- The statements below are the whole of the database side. `04000` adds the one
-- index the sweep reads and nothing else; the columns have existed, unwritten,
-- since `00200_identity.sql`.

-- name: LockUserForDeletion :one
-- The account being deleted, locked for the rest of the transaction.
--
-- `deleted_at IS NULL` is in the predicate rather than checked afterwards, so a
-- second deletion of the same account is not-found instead of a second pass over
-- rows the first one already took. Two browsers pressing the button at once is
-- the ordinary way that happens.
SELECT * FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: LockOrganizationsSolelyOwnedBy :many
-- The organizations this account owns alone, locked before they are counted.
--
-- The refusal M28.5 makes from the other side. `team.guardOwnerSet` blocks
-- removing or demoting an organization's last owner; this blocks deleting the
-- *account* that is one, because otherwise the rule is bypassable by leaving
-- through a different door.
--
-- **Organization-wide owner memberships only**, the sentence
-- `LockOrganizationOwners` states and for the same reason: a workspace-scoped
-- owner membership is ownership of one workspace, so counting it would either
-- hide a sole owner or invent one.
--
-- The owner rows are locked before the count is taken, so this is a rule rather
-- than a check-then-act — a second administrator promoting or removing an owner
-- blocks until this transaction ends. Ordered by membership id inside the
-- locking CTE so two accounts deleting themselves at once take the rows in the
-- same order and cannot deadlock. MATERIALIZED because the lock has to be taken
-- once, on the rows this counts, rather than folded into the outer query by the
-- planner.
WITH mine AS MATERIALIZED (
    SELECT m.organization_id
      FROM memberships m
      JOIN roles r ON r.id = m.role_id
     WHERE m.user_id = @user_id
       AND m.workspace_id IS NULL
       AND r.slug = 'owner'
       AND r.organization_id IS NULL
),
owners AS MATERIALIZED (
    SELECT m.id, m.organization_id
      FROM memberships m
      JOIN roles r ON r.id = m.role_id
     WHERE m.organization_id IN (SELECT organization_id FROM mine)
       AND m.workspace_id IS NULL
       AND r.slug = 'owner'
       AND r.organization_id IS NULL
     ORDER BY m.id
       FOR UPDATE OF m
)
SELECT o.id, o.name, o.slug
  FROM organizations o
  JOIN owners w ON w.organization_id = o.id
 WHERE o.deleted_at IS NULL
 GROUP BY o.id, o.name, o.slug
HAVING count(*) = 1
 ORDER BY o.name;

-- name: DeleteAccountDependents :one
-- Everything hanging off the account that must not outlive it, removed in one
-- statement and counted.
--
-- **Written out because a soft delete fires no foreign key.** All eight tables
-- below declare `ON DELETE CASCADE` against `users`, and every one of those
-- clauses triggers on `DELETE`; the account row is kept — that is what
-- `anonymized_at` marks and what the partial `users_email_key` is shaped for —
-- so the cascade never runs and these statements are what stands in for it.
--
-- Four of them are the tables M52 enumerates: `memberships`, `sessions`,
-- `api_keys`, `notifications`. Four more are here because leaving them would
-- falsify a claim the schema already makes:
--
--   * `password_resets`, whose own comment (03900) says *"there is no route by
--     which a reset for a deleted account could still be consumed, because the
--     row is gone with the account"*. Under a soft delete the row is not gone,
--     and it is the one credential in this schema that sets a password.
--   * `instance_grants`, whose own comment (03400) says a grant naming a user
--     who does not exist *"is not a record worth keeping, it is a permission
--     nobody can hold"*. The instance principal cannot reach this statement at
--     all — deleting it is refused — but a delegated dispute reviewer can.
--   * `mfa_recovery_codes` and `mfa_pending_logins` (04100), added by M53 and
--     added *by* M53 rather than deferred, because M53 is what creates them: a
--     recovery code is a standing credential that admits somebody to an account
--     with no password, and a pending login is one that mints a session. Both
--     are the `password_resets` defect in a new table, and shipping the tables
--     without the statements would have reintroduced it in the same phase that
--     closed it.
--
-- The counts come back so the caller can log what went, and so a test can assert
-- the statement reached each table rather than assert it did not error.
WITH removed_memberships AS (
    DELETE FROM memberships m WHERE m.user_id = @account_id RETURNING 1
), removed_sessions AS (
    DELETE FROM sessions s WHERE s.user_id = @account_id RETURNING 1
), removed_api_keys AS (
    DELETE FROM api_keys k WHERE k.user_id = @account_id RETURNING 1
), removed_notifications AS (
    DELETE FROM notifications n WHERE n.user_id = @account_id RETURNING 1
), removed_password_resets AS (
    DELETE FROM password_resets pr WHERE pr.user_id = @account_id RETURNING 1
), removed_instance_grants AS (
    DELETE FROM instance_grants ig WHERE ig.user_id = @account_id RETURNING 1
), removed_recovery_codes AS (
    DELETE FROM mfa_recovery_codes rc WHERE rc.user_id = @account_id RETURNING 1
), removed_pending_logins AS (
    DELETE FROM mfa_pending_logins pl WHERE pl.user_id = @account_id RETURNING 1
)
SELECT (SELECT count(*) FROM removed_memberships)::bigint      AS memberships,
       (SELECT count(*) FROM removed_sessions)::bigint         AS sessions,
       (SELECT count(*) FROM removed_api_keys)::bigint         AS api_keys,
       (SELECT count(*) FROM removed_notifications)::bigint    AS notifications,
       (SELECT count(*) FROM removed_password_resets)::bigint  AS password_resets,
       (SELECT count(*) FROM removed_instance_grants)::bigint  AS instance_grants,
       (SELECT count(*) FROM removed_recovery_codes)::bigint   AS mfa_recovery_codes,
       (SELECT count(*) FROM removed_pending_logins)::bigint   AS mfa_pending_logins;

-- name: SoftDeleteUser :execrows
-- The deletion itself: the first writer `status` and `deleted_at` have ever had.
--
-- `status = 'deleted'` and `deleted_at` are set together and only together. The
-- timestamp is what every query in this product filters on and what releases the
-- address through the partial unique index; the status is what a person reading
-- the row sees. Setting one without the other would make the two disagree about
-- the same fact, which is the state the CHECK constraint cannot catch.
--
-- `password_hash` is **not** cleared here. Scrubbing is the erasure pass's, and
-- clearing it early would take the one field that makes a mistaken deletion
-- recoverable by an operator inside the sweep's window, while ending no access
-- that the session and key rows going in the same transaction have not already
-- ended.
UPDATE users
   SET status     = 'deleted',
       deleted_at = now(),
       updated_at = now()
 WHERE id = @user_id
   AND deleted_at IS NULL;

-- name: EraseDeletedAccounts :many
-- The erasure pass. One batch, one statement, one transaction.
--
-- **What it scrubs is what deletion could not reach**, and there are two ways a
-- row gets there. Two tables carry an address snapshot and **no** foreign key to
-- `users` at all, because a record of the past that vanishes with its subject is
-- not a record: `audit_logs` and `destination_disputes`. Two more do have one and
-- are still out of reach — `notifications` (`00600:127`) and `invitations`
-- (`01200:62`) — because the row belongs to a *different* person, so ending this
-- account was never going to remove it and no retention window expires it.
--
-- *No foreign key* was the criterion when this pass scrubbed two tables. It is
-- not the criterion now and saying so was wrong for a while: what finds all four
-- is **a record about this person that ending the account does not remove**.
--
--   * `audit_logs.actor_label` (`00600:137,150`). An audit trail that vanishes
--     with its actor is not an audit trail.
--   * `destination_disputes.created_by_label` **and** `decided_by_label`
--     (`01600:64,68`). Two snapshots, not one: an account is as identifiable as
--     the moderator of a dispute as it is as the filer of one, and F44 names this
--     as the second table with no deletion path of any kind.
--   * `audit_logs.metadata`'s `"email"` key (F177). **Seven** writers, counted
--     against the tree on 2026-08-09 rather than recalled: `invite.go:437`
--     (`invitation.created`) and `:860` (`invitation.redeemed`),
--     `team/member.go:197`, `:258` and `:386` (`member.role_changed`,
--     `member.removed`, `member.added`), and `instance/instance.go:642`
--     (`instance.principal_moved`) and `:677` (the review grants). The address
--     there is usually the *subject* of somebody else's action, so scrubbing it
--     edits a record whose actor is still here — weighed and taken, because an
--     erasure that reaches the label and stops at the detail one column over has
--     not erased the person.
--
--     Matched on the value, because there is no foreign key to match on: the
--     column is jsonb and the address in it is a snapshot. Case-folded on both
--     sides, since `invitation.created` stores what the administrator typed and
--     that need not be the case the account was registered in. The accepted cost
--     is a sequential scan of a partitioned table — the largest thing in this
--     schema after analytics — paid once per batch, and only when there is a
--     batch: see `HAVING count(*) > 0` on `batch` below, which is what makes an
--     idle pass free rather than hourly.
--
--     Written by the **same** UPDATE as `actor_label` rather than by a second
--     one, and that is not a tidiness choice — see the note on `batch`.
--   * `audit_logs.metadata`'s `"from"` **array** (F189). One writer:
--     `instance/instance.go:642` puts the outgoing principals' addresses in an
--     array beside the `"email"` key it also writes, so a prior instance
--     principal who later deletes their account kept an address one key over
--     from the one that was scrubbed. The scalar predicate above is the only
--     shape F177 specified and an array is a different one, which is why this
--     was a second finding rather than an oversight in the first.
--
--     Rewritten element by element rather than dropped: the array says how many
--     principals the role moved away from, and losing a member of it loses that
--     count. Order is held by `WITH ORDINALITY`, because the addresses are the
--     record of a single act and the sequence is part of what it says. A `from`
--     that is not an array is left alone rather than erroring, so a record
--     written by hand cannot fail the whole batch.
--   * `notifications.data`'s `"email"` key **and the title beside it** (F188).
--     `invite/invite.go:973` tells the inviter that their invitation was
--     accepted, and both the detail and the sentence carry the address of the
--     person who accepted. The row belongs to the *inviter*, so deleting the
--     erased account's own notifications never reached it and nothing expires
--     it — notifications are scoped to a reader, not swept by age.
--
--     The title is rewritten by `replace` against `data->>'email'` rather than
--     against the batch, and that is what keeps this out of the business of
--     knowing how the sentence is worded: the two came from one value at one
--     call site, so the address in the title is exactly the string the detail
--     holds. Both CASEs read the pre-update row, so the title still finds the
--     address after the same statement has replaced it in `data`.
--
--     An **outstanding** invitation is left alone for the reason stated below,
--     and this is deliberately the other answer: a notification is a record
--     *about* a person delivered to somebody else, which is the same thing
--     `audit_logs.metadata` is, and it gets the same treatment.
--   * `invitations.email`, on the invitations the erased account **redeemed**
--     (F181). `ListInvitations` carries no state predicate, so `/invites`
--     renders every invitation an organization ever issued — redeemed ones
--     included — and the address each was sent to. Nothing deletes those rows
--     and no setting expires them, so an account deleted, erased and tombstoned
--     everywhere else was still named in full on an ordinary dashboard page.
--
--     `redeemed_by` is the join, not the address. What is scrubbed is the row
--     this account *joined by*, which is a record about them; an **outstanding**
--     invitation addressed to the same text is deliberately left alone, because
--     it is an offer to an address rather than a record of a person, the address
--     became reusable the moment the account was deleted, and blanking it would
--     break the redemption comparison for whoever takes it next.
--
--     Empty string rather than the tombstone, for the reason `users.email` is
--     one: redemption compares a redeeming account's address against this
--     column, and a placeholder that reads like a label is a value that
--     comparison would then have to rule out. `invitations_outstanding_email_key`
--     cannot collide on the blanks — it excludes redeemed rows, which is every
--     row this reaches.
--
-- **The label is a constant and the ids survive** — D148, owner-set 2026-08-08.
-- Nothing is derived from anything, so there is no derivation to reverse.
-- Correlating one erased actor's entries is `audit_logs_actor_idx`, which is
-- keyed on `actor_user_id` and never reads the label. The accepted cost is that a
-- surviving uuid is pseudonymous rather than anonymous data, which
-- `docs/SECURITY.md` states in those words.
--
-- **Re-entrant, because the two-leader window during a rolling deploy is a
-- stated property of this scheduler** (`cmd/linkctrl/jobs.go:117-127`).
-- `FOR UPDATE SKIP LOCKED` means a second leader takes a disjoint batch instead
-- of waiting for the first, and the guard on each id-matched scrub makes a second
-- pass over the same row a no-op rather than a rewrite. Three of those four
-- compare against the tombstone they write; `invitations` blanks its column
-- instead of labelling it, so its guard is an emptiness test on `i.email` beside
-- `i.redeemed_at IS NOT NULL` — the same claim in the vocabulary that column
-- uses. Running the pass twice and diffing is what the test asserts. The
-- **three** value-matched scrubs are re-entrant for a different reason and it is
-- worth stating: after the first pass the address they matched on no longer
-- exists in the column, so the second pass finds nothing to rewrite.
--
-- Ordered by `deleted_at`, oldest first, which is the order
-- `users_pending_erasure_idx` stores and the order the requests arrived in.
--
-- `pending` carries the address as well as the id, because two of the scrubs
-- below have nothing else to match on and the final UPDATE blanks it. That is
-- safe rather than lucky: every CTE in a statement reads one snapshot, so the
-- value here is the pre-erasure one no matter which order the executor runs them
-- in.
WITH pending AS (
    SELECT id, email
      FROM users
     WHERE deleted_at IS NOT NULL
       AND anonymized_at IS NULL
     ORDER BY deleted_at
     LIMIT sqlc.arg(batch)::int
       FOR UPDATE SKIP LOCKED
), batch AS (
    -- The batch as one row of arrays, so the audit scrub below joins each record
    -- exactly once. That is a correctness requirement rather than tidiness: two
    -- data-modifying CTEs may not both write one row, and one CTE joined twice
    -- may not either — Postgres applies one of the updates and drops the other,
    -- and which one is not defined. Both cases are live here. A record is very
    -- often the erased actor's *and* carries their address, and in a batch of
    -- more than one it can be A's record carrying B's address. Aggregating first
    -- makes the join single-valued and the two scrubs one write.
    --
    -- `HAVING count(*) > 0` is what keeps an idle pass cheap, and it is the whole
    -- reason this is not a plain aggregate. An ungrouped aggregate returns one
    -- row over zero input, and one row is enough to make the executor scan
    -- `audit_logs` looking for a match that cannot exist — every hour, on the
    -- largest table in this schema after analytics, on an instance where nobody
    -- has ever deleted an account. Returning no rows instead leaves the join with
    -- an empty side, which ends it before the other side is read.
    SELECT array_agg(id) AS ids,
           coalesce(array_agg(lower(email)) FILTER (WHERE email <> ''), '{}') AS emails
      FROM pending
    HAVING count(*) > 0
), scrubbed_audit AS (
    -- One UPDATE, three scrubs, and they are separate claims about the same row.
    --
    --   * `actor_label` is the snapshot of who acted (M52, D148).
    --   * `metadata`'s `"email"` key is an address carried in the *detail* of a
    --     record (F177), usually of what somebody else did to this account.
    --   * `metadata`'s `"from"` array is the same address in the one place a
    --     writer put a list rather than a scalar (F189).
    --
    -- **One statement, and that is a correctness requirement rather than
    -- tidiness** — it is the note on `batch` below, and F186 is what it costs to
    -- forget: while this scrub was briefly two CTEs, Postgres applied the
    -- metadata one and dropped the label one on the single row that satisfies
    -- both, and the demo's *erased actor in the audit trail* went to zero on
    -- every seed while the seeder reported erasing an account each time.
    -- Reproduced on 2026-08-09 by splitting them again, three seeds and a second
    -- test, and restored.
    --
    -- The two metadata keys merge through `||` rather than nesting two
    -- `jsonb_set`s, so each key names its own predicate once and a fourth would
    -- be one more arm. `jsonb_object_agg` over no matching arm is NULL, and
    -- `metadata || '{}'` is the row unchanged. The WHERE is the disjunction of
    -- all three, so a row is touched when any applies, and the `<>` guard still
    -- makes a second pass over an already-tombstoned label a no-op — the two
    -- value-matched scrubs need no guard, because after one pass the address
    -- they matched on is no longer in the column.
    UPDATE audit_logs a
       SET actor_label = CASE
                           WHEN a.actor_user_id = ANY (b.ids)
                             THEN sqlc.arg(tombstone)::text
                           ELSE a.actor_label
                         END,
           metadata    = a.metadata || coalesce((
                           SELECT jsonb_object_agg(k, v) FROM (
                               SELECT 'email'::text AS k,
                                      to_jsonb(sqlc.arg(tombstone)::text) AS v
                                WHERE lower(a.metadata->>'email') = ANY (b.emails)
                               UNION ALL
                               SELECT 'from',
                                      (SELECT jsonb_agg(
                                                CASE WHEN lower(e.v) = ANY (b.emails)
                                                       THEN sqlc.arg(tombstone)::text
                                                     ELSE e.v END
                                                ORDER BY e.ord)
                                         FROM jsonb_array_elements_text(a.metadata->'from')
                                              WITH ORDINALITY AS e(v, ord))
                                WHERE jsonb_typeof(a.metadata->'from') = 'array'
                                  AND EXISTS (
                                      SELECT 1
                                        FROM jsonb_array_elements_text(a.metadata->'from') g
                                       WHERE lower(g) = ANY (b.emails))
                           ) scrubs), '{}'::jsonb)
      FROM batch b
     WHERE (a.actor_user_id = ANY (b.ids)
            AND a.actor_label <> sqlc.arg(tombstone)::text)
        OR lower(a.metadata->>'email') = ANY (b.emails)
        OR (jsonb_typeof(a.metadata->'from') = 'array'
            AND EXISTS (
                SELECT 1 FROM jsonb_array_elements_text(a.metadata->'from') g
                 WHERE lower(g) = ANY (b.emails)))
    RETURNING 1
), scrubbed_notifications AS (
    -- The inviter's own inbox row, which nothing else here reaches (F188).
    --
    -- Its own CTE because it is its own table: the contention the note on
    -- `batch` describes is between two writes to *one* row, and no other
    -- statement in this pass touches `notifications`.
    --
    -- Both columns in one write for the same reason, and the title reads
    -- `data->>'email'` rather than the batch — the pre-update row is what both
    -- CASEs see, so the address is still there to be replaced.
    UPDATE notifications n
       SET data  = jsonb_set(n.data, '{email}',
                             to_jsonb(sqlc.arg(tombstone)::text)),
           title = replace(n.title, n.data->>'email', sqlc.arg(tombstone)::text)
      FROM batch b
     WHERE lower(n.data->>'email') = ANY (b.emails)
    RETURNING 1
), scrubbed_filed AS (
    UPDATE destination_disputes d
       SET created_by_label = sqlc.arg(tombstone)::text
      FROM pending p
     WHERE d.created_by = p.id
       AND d.created_by_label <> sqlc.arg(tombstone)::text
    RETURNING 1
), scrubbed_decided AS (
    UPDATE destination_disputes d
       SET decided_by_label = sqlc.arg(tombstone)::text
      FROM pending p
     WHERE d.decided_by = p.id
       AND d.decided_by_label <> sqlc.arg(tombstone)::text
    RETURNING 1
), scrubbed_invitations AS (
    UPDATE invitations i
       SET email = ''
      FROM pending p
     WHERE i.redeemed_by = p.id
       AND i.redeemed_at IS NOT NULL
       AND i.email <> ''
    RETURNING 1
)
-- The account row itself, scrubbed in place. It survives, which is the whole
-- difference between `anonymized_at` and `deleted_at`: foreign keys and audit
-- records go on pointing at a row that identifies nobody.
--
-- `email` becomes the empty string rather than a placeholder address. No live
-- query can reach it — every read of `users` filters `deleted_at IS NULL` — and
-- the partial unique index excludes the row, so the address it held was already
-- reusable the moment the account was deleted.
UPDATE users u
   SET email              = '',
       name               = '',
       password_hash      = NULL,
       email_verified_at  = NULL,
       mfa_secret         = NULL,
       mfa_enabled_at     = NULL,
       last_login_at      = NULL,
       failed_login_count = 0,
       locked_until       = NULL,
       anonymized_at      = now(),
       updated_at         = now()
  FROM pending p
 WHERE u.id = p.id
RETURNING u.id;
