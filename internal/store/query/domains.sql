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
INSERT INTO domains (id, organization_id, workspace_id, hostname)
VALUES ($1, $2, $3, $4)
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
UPDATE domains
   SET hostname = $2, updated_at = now()
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

-- name: CountLinksOnDomain :one
-- What deletion is refused for. Zero on every registered hostname today, because
-- nothing serves one and links are created on the default domain — the guard is
-- here so that it is already true when M40 makes it reachable.
SELECT count(*) FROM links WHERE domain_id = $1 AND deleted_at IS NULL;
