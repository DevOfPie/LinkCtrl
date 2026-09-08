-- name: AddonSettingValues :many
-- Every stored value for one add-on, whatever its manifest currently declares.
--
-- Scoping to the declaration is the *caller's*, not this statement's, and the
-- split is deliberate: `config_get` answers only for a declared setting (D263)
-- and the manager renders only declared settings, so filtering here as well would
-- put the same rule in two places and would silently delete the value an operator
-- typed for a setting an add-on temporarily stopped declaring. What comes back is
-- the whole of what is stored; what is used is decided against the manifest in
-- hand.
SELECT name, value, secret, updated_at
  FROM addon_settings
 WHERE addon = @addon
 ORDER BY name;

-- name: SaveAddonSetting :exec
-- Write one declared setting's value.
--
-- The manager saves a form as a sequence of these inside one transaction, so a
-- half-applied form is not a state the next `config_get` can read. `updated_at`
-- is restamped on every save including one that changes nothing, because the
-- question an operator asks of this column is *when was this last touched* rather
-- than *when did it last differ*.
--
-- `secret` is written from the type the manifest declares *now*, which is what
-- makes the column say what the value being written is rather than what some
-- earlier manifest called it. Overwriting a stored secret with a non-secret value
-- therefore clears the flag — and that is the deliberate act the column's own
-- comment describes: somebody typed a new value in, so nothing of the credential
-- is left to withhold.
INSERT INTO addon_settings (addon, name, value, secret)
VALUES (@addon, @name, @value, @secret)
ON CONFLICT (addon, name)
DO UPDATE SET value = excluded.value, secret = excluded.secret, updated_at = now();

-- name: DeleteAddonSetting :exec
-- Clear one declared setting, so the add-on falls back to its manifest default.
--
-- Emptying a field in the manager means *unset*, not *the empty string*: the
-- environment route reads a set-and-empty variable as unset (config.AddonSettings)
-- and a stored empty string that behaved differently would make the same value
-- mean two things depending on which route an operator used.
DELETE FROM addon_settings WHERE addon = @addon AND name = @name;

-- name: CountAddonSettings :one
-- How many stored values there are for one add-on's name.
--
-- For the orphan purge's confirmation, which is the point of decision the M68
-- manager puts every leftover at. The row is keyed on the *name* (04800, and
-- 04500 before it), so what this counts is what whatever is installed under that
-- name next inherits — and a purge deletes none of it. Counted rather than
-- described, for the reason the schema's size is measured rather than cached:
-- a sentence about data an operator cannot see is worth less than a number.
SELECT count(*) FROM addon_settings WHERE addon = @addon;
