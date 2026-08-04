-- Domain ownership and registration (M39).
--
-- The settings queries for the instance default live in links.sql, where they
-- were written when there was exactly one domain. These are the ones that exist
-- because there can now be more than one, and every statement here reads or
-- writes the *ownership* columns rather than the serving ones — nothing on a
-- registered hostname is served until M40 verifies it.
--
-- Ownership is never decided in SQL. Every write below is by id, and
-- link.Service reads the row first and judges the actor against it, so the
-- refusal is a sentence naming whose domain it is rather than a statement that
-- silently affected no rows. ListDomains is the exception, and it is a read: it
-- is scoped by the actor's organization and workspace because a list is only
-- ever the caller's own.

-- name: CreateDomain :one
-- Registered, and deliberately unverified: verified_at stays NULL and
-- ssl_status stays at its 'none' default. is_default is never set here — there
-- is one instance default and 00700 seeded it.
--
-- The challenge token is minted here (M40) rather than lazily on the first
-- verification attempt, so the page that tells somebody which DNS record to
-- publish can do it the moment they register — the alternative is a page that
-- asks them to come back.
INSERT INTO domains (id, organization_id, workspace_id, hostname, verification_token)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetDomainByID :one
-- The row the ownership check is made against, so it carries both owner columns.
SELECT * FROM domains WHERE id = $1 AND deleted_at IS NULL;

-- name: GetDomainByHostname :one
-- Matches domains_hostname_key exactly — lower(hostname) among the undeleted —
-- so the availability check and the unique index cannot disagree about which
-- names collide.
SELECT * FROM domains WHERE lower(hostname) = lower($1) AND deleted_at IS NULL;

-- name: ListDomains :many
-- Every domain the caller may use: the instance default, whatever their
-- organization owns, and whatever their own workspace owns.
--
-- Another workspace's hostname is absent rather than present and unmanageable.
-- A list that showed it would disclose which hostnames a neighbouring workspace
-- has registered, and the registration is the only thing there is to disclose at
-- this milestone.
--
-- Ordered default-first, then by hostname: the default is the one every link is
-- on today, so it belongs at the top rather than wherever its placeholder name
-- happens to sort.
SELECT d.*, count(l.id)::bigint AS link_count
FROM domains d
LEFT JOIN links l ON l.domain_id = d.id AND l.deleted_at IS NULL
WHERE d.deleted_at IS NULL
  AND (
        (d.organization_id IS NULL AND d.workspace_id IS NULL)
     OR (d.organization_id = sqlc.arg(organization_id) AND d.workspace_id IS NULL)
     OR  d.workspace_id = sqlc.arg(workspace_id)
      )
GROUP BY d.id
ORDER BY d.is_default DESC, lower(d.hostname), d.id;

-- name: RenameDomain :one
-- The hostname is the only thing a registration has to change, and it is
-- changeable only while nothing serves it; see decisions.md, D69.
--
-- Not scoped by owner. The caller has already been judged against the row read
-- by GetDomainByID, and repeating the predicate here would turn a 403 into a
-- 404 for anybody who got past that check by a route this file cannot see.
--
-- **A rename un-verifies (M40), and that is the bullet D69 deferred to here.**
-- The proof of control is a TXT record published under the *old* name and says
-- nothing about the new one, so carrying verified_at across would let a
-- workspace verify a name it controls and then rename the row to one it does
-- not. The token is minted afresh for the same reason: the old value is
-- published in somebody else's zone.
UPDATE domains
   SET hostname                  = $2,
       verification_token        = $3,
       verified_at               = NULL,
       ssl_status                = 'none',
       verification_checked_at   = NULL,
       verification_failing_since = NULL,
       verification_error        = NULL,
       updated_at                = now()
 WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteDomain :execrows
-- Soft, unlike a folder. A domain is the namespace its links' aliases live in
-- and `links.domain_id` is NOT NULL with no cascade, so a hard delete is refused
-- by the database the moment one link exists; a soft delete keeps the row that
-- every historic click event and reserved alias still points at.
UPDATE domains
   SET deleted_at = now(), updated_at = now()
 WHERE id = $1 AND deleted_at IS NULL;

-- name: CountWorkspaceDomains :one
-- How many hostnames this workspace has registered, which is what the
-- per-workspace cap is applied to (M40, reopened).
--
-- Every undeleted row, verified or not, because what the cap bounds is the work
-- a registration creates: one outbound DNS lookup per hostname per pass, against
-- a nameserver the registrant chooses. An unverified hostname costs exactly the
-- same lookup as a verified one, so counting only the verified ones would bound
-- the wrong number.
SELECT count(*) FROM domains
WHERE workspace_id = $1 AND deleted_at IS NULL;

-- name: CountLinksOnDomain :one
-- What deletion is refused for. Zero on every registered hostname today, because
-- nothing serves one and links are created on the default domain — the guard is
-- here so that it is already true when M40 makes it reachable.
SELECT count(*) FROM links WHERE domain_id = $1 AND deleted_at IS NULL;

-- Verification and serving (M40).
--
-- The gate this milestone exists for is one column: `verified_at`. Nothing below
-- lets a hostname be served without it, and the two statements that clear it —
-- UnverifyDomain here, RenameDomain above — are the only ways serving stops.

-- name: ListVerifiedDomains :many
-- Everything the host router may resolve aliases on, which is the whole of what
-- the in-process hostname cache holds.
--
-- **`verified_at IS NOT NULL` is the gate.** A registered-but-unverified
-- hostname is absent from this result, so it is absent from the cache, so the
-- router has nothing to match a Host header against and the request lands on
-- ops-only 404. There is no second predicate anywhere that could disagree with
-- this one, because there is no second query.
--
-- The instance default is excluded: it is matched on `is_default` at boot and
-- serves through the ordinary link host, and including it here would give one
-- hostname two routes into the redirect tree.
--
-- Hostnames are lowered here rather than at lookup, so the map is keyed the way
-- config.CanonicalHost spells a Host header.
SELECT id, lower(hostname) AS hostname, root_redirect_url, ssl_status
FROM domains
WHERE verified_at IS NOT NULL
  AND deleted_at IS NULL
  AND NOT is_default;

-- The re-verification job's work list, in two classes (M40, reopened).
--
-- **One queue could be starved, and the thing it starved was the hard stop.**
-- The job walked a single list ordered `verification_checked_at NULLS FIRST`,
-- which is the right order for one class and fatal across both: `RenameDomain`
-- writes that column back to NULL, so a workspace renaming its rows in a loop
-- kept them at the head of the queue for ever, while a *serving* hostname — which
-- always carries a watermark, because a check is what made it serve — sorted last
-- and was never reached. The only mechanism that takes a lapsed or hijacked
-- hostname out of service therefore stopped running instance-wide, while every
-- pass logged healthy counts.
--
-- Splitting on `verified_at` closes it exactly, because a rename un-verifies:
-- churn can only ever crowd the *pending* class, and the class whose checks can
-- stop serving is drawn separately and walked first. Neither statement reads a
-- NULL `verification_checked_at` as anything but "not checked yet" — it is what a
-- live renamed row carries, and treating it as abandonment would delete the
-- registration of anybody mid-cut-over.

-- name: ListServingDomainsForVerification :many
-- Hostnames this instance is serving, oldest check first.
--
-- Walked first and given the whole budget it needs, because these are the rows
-- where a failing check has a consequence: the grace window runs against them and
-- `UnverifyDomain` ends it. A pass that runs out of time must run out of it on
-- the class where the cost of waiting is another hour of not-yet-serving, not on
-- the class where it is another hour of serving a name whose DNS is gone.
SELECT id, hostname
FROM domains
WHERE deleted_at IS NULL
  AND NOT is_default
  AND verification_token IS NOT NULL
  AND verified_at IS NOT NULL
ORDER BY verification_checked_at NULLS FIRST, id
LIMIT sqlc.arg(row_limit);

-- name: ListPendingDomainsForVerification :many
-- Registered hostnames that are not being served, oldest check first.
--
-- Included deliberately, and second. Somebody who registers a hostname and
-- publishes the record should not have to come back and press a button — the
-- on-demand check exists for the person who does not want to wait, not because
-- waiting is the only other option. What they may now have to wait for is a pass
-- with room left after the serving class, which is the price of the serving class
-- never waiting for them.
SELECT id, hostname
FROM domains
WHERE deleted_at IS NULL
  AND NOT is_default
  AND verification_token IS NOT NULL
  AND verified_at IS NULL
ORDER BY verification_checked_at NULLS FIRST, id
LIMIT sqlc.arg(row_limit);

-- name: MarkDomainVerified :one
-- A successful check, written only onto the row that was actually checked.
--
-- **The hostname and the token are in the predicate because they are what was
-- proved (M40, reopened).** Verification reads a row, resolves DNS against the
-- hostname it read — seconds, against a nameserver the registrant runs — and then
-- writes here. `RenameDomain` is concurrently reachable and clears `verified_at`;
-- an unconditional write landing after it would fill exactly that NULL through
-- the COALESCE below and start serving a name nobody proved, up to and including
-- one of the instance's own hosts. Predicating on `hostname` and
-- `verification_token` makes the late write affect **zero rows** instead, and the
-- caller treats that as the conflict it is.
--
-- A transaction around the read and the write would not have closed this: the
-- rename commits in the gap between two separate transactions, so there is
-- nothing for it to serialise against. `FOR UPDATE` across the lookup would have,
-- by pinning a row lock for the DNS timeout inside a job that walks a batch —
-- which is a different outage.
--
-- Sets verified_at only when it is not already set, so a domain that has been
-- serving for a month does not have its start date rewritten every hour — the
-- column answers "since when has this been served", and a re-check is not a new
-- answer.
--
-- ssl_status moves to 'pending': the app never speaks ACME (decision D3), so the
-- most it can say is that it will now answer Caddy's on-demand ask for this
-- hostname. 'active' is written by that ask endpoint, once, when it is first
-- consulted.
--
-- The failing streak is cleared unconditionally, which is D70's "a successful
-- check at any point resets the count".
UPDATE domains
   SET verified_at                = COALESCE(verified_at, now()),
       ssl_status                 = CASE WHEN ssl_status = 'none' THEN 'pending' ELSE ssl_status END,
       verification_checked_at    = now(),
       verification_failing_since = NULL,
       verification_error         = NULL,
       updated_at                 = now()
 WHERE id = sqlc.arg(id)
   AND hostname = sqlc.arg(hostname)
   AND verification_token = sqlc.arg(verification_token)
   AND deleted_at IS NULL
RETURNING *;

-- name: MarkDomainVerificationFailed :one
-- A failed check, and deliberately *not* a stop.
--
-- verified_at is untouched: a domain that is serving goes on serving while the
-- grace window runs (D70). What this records is that the window has started —
-- COALESCE keeps the first failure's timestamp, so a run of failures anchors on
-- when the run began rather than sliding forward with every poll, which would
-- make the window unreachable.
UPDATE domains
   SET verification_checked_at    = now(),
       verification_failing_since = COALESCE(verification_failing_since, now()),
       verification_error         = sqlc.arg(verification_error)::text,
       updated_at                 = now()
 WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: UnverifyDomain :one
-- The end of the grace window, and the only place serving stops on its own.
--
-- `verified_at` is cleared, so the next ListVerifiedDomains does not return the
-- row, so every replica's host cache drops it and the hostname goes back to
-- ops-only 404. ssl_status goes with it: this instance will stop answering
-- Caddy's ask for the name, which is the other half of no longer serving it.
--
-- The failing streak is *kept*. The page has to be able to say "this stopped
-- being served at 03:00 because it had been failing since yesterday", and
-- clearing the anchor here would throw away the only record of why.
UPDATE domains
   SET verified_at        = NULL,
       ssl_status         = 'none',
       verification_error = sqlc.arg(verification_error)::text,
       updated_at         = now()
 WHERE id = $1 AND verified_at IS NOT NULL AND deleted_at IS NULL
RETURNING *;

-- name: MarkDomainTLSActive :execrows
-- Caddy asked whether to obtain a certificate for this hostname and was told
-- yes. Guarded on 'pending' so it is one write per verification rather than one
-- per handshake: the ask endpoint is public and unauthenticated, and a statement
-- it could run on every request would be a write amplifier anybody can pull.
UPDATE domains
   SET ssl_status = 'active', updated_at = now()
 WHERE id = $1 AND ssl_status = 'pending' AND deleted_at IS NULL;

-- name: SetDomainRootRedirect :one
-- Where a verified hostname's own root sends a visitor (M40).
--
-- The same column 00800 added for the instance default, addressed by id instead
-- of by `is_default`. A custom hostname is a bare domain somebody will type, and
-- answering 404 there is a choice its owner should get to make rather than
-- inherit from the instance.
--
-- NULL clears it, restoring the 404.
UPDATE domains
   SET root_redirect_url = sqlc.narg(root_redirect_url), updated_at = now()
 WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING *;
