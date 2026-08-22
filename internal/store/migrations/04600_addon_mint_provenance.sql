-- +goose Up
--
-- Which add-on's assertion a pending second factor came from (M65).
--
-- **The provenance record has to survive the prompt, or it describes the wrong
-- event.** m65.md asks the audit writer to record *the session's* provenance. For
-- an account with no second factor that is one write at the mint. For an account
-- with TOTP enrolled the assertion produces no session at all: it produces a
-- pending login, and the session is minted minutes later by
-- `CompleteSecondFactor`, on a path that has never heard of an add-on. Without
-- these two columns the trail for a TOTP account says an add-on asserted an
-- identity and then, separately, that a session came into existence — and nothing
-- joins the two, which is precisely the account the provenance is most worth
-- having for.
--
-- Nullable, and null is the ordinary case: a pending login from the password form
-- has no add-on and must not acquire a label that says it did.
--
-- **The issuer and nothing else.** `addon` names which module vouched and
-- `issuer` names the provider as it named itself; the external *subject*, the
-- address the assertion carried and the display name are all absent, for the
-- reason `audit_logs.metadata` does not carry them either — M52's erasure sweep
-- scrubs by the keys it knows, its coverage was counted site by site to close
-- F177, and a person's provider identifier in a column nothing sweeps would be
-- that count going wrong again. Neither column identifies a person: both are
-- properties of the software in the middle.
--
-- Additive, per the host's own DDL rule: two nullable columns on an existing
-- table, no backfill, and every statement that reads the table today keeps
-- working. `DeleteAccountDependents` already takes these rows whole.
ALTER TABLE mfa_pending_logins
    ADD COLUMN minted_by_addon  text,
    ADD COLUMN minted_by_issuer text;

-- +goose Down
ALTER TABLE mfa_pending_logins
    DROP COLUMN minted_by_addon,
    DROP COLUMN minted_by_issuer;
