//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// M68's claims that need a real database and a real module (the Add-on manager).
//
// Four of them, and none is checkable without both: a saved setting is stored and
// read back without its secret, it is what the *module's own* `config_get` answers
// the next time the add-on loads, a purge drops the schema and leaves the login
// role, and both writes leave an instance-wide audit record naming what changed
// and no value. The arithmetic either side — which source wins, what a value may
// be, how a figure is written — is asserted without a database in internal/addon
// and internal/httpx.

type managerFixture struct {
	t         *testing.T
	pool      *pgxpool.Pool
	host      *addon.Host
	dir       string
	dsn       string
	audit     *audit.Service
	log       *logSink
	principal *auth.Identity
	tenant    *auth.Identity
}

// newAddonManager builds a host with a database behind it and an instance
// principal to act as, over one add-on that declares settings and owns a schema.
func newAddonManager(t *testing.T, name string, settings []addon.Setting) *managerFixture {
	return newAddonManagerWith(t, name, settings, nil)
}

// newAddonManagerWith is the same fixture with extra manifest permissions.
//
// One caller wants `redirect.inline`, because the claim it holds — a saved value
// reaches a *live* instance — is only checkable by invoking the module on a
// running host, and the redirect path is the one path in this product that keeps
// instances between invocations. Every other test here wants the two permissions
// below and nothing else, which is why the addition is a parameter rather than a
// new default.
func newAddonManagerWith(
	t *testing.T, name string, settings []addon.Setting, extra []string,
) *managerFixture {
	t.Helper()
	pool, dsn, dir := newAddonDB(t, name)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	auditSvc := audit.NewService(pool)
	// The first account claims the instance, which is what makes it the principal
	// and therefore the holder of `addons.manage`.
	principal, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "principal@example.com", Name: "Principal",
		Password: "a-sufficiently-long-password", IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("claim the instance: %v", err)
	}
	tenant, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "tenant@example.com", Name: "Tenant",
		Password: "another-sufficiently-long-password",
	})
	if err != nil {
		t.Fatalf("register a tenant: %v", err)
	}

	writeConfigurableAddon(t, dir, name, settings, extra)
	f := &managerFixture{
		t: t, pool: pool, dir: dir, dsn: dsn, audit: auditSvc,
		principal: principal, tenant: tenant,
	}
	f.host, f.log = f.open()
	return f
}

// open starts a host over the fixture's directory. Called again by the test that
// needs a *second* load to see what a module reads at start-up.
func (f *managerFixture) open() (*addon.Host, *logSink) {
	f.t.Helper()
	sink := &logSink{}
	h, err := addon.Open(context.Background(), addon.Options{
		Dir:     f.dir,
		DB:      f.pool,
		DSN:     f.dsn,
		Audit:   f.audit,
		Metrics: observability.NewMetrics(),
		Logger:  slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		f.t.Fatalf("open a host: %v\n%s", err, sink.String())
	}
	f.t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h, sink
}

// writeConfigurableAddon places a module and the manifest that declares its
// settings.
//
// The `settings` fixture, which reads its declared settings at load and logs what
// it got — the only way to assert that a stored value reached the *module* rather
// than merely the database. Deliberately **not** `probe`, which asserts that
// storage answers `ErrInternal` and therefore panics against the real Postgres
// these tests need; that fixture's header says so and this one's says why it
// exists beside it.
//
// The grant is `config.read` plus `storage.own_schema`, and the second is what
// gives every add-on here a schema for the orphan tests to find. No migrations:
// the schema is created by the grant, and what the purge drops is a schema rather
// than the tables in it.
func writeConfigurableAddon(
	t *testing.T, root, name string, settings []addon.Setting, extra []string,
) {
	t.Helper()
	code := addonFixture(t, "settings")
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(code)
	m := addon.Manifest{
		SchemaVersion: addon.SchemaVersion,
		Name:          name,
		Version:       "1.0.0",
		ABIVersion:    1,
		Module:        name + ".wasm",
		SHA256:        hex.EncodeToString(sum[:]),
		FailureClass:  addon.ClassDegrade,
		Permissions:   append([]string{"config.read", "storage.own_schema"}, extra...),
		Settings:      settings,
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, m.Module), code, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, addon.ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// probeSettings is the declaration the `settings` fixture is written against.
func probeSettings() []addon.Setting {
	return []addon.Setting{
		{Name: "retention_days", Type: addon.SettingText, Default: "30"},
		{Name: "api_token", Type: addon.SettingSecret},
	}
}

// A saved setting is stored, reads back without its secret, and is what the
// module's own `config_get` answers.
//
// The last clause is the end-to-end one and it is why this test loads a host
// twice. `probe` reads `retention_days` during package initialization and logs
// what it got, so a second load over the same directory — after the value is
// saved — is a module reporting what this product handed it, from inside the
// guest, through the ABI. Nothing else here proves the value left Postgres.
func TestASavedSettingReachesTheModule(t *testing.T) {
	f := newAddonManager(t, "cfg", probeSettings())

	before, err := f.host.Detail(t.Context(), f.principal, "cfg")
	if err != nil {
		t.Fatalf("read the add-on: %v\n%s", err, f.log.String())
	}
	if before.ConfiguredCount != 0 || before.DeclaredSettings != 2 {
		t.Errorf("a fresh add-on reads %d of %d configured, want 0 of 2",
			before.ConfiguredCount, before.DeclaredSettings)
	}
	if !strings.Contains(f.log.String(), "settings: retention_days=30") {
		t.Errorf("the first load did not read the manifest's default:\n%s", f.log.String())
	}

	saved, err := f.host.SaveSettings(t.Context(), f.principal, "cfg", map[string]string{
		"retention_days": "7",
		"api_token":      "the-secret-value",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	byName := map[string]addon.SettingView{}
	for _, v := range saved {
		byName[v.Name] = v
	}
	if got := byName["retention_days"]; got.Value != "7" || got.Source != addon.SourceStored {
		t.Errorf("retention_days reads back as %q from %s", got.Value, got.Source)
	}
	if got := byName["api_token"]; got.Value != "" || !got.Configured {
		t.Errorf("the secret reads back as configured=%v value=%q; a stored secret is "+
			"never echoed and must still be reported as set", got.Configured, got.Value)
	}
	if got := byName["api_token"]; got.UpdatedAt == nil {
		t.Error("the secret carries no timestamp; when it was last changed is the one " +
			"fact about a secret this page can honestly show")
	}

	// The module's own reading, at the next load.
	_ = f.host.Close(context.Background())
	second, log := f.open()
	f.host = second
	if !strings.Contains(log.String(), "settings: retention_days=7") {
		t.Errorf("the module read something other than the saved value:\n%s", log.String())
	}

	// Clearing puts the declared default back, which is the other half of "empty
	// means unset" — and it is a different claim from never having set one.
	if _, err := f.host.SaveSettings(t.Context(), f.principal, "cfg",
		map[string]string{"retention_days": ""}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	var n int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM addon_settings WHERE addon = 'cfg' AND name = 'retention_days'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("clearing a setting left %d rows; empty means unset, not the empty "+
			"string, or the same value means two things depending on the route", n)
	}
}

// A save reaches an instance the host has **already built**, on a running system,
// which is the sentence the page shows after every save.
//
// The page says *the add-on reads the new values on its next invocation*, and
// docs/usage.md, docs/configuration.md and docs/addon-abi.md repeat it. Until this
// test nothing drove it: the end-to-end assertion above proves the value survives
// to the *next load* of the host, and internal/addon's unit test proves the holder
// is shared by a hostState copy. Neither invokes a guest on a live host, and
// between them sat the case that made the sentence false — M66.5's pool keeps
// instances between invocations, so a module that read its setting during package
// initialization went on using the old value until the entry aged out
// (`addon.DefaultPoolTTL`, a minute).
//
// The redirect path is the one path that pools, which is why this drives it. The
// fixture reports two things per invocation: what it cached at initialization, and
// what `config_get` answers now. Both must move, and they move for different
// reasons — the live read because the settings holder is swapped, the cached one
// because SaveSettings drains the pool.
func TestASavedSettingReachesAnInstanceTheHostAlreadyBuilt(t *testing.T) {
	f := newAddonManagerWith(t, "live", probeSettings(),
		[]string{addon.PermissionRedirectInline})

	decision := addon.RedirectDecision{
		LinkID: uuid.New(), WorkspaceID: uuid.New(),
		Alias: "anything", Destination: "https://example.test/",
	}

	// The first invocation. It instantiates, so the module caches the manifest's
	// declared default, and it leaves the instance in the pool behind it.
	if out := f.host.Inline(t.Context(), decision); out.Vetoed {
		t.Fatalf("the fixture vetoed a redirect it should have allowed:\n%s", f.log.String())
	}
	if !strings.Contains(f.log.String(), "invoked: cached=30") {
		t.Fatalf("the first invocation did not cache the manifest's default, so the "+
			"rest of this test would be measuring nothing:\n%s", f.log.String())
	}
	mark := len(f.log.String())

	if _, err := f.host.SaveSettings(t.Context(), f.principal, "live",
		map[string]string{"retention_days": "7"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The next invocation, on the same running host. No reload, no restart.
	if out := f.host.Inline(t.Context(), decision); out.Vetoed {
		t.Fatalf("the fixture vetoed after the save:\n%s", f.log.String())
	}
	after := f.log.String()[mark:]
	if !strings.Contains(after, "invoked: live=7") {
		t.Errorf("`config_get` on the invocation after the save answered something "+
			"other than the saved value. The settings holder is what makes a live read "+
			"see it:\n%s", after)
	}
	if !strings.Contains(after, "invoked: cached=7") {
		t.Errorf("the invocation after the save ran on an instance that had cached the "+
			"old value at initialization, so a module that reads its settings once — "+
			"which is what a real add-on does — kept using them. SaveSettings drains "+
			"the add-on's pool for exactly this, and the page's own sentence is wrong "+
			"for a pool TTL without it:\n%s", after)
	}
}

// A stored secret stays withheld even when a later manifest declares it as text.
//
// m68.md states the promise absolutely — *Secrets get the Secret treatment and are
// never echoed back into the form* — and until the `secret` column existed the
// promise rested on the manifest in hand. M67 made remove-then-install the
// documented way to replace an add-on, so a successor declaring the same setting
// name as `text` is a path an operator walks rather than a hypothetical, and it
// had the predecessor's credential rendered into the form and returned by the API.
//
// Nothing is escalated by it — reaching either costs `addons.manage` — which is
// why the fix is a column rather than a refusal: what changes is that the promise
// is now a property of what was stored.
func TestAStoredSecretIsNotUnmaskedByALaterManifest(t *testing.T) {
	f := newAddonManager(t, "swapped", probeSettings())

	if _, err := f.host.SaveSettings(t.Context(), f.principal, "swapped",
		map[string]string{"api_token": "the-credential"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The replacement: same name, same setting name, `text` where the predecessor
	// declared a secret. Written over the directory and loaded, which is what
	// remove-then-install leaves behind.
	writeConfigurableAddon(t, f.dir, "swapped", []addon.Setting{
		{Name: "retention_days", Type: addon.SettingText, Default: "30"},
		{Name: "api_token", Type: addon.SettingText},
	}, nil)
	_ = f.host.Close(context.Background())
	second, log := f.open()
	f.host = second

	got, err := second.Detail(t.Context(), f.principal, "swapped")
	if err != nil {
		t.Fatalf("read the replacement: %v\n%s", err, log.String())
	}
	for _, v := range got.Settings {
		if v.Name != "api_token" {
			continue
		}
		if v.Value != "" {
			t.Errorf("the predecessor's secret is echoed as %q by a manifest that "+
				"re-declares the setting as %s. The promise has to rest on the column "+
				"the value was written under, not on the manifest in hand", v.Value, v.Type)
		}
		if !v.Configured {
			t.Error("the replacement reports the setting as unset; it has a stored value " +
				"and the page has to say so, which is the whole of what it may say")
		}
		if !v.IsSecret() {
			t.Errorf("the setting renders as %s, so the form draws a text box: blank "+
				"means unset there, and the next save would delete a credential nobody "+
				"asked to remove", v.Type)
		}
		return
	}
	t.Fatal("the replacement declares no api_token setting")
}

// A save is refused for a caller who is not the instance principal, for a key the
// manifest does not declare, and for a value its type does not admit — and none of
// the three writes anything.
func TestSavingSettingsRefusesWhatItShould(t *testing.T) {
	f := newAddonManager(t, "guard", probeSettings())

	_, err := f.host.SaveSettings(t.Context(), f.tenant, "guard",
		map[string]string{"retention_days": "7"})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an owner of their own organization saved an add-on's settings: %v", err)
	}
	_, err = f.host.SaveSettings(t.Context(), f.principal, "guard",
		map[string]string{"nosuch": "x"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("an undeclared key was accepted: %v", err)
	}
	_, err = f.host.SaveSettings(t.Context(), f.principal, "guard",
		map[string]string{"retention_days": strings.Repeat("x", addon.MaxSettingValueBytes+1)})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a value past the bound was accepted: %v", err)
	}
	// A refused save is refused whole: the valid key travelling beside an invalid
	// one is not written either, because the form is one transaction.
	_, err = f.host.SaveSettings(t.Context(), f.principal, "guard",
		map[string]string{"retention_days": "7", "nosuch": "x"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a mixed form was accepted: %v", err)
	}

	var n int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM addon_settings WHERE addon = 'guard'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows were written by refused saves", n)
	}
}

// Removing an add-on leaves its schema, the manager lists it, and purging drops
// the schema and leaves the login role.
//
// One test because it is one decision an operator makes, and the last clause is
// the one worth pinning: `DROP SCHEMA … CASCADE` takes the add-on's tables, and
// the role stays so that re-installing under the same name works as it did —
// which is what docs/SECURITY.md and the migration both say.
func TestAnOrphanIsListedAndPurgedAndTheRoleStays(t *testing.T) {
	f := newAddonManager(t, "leaves", probeSettings())

	// One saved value, so the counts below are about something. It is what makes
	// the last clause of this test checkable: a removal leaves the row, a purge
	// leaves the row, and nothing here can delete it.
	if _, err := f.host.SaveSettings(t.Context(), f.principal, "leaves",
		map[string]string{"retention_days": "7"}); err != nil {
		t.Fatalf("save a setting: %v", err)
	}

	if orphans, err := f.host.Orphans(t.Context(), f.principal); err != nil {
		t.Fatal(err)
	} else if len(orphans) != 0 {
		t.Fatalf("an installed add-on's schema is listed as orphaned: %v", orphans)
	}
	// Purging an installed add-on's data is refused rather than done: dropping a
	// schema out from under a running module has no upside over removing it first.
	if _, err := f.host.PurgeData(t.Context(), f.principal, "leaves"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("purging an installed add-on's data answered %v, want a conflict", err)
	}
	// And a tenant may not read the list at all, because what it names is what this
	// box runs.
	if _, err := f.host.Orphans(t.Context(), f.tenant); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an ordinary owner listed the instance's orphaned data: %v", err)
	}

	if _, err := f.host.Remove(t.Context(), f.principal, "leaves"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	orphans, err := f.host.Orphans(t.Context(), f.principal)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].Name != "leaves" {
		t.Fatalf("the removal's leftover is %v, want one row naming the add-on", orphans)
	}
	if orphans[0].Schema != store.AddonSchema("leaves") {
		t.Errorf("the row names %q as the schema", orphans[0].Schema)
	}
	// **The schema is not the only thing the removal left, and the row says so.**
	// `addon_settings` is keyed on the add-on's *name*, so the value saved above
	// survives the removal and is inherited by whatever is installed under the name
	// next. The confirmation counts it because nothing in the product deletes it —
	// `SaveSettings` refuses a name that is not loaded, which the last clause of
	// this test is.
	if orphans[0].StoredSettings != 1 {
		t.Errorf("the orphan reports %d stored settings; one was saved and a removal "+
			"deletes none of them", orphans[0].StoredSettings)
	}

	gone, err := f.host.PurgeData(t.Context(), f.principal, "leaves")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if gone.Schema != store.AddonSchema("leaves") {
		t.Errorf("the purge answered for %q", gone.Schema)
	}

	if exists(t, f.pool,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`,
		store.AddonSchema("leaves")) {
		t.Error("the schema is still there after a purge")
	}
	if !exists(t, f.pool,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`,
		store.AddonSchema("leaves")) {
		t.Error("the login role went with the schema; the migration and docs/SECURITY.md " +
			"both say it stays, and re-installing under this name depends on it")
	}

	if gone.StoredSettings != 1 {
		t.Errorf("the purge answered with %d stored settings left; it drops the schema "+
			"and nothing else", gone.StoredSettings)
	}
	// And the row is still there afterwards, which is the claim four documents make
	// and the reason the confirmation says *four things*. There is no surface in
	// this product that would remove it — the save below is the one that would, and
	// it refuses.
	var left int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM addon_settings WHERE addon = 'leaves'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("the purge deleted %d of the stored settings; it is DROP SCHEMA and "+
			"nothing else, and docs/SECURITY.md says so", 1-left)
	}
	if _, err := f.host.SaveSettings(t.Context(), f.principal, "leaves",
		map[string]string{"retention_days": ""}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a removed add-on's settings were writable: %v — F332 is the row that "+
			"says nothing in this product can delete them", err)
	}

	// Purging twice is a 404 rather than a second success, because "there was
	// nothing there" and "deleted" are different answers to somebody who may have
	// mistyped a name.
	if _, err := f.host.PurgeData(t.Context(), f.principal, "leaves"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a second purge answered %v, want not-found", err)
	}
}

// A `select` can be unset once it has been set, which the type check refused.
//
// Empty means *unset* on this form for every type — the row is deleted and the
// add-on falls back to whatever its manifest declares. For a `select` the check
// was membership of the option list, `""` is in no option list, and the manager's
// own empty option therefore came back 422: a value could be chosen and never
// unchosen. The page half is `TestASelectCanBeLeftUnset` in internal/ui; this is
// the half with a database under it, because what is asserted is that the row goes.
func TestASelectIsUnsetByAnEmptyValue(t *testing.T) {
	f := newAddonManager(t, "chooser", []addon.Setting{
		{Name: "grouping", Type: addon.SettingSelect, Options: []string{"day", "week"}},
	})

	if _, err := f.host.SaveSettings(t.Context(), f.principal, "chooser",
		map[string]string{"grouping": "week"}); err != nil {
		t.Fatalf("save a chosen option: %v", err)
	}
	// A value outside the list is still refused, or the fix below would be the
	// check deleted rather than the empty string exempted from it.
	if _, err := f.host.SaveSettings(t.Context(), f.principal, "chooser",
		map[string]string{"grouping": "fortnight"}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("an option that is not on the list was accepted: %v", err)
	}

	views, err := f.host.SaveSettings(t.Context(), f.principal, "chooser",
		map[string]string{"grouping": ""})
	if err != nil {
		t.Fatalf("clear a select: %v — empty means unset for every other type", err)
	}
	if len(views) != 1 || views[0].Configured {
		t.Errorf("the setting still reads as configured after being cleared: %+v", views)
	}
	var n int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM addon_settings WHERE addon = 'chooser'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows survived the clear; an empty value deletes the row rather "+
			"than storing an empty string", n)
	}
}

// Both of the manager's writes are in the instance-wide audit log, and the
// settings record names what changed and never a value.
func TestTheManagersWritesAreAudited(t *testing.T) {
	f := newAddonManager(t, "audited", probeSettings())
	if _, err := f.host.SaveSettings(t.Context(), f.principal, "audited",
		map[string]string{"api_token": "must-not-appear"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.Remove(t.Context(), f.principal, "audited"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.PurgeData(t.Context(), f.principal, "audited"); err != nil {
		t.Fatal(err)
	}

	rows, err := f.pool.Query(t.Context(),
		`SELECT action, metadata::text FROM audit_logs
		  WHERE action IN ($1, $2) AND organization_id IS NULL`,
		audit.ActionAddonSettingsSaved, audit.ActionAddonDataPurged)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]string{}
	for rows.Next() {
		var action, meta string
		if err := rows.Scan(&action, &meta); err != nil {
			t.Fatal(err)
		}
		seen[action] = meta
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	saved, ok := seen[audit.ActionAddonSettingsSaved]
	if !ok {
		t.Fatalf("no %s record; both manager writes are instance-wide events",
			audit.ActionAddonSettingsSaved)
	}
	if !strings.Contains(saved, "api_token") || !strings.Contains(saved, "audited") {
		t.Errorf("the settings record does not name what changed: %s", saved)
	}
	if strings.Contains(saved, "must-not-appear") {
		t.Errorf("the audit record carries a secret's value: %s", saved)
	}
	purged, ok := seen[audit.ActionAddonDataPurged]
	if !ok {
		t.Fatalf("no %s record; nothing can measure the schema after the drop, so "+
			"this row is the durable record of what was deleted", audit.ActionAddonDataPurged)
	}
	if !strings.Contains(purged, store.AddonSchema("audited")) ||
		!strings.Contains(purged, "bytes") {
		t.Errorf("the purge record does not name the schema and its size: %s", purged)
	}
}

// exists runs a one-column EXISTS query.
func exists(t *testing.T, pool *pgxpool.Pool, q string, args ...any) bool {
	t.Helper()
	var out bool
	if err := pool.QueryRow(t.Context(), q, args...).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
