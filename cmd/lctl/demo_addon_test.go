package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// The demo's seeded add-on settings are settings the shipped manifest declares
// (M68).
//
// `seedAddonSettings` writes rows straight into `addon_settings` because `lctl`
// has no add-on host to call `SaveSettings` on — the one place in the seeder that
// does not go through the product's own service. What that skips is the
// validation the service would have done: a name the manifest does not declare is
// written happily, and then `config_get` never answers it and the manager never
// renders it, so the demo shows a detail page with a setting missing and a row in
// the database nothing reads.
//
// `demoCoverage()` cannot see it — it counts rows. So the check that the service
// would have made is made here instead, against the manifest this repository
// ships, and a setting renamed or retyped in `examples/addons/pageviews` fails
// this test rather than the demo.
//
// The manifest is read as `addon.json.in` — the template the image's build stage
// substitutes the digest into. Only `@SHA256@` is substituted and it is a JSON
// string either way, so the template parses as the manifest it becomes.
func TestTheDemosAddonSettingsAreTheOnesItsManifestDeclares(t *testing.T) {
	// This package sits two levels down: cmd/lctl.
	raw, err := os.ReadFile(filepath.Join("..", "..",
		"examples", "addons", demoAddonName, "addon.json.in"))
	if err != nil {
		t.Fatalf("the demo's sample add-on manifest is not where the seeder's "+
			"comment says it is: %v", err)
	}
	var m addon.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse the sample manifest: %v", err)
	}
	if m.Name != demoAddonName {
		t.Fatalf("the manifest names %q and the seeder writes rows for %q",
			m.Name, demoAddonName)
	}

	declared := map[string]addon.Setting{}
	for _, s := range m.Settings {
		declared[s.Name] = s
	}
	for _, seeded := range demoAddonSettings {
		s, ok := declared[seeded.Name]
		if !ok {
			t.Errorf("the seeder configures %q, which %s's manifest does not declare. "+
				"config_get answers only for a declared setting and the manager renders "+
				"only declared settings, so this row is written and never read",
				seeded.Name, demoAddonName)
			continue
		}
		if s.Type == addon.SettingSecret {
			t.Errorf("the seeder writes a value for the secret %q. Not set is the state "+
				"a secret field has to render correctly, and the demo is where somebody "+
				"looks at it", seeded.Name)
		}
		switch s.Type {
		case addon.SettingToggle:
			if seeded.Value != "true" && seeded.Value != "false" {
				t.Errorf("%q is a toggle and is seeded as %q", seeded.Name, seeded.Value)
			}
		case addon.SettingSelect:
			if !slices.Contains(s.Options, seeded.Value) {
				t.Errorf("%q is seeded as %q, which is not one of its declared options %v",
					seeded.Name, seeded.Value, s.Options)
			}
		case addon.SettingText, addon.SettingSecret:
			// Text admits anything inside the byte bound, which nothing the seeder
			// writes approaches; the secret case is refused above rather than
			// value-checked, because the demo must not set one at all.
		}
	}

	// And the other direction, which is what makes the demo's own claim honest:
	// the seeder says "three of four settings set" and the page draws that figure
	// from the manifest.
	if len(m.Settings) != 4 || len(demoAddonSettings) != 3 {
		t.Errorf("the sample declares %d settings and the seeder sets %d; the seeder's "+
			"own log line says three of four, and the manager's list column says the "+
			"same arithmetic", len(m.Settings), len(demoAddonSettings))
	}
}

// demoAddonBuild caches the built sample module across the tests that load it,
// so two coverage tests in one run pay for one `go build`.
var demoAddonBuild struct {
	sync.Mutex
	code []byte
	err  error
}

// buildTheDemosAddon compiles `examples/addons/pageviews` the way the image's
// build stage compiles it.
//
// The command, the flags and the environment are the Dockerfile's, deliberately
// copied rather than approximated: `-buildmode=c-shared` is what makes the entry
// point `_initialize`, and a module built without it is not a reactor and never
// runs its package initialization.
func buildTheDemosAddon(t *testing.T) []byte {
	t.Helper()
	demoAddonBuild.Lock()
	defer demoAddonBuild.Unlock()
	if demoAddonBuild.code != nil || demoAddonBuild.err != nil {
		if demoAddonBuild.err != nil {
			t.Fatalf("the demo's sample add-on will not build: %v", demoAddonBuild.err)
		}
		return demoAddonBuild.code
	}
	out := filepath.Join(t.TempDir(), demoAddonName+".wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out,
		"./examples/addons/"+demoAddonName)
	// This package sits at cmd/lctl, and the build path above is repository-root
	// relative — the same shape test/integration's fixture builder uses, and for
	// the same reason its comment gives.
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if log, err := cmd.CombinedOutput(); err != nil {
		demoAddonBuild.err = fmt.Errorf("%w: %s", err, log)
		t.Fatalf("the demo's sample add-on will not build: %v", demoAddonBuild.err)
	}
	code, err := os.ReadFile(out)
	if err != nil {
		demoAddonBuild.err = err
		t.Fatalf("the demo's sample add-on built and is not readable: %v", err)
	}
	demoAddonBuild.code = code
	return code
}

// loadTheDemosAddon runs the demo's sample add-on against the coverage test's
// database, the way the demo instance runs it.
//
// **This is what makes the M68 coverage row an assertion about the add-on rather
// than about three rows somebody inserted.** `lctl demo` writes `addon_settings`
// unconditionally — it has no host — so a row counting those rows passes on a
// demo whose module never loaded and whose manager page is an empty table. What
// cannot be faked is the schema: `addon_pageviews` is created by the host at load
// (M63's EnsureAddonSchema), and `addon_pageviews.views` is created by the
// module's own `init` through `storage_exec`. A row asserting the table therefore
// asserts that the manifest parsed, the digest matched, the grants were honoured,
// wazero compiled and instantiated the module, and the guest's first host call
// reached Postgres.
//
// The directory is assembled here rather than read from `/addons`, because the
// image is not built when this test runs and the digest in `addon.json.in` is a
// placeholder the image's build stage substitutes. Doing the substitution the
// same way is also the check that the placeholder is still the only thing between
// the shipped manifest and a real one.
//
// A `degrade`-class add-on that will not load does **not** fail `addon.Open` —
// that is the whole of its failure class — so a broken module surfaces here as a
// missing table and the coverage row is what says so.
func loadTheDemosAddon(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	code := buildTheDemosAddon(t)

	raw, err := os.ReadFile(filepath.Join("..", "..",
		"examples", "addons", demoAddonName, "addon.json.in"))
	if err != nil {
		t.Fatalf("read the sample manifest: %v", err)
	}
	sum := sha256.Sum256(code)
	manifest := strings.ReplaceAll(string(raw), "@SHA256@", hex.EncodeToString(sum[:]))
	if strings.Contains(manifest, "@SHA256@") {
		t.Fatal("the sample manifest still carries an unsubstituted digest placeholder")
	}

	root := t.TempDir()
	dir := filepath.Join(root, demoAddonName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, demoAddonName+".wasm"), code, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, addon.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	var sink strings.Builder
	h, err := addon.Open(context.Background(), addon.Options{
		Dir:     root,
		DB:      pool,
		DSN:     pool.Config().ConnString(),
		Metrics: observability.NewMetrics(),
		Logger: slog.New(slog.NewTextHandler(&sink,
			&slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("open a host over the demo's add-on: %v\n%s", err, sink.String())
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	t.Logf("the demo's add-on loaded:\n%s", sink.String())
}
