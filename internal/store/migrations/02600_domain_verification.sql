-- +goose Up
--
-- A registered hostname gets something to prove with (M40).
--
-- M39 stored ownership and served nothing. The gap between "registered" and
-- "verified" is the alias-namespace hijack this milestone exists to close: until
-- a workspace has proved it controls the DNS for a name, no router may resolve
-- an alias on it. These columns are that proof and its expiry.
--
-- Every column is additive and every one is nullable, so an instance mid-upgrade
-- reads the same rows it wrote — a domain with no token is simply one that has
-- never been checked, which is exactly what every row registered under M39 is.

ALTER TABLE domains
    -- The DNS TXT challenge value, minted once per domain and never rotated on
    -- its own. Rotating it would invalidate a record the owner has already
    -- published, which turns a re-verification into an outage; a rename mints a
    -- new one, because the record lives under the old name.
    ADD COLUMN verification_token text,
    -- When the last check ran, whatever it concluded. Separate from
    -- verified_at, which is when serving started: an operator asking "is this
    -- being re-checked at all?" is asking about this column, and a domain that
    -- verified last week and has not been polled since is a different fault
    -- from one that is failing.
    ADD COLUMN verification_checked_at timestamptz,
    -- When the current run of failures began. NULL means the last check passed,
    -- or none has run. This is the grace window's anchor (decision D70): the
    -- domain stops serving when now() - verification_failing_since exceeds the
    -- configured window, and a single success clears it back to NULL.
    --
    -- A timestamp rather than a counter, deliberately. A counter measures how
    -- many times the job happened to run, so an instance that was down for the
    -- weekend would wake up and count three failures in three minutes; the
    -- window is meant to be wall-clock patience, and only a timestamp is.
    ADD COLUMN verification_failing_since timestamptz,
    -- What the last failed check said, in the words the page shows. Cleared on
    -- success. Not an error code: the useful sentence is "no TXT record at
    -- _linkctrl-challenge.go.example.com", and reducing that to a code would
    -- mean the page has to reconstruct it.
    ADD COLUMN verification_error text;

-- Existing registrations get a token rather than being left unverifiable.
--
-- gen_random_uuid() is core Postgres since 13 and is a CSPRNG draw; the dashes
-- are stripped so the value a person copies into a DNS record is one word. New
-- rows are given a token by the application, which is where the rest of this
-- product mints identifiers.
UPDATE domains
   SET verification_token = replace(gen_random_uuid()::text, '-', '')
 WHERE verification_token IS NULL;

-- The host cache loads every verified domain at boot and on every invalidation,
-- and nothing else asks this question. Partial, so the index is the size of the
-- answer rather than the size of the table.
CREATE INDEX domains_verified_idx ON domains (verified_at)
    WHERE verified_at IS NOT NULL AND deleted_at IS NULL;

-- The re-verification job's candidate list: everything registered, which is
-- every domain that is neither the instance default nor soft-deleted. Ordered
-- by the check that is furthest behind, so a pass that runs out of budget
-- starves nothing.
CREATE INDEX domains_verification_due_idx ON domains (verification_checked_at NULLS FIRST)
    WHERE deleted_at IS NULL AND NOT is_default;

-- +goose Down
DROP INDEX IF EXISTS domains_verification_due_idx;
DROP INDEX IF EXISTS domains_verified_idx;
ALTER TABLE domains
    DROP COLUMN IF EXISTS verification_error,
    DROP COLUMN IF EXISTS verification_failing_since,
    DROP COLUMN IF EXISTS verification_checked_at,
    DROP COLUMN IF EXISTS verification_token;
