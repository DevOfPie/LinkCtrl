-- +goose Up
--
-- Where an add-on's declared settings are kept once somebody types them into the
-- Add-on manager (M68).
--
-- **Host-side, and that is the whole reason this table exists.** An add-on's own
-- schema (M63) is writable by the add-on's own role, so a setting kept there
-- would be a value the module could rewrite and then read back as though an
-- operator had chosen it — which for a `secret` is the difference between a
-- credential and a suggestion. This table is in the host's schema, owned by the
-- application role, and no add-on role has any grant on it: `EnsureAddonSchema`
-- revokes the role's access to `public` and grants it nothing here. A module
-- reads these values only through `config_get`, which the host answers.
--
-- **Keyed on the add-on's name, like `addon_identity_links` (04500), and with the
-- same consequence.** The name is the directory name, which the host proves
-- against the manifest at load, so it is the only stable identifier an add-on
-- has — there is no publisher, no id, nothing signed. What that costs is stated
-- rather than discovered: a *different* module installed under a name that has
-- been used before inherits the rows written for its predecessor, exactly as it
-- inherits that add-on's identity mappings
-- ([F330](../../../docs/build-notes/deferred-findings.md)). M68 answers it where
-- an operator can act on it — removing an add-on names what its removal leaves,
-- and the manager's orphan list is where leftovers are purged — rather than by
-- inventing an identifier the product has no way to verify.
--
-- **Numbering.** `04700` is the last number in this directory, so this is the
-- next free one.
CREATE TABLE addon_settings (
    -- The add-on's manifest name. Not a foreign key, because there is nothing to
    -- reference: what is installed is a directory on disk, and the only record of
    -- it in this database is rows like these.
    addon      text        NOT NULL,

    -- The declared setting's name, as the manifest spells it. A row for a setting
    -- the manifest no longer declares is not read — `config_get` scopes to the
    -- declaration (D263) and the manager renders only declared settings — and it
    -- is deliberately not deleted, so an add-on downgraded and then upgraded again
    -- does not lose what an operator typed.
    name       text        NOT NULL,

    -- The value, as typed. Text for every declared type, including `toggle` and
    -- `select`, for the reason `Setting.Default` is text: the manifest is JSON
    -- written by hand, one representation is one fewer thing to get wrong, and the
    -- host validates a value against its declared type before it is written.
    --
    -- **Not encrypted, and that is a statement rather than an omission.** A
    -- `secret` setting is held in this column in the clear, exactly as
    -- `LINKCTRL_ADDON_<NAME>_<SETTING>` holds one in the process environment. The
    -- protections it does have are that nothing echoes it back — the manager's
    -- form never renders a stored secret's value, and `config.Secret` is what the
    -- host holds it in — and that reading this table requires the database.
    -- Encrypting it would need a key, the key would live beside the database in
    -- the same environment, and the result would be a longer sentence describing
    -- the same exposure. `docs/SECURITY.md` says so where it says it about the
    -- environment.
    value      text        NOT NULL,

    -- Whether the value was written for a setting the manifest declared as a
    -- `secret` **at the moment it was saved**.
    --
    -- It exists because the promise is *a secret is never echoed back into the
    -- form*, and that promise cannot rest on the manifest in hand: an add-on is
    -- replaced by removing it and installing another (M67, and it is the
    -- documented path), the replacement declares the same setting names, and a
    -- version re-declaring `client_secret` as `text` would have had its
    -- predecessor's credential rendered into the form and returned by the API.
    -- Nothing is escalated — reaching either costs `addons.manage` — but the
    -- promise is absolute in m68.md and this is what makes it a property of the
    -- column rather than of a manifest's honesty.
    --
    -- Read in the withholding direction only: true here withholds whatever the
    -- manifest now says, and a manifest declaring a secret withholds whatever is
    -- here. A value stored as a credential stops being one when somebody clears
    -- it, which is a deliberate act rather than a side effect of a save.
    secret     boolean     NOT NULL DEFAULT false,

    updated_at timestamptz NOT NULL DEFAULT now(),

    -- One value per setting per add-on. The manager writes with ON CONFLICT, so
    -- saving a form twice is one row either way.
    PRIMARY KEY (addon, name)
);

-- +goose Down
DROP TABLE addon_settings;
