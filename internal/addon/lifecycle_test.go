package addon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// M67's tests. What they are about is the *lifecycle* — what is on disk, what is
// in the set, and what is released — and not the permission gate, which needs an
// identity only internal/auth can mint and is asserted in test/integration.

// lifecycleHost opens a host on an empty directory, with an auditor watching.
func lifecycleHost(t *testing.T) (*Host, string, *recordingAuditor, *observability.Metrics) {
	t.Helper()
	dir := t.TempDir()
	rec := &recordingAuditor{}
	metrics := observability.NewMetrics()
	h, err := Open(t.Context(), Options{
		Dir:                 dir,
		Metrics:             metrics,
		Audit:               rec,
		Logger:              slog.New(slog.NewTextHandler(&logSink{}, nil)),
		InlineDeadline:      testInlineDeadline,
		InstantiateDeadline: testInstantiateDeadline,
	})
	if err != nil {
		t.Fatalf("opening an empty add-ons directory: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h, dir, rec, metrics
}

// recordingAuditor keeps every event, so a test can assert what an operator would
// read rather than that a call happened.
type recordingAuditor struct {
	mu     sync.Mutex
	events []audit.Event
}

// **The context is honoured, and that is the whole reason this fake is not a
// slice append.** The real recorder is an insert through pgx, which refuses a
// cancelled context and writes nothing; a fake that ignored the context would
// record an event the product would have dropped, and
// [TestRemovalAuditsAndReleasesWhenTheCallerHasHungUp] would then pass whatever
// internal/addon does with the caller's context.
func (r *recordingAuditor) Record(ctx context.Context, _ *auth.Identity, e audit.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recordingAuditor) all() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.Event(nil), r.events...)
}

// uploadFor is the pair a caller sends: a manifest that describes the code, and
// the code.
func uploadFor(t *testing.T, name string, class FailureClass, permissions ...string,
) (InstallRequest, Manifest) {
	t.Helper()
	code := fixture(t, "minimal")
	m := manifestFor(name, class, code)
	m.Permissions = permissions
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return InstallRequest{Manifest: raw, Module: code}, m
}

// --- install ------------------------------------------------------------------

// The whole of the arrival: nothing is running, an upload is verified and
// written, and the add-on is serving without a restart.
func TestAnUploadedAddonRunsWithoutARestart(t *testing.T) {
	h, dir, rec, metrics := lifecycleHost(t)
	if h.Len() != 0 {
		t.Fatalf("the fixture host started with %d add-ons", h.Len())
	}

	req, m := uploadFor(t, "arrival", ClassDegrade)
	out, err := h.install(t.Context(), nil, req)
	if err != nil {
		t.Fatalf("installing: %v", err)
	}

	if out.Name != "arrival" || out.SHA256 != m.SHA256 {
		t.Errorf("the answer describes %+v, want arrival at %s", out, m.SHA256)
	}
	if h.Len() != 1 {
		t.Errorf("the host runs %d add-ons after an install", h.Len())
	}
	// On disk, in the directory the operator's own route reads, so the next boot
	// finds it without this process being involved.
	for _, name := range []string{ManifestFile, m.Module} {
		if _, err := os.Stat(filepath.Join(dir, "arrival", name)); err != nil {
			t.Errorf("the add-ons directory has no %s after an install: %v", name, err)
		}
	}
	// The manifest is the bytes that were uploaded, not a re-encoding of them.
	written, err := os.ReadFile(filepath.Join(dir, "arrival", ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(req.Manifest) {
		t.Errorf("the manifest on disk is not the manifest that was uploaded:\n%s\n%s",
			written, req.Manifest)
	}
	if got := scrape(t, metrics); !strings.Contains(got,
		`linkctrl_addon_loads_total{addon="arrival",outcome="loaded"} 1`) {
		t.Errorf("the install published no load: %s", addonSeries(got))
	}
	assertAudited(t, rec, audit.ActionAddonInstalled, "arrival", m.SHA256)
}

// The digest is checked against the uploaded bytes, and the refusal happens
// before anything reaches the directory this instance executes from.
func TestAModuleThatDoesNotMatchItsManifestIsNeverWritten(t *testing.T) {
	h, dir, rec, _ := lifecycleHost(t)

	req, _ := uploadFor(t, "liar", ClassDegrade)
	req.Module = append([]byte{0x00}, req.Module...)

	_, err := h.install(t.Context(), nil, req)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a mismatched module answered %v, want a validation refusal", err)
	}
	if !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("the refusal does not say what it compared: %v", err)
	}
	assertDirectoryHolds(t, dir)
	if events := rec.all(); len(events) != 0 {
		t.Errorf("a refused install wrote %d audit records", len(events))
	}
}

// A manifest declaring DDL files is refused in a sentence rather than by a load
// failure naming a path nobody uploaded.
func TestAnAddonShippingMigrationsIsRefusedWithAReason(t *testing.T) {
	h, dir, _, _ := lifecycleHost(t)

	code := fixture(t, "minimal")
	m := manifestFor("ddl", ClassDegrade, code)
	m.Permissions = []string{"storage.own_schema"}
	m.Migrations = []MigrationFile{{File: "0001_init.sql", SHA256: digest([]byte("create table t()"))}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	_, err = h.install(t.Context(), nil, InstallRequest{Manifest: raw, Module: code})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a manifest with migrations answered %v, want a validation refusal", err)
	}
	if !strings.Contains(err.Error(), "LINKCTRL_ADDONS_DIR") {
		t.Errorf("the refusal does not say how to install such an add-on: %v", err)
	}
	assertDirectoryHolds(t, dir)
}

// One name, one add-on. Replacing is a removal and an install, which is what
// m67.md puts upgrade-in-place out of scope for.
func TestInstallingOverARunningAddonIsRefused(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	req, _ := uploadFor(t, "twice", ClassDegrade)
	if _, err := h.install(t.Context(), nil, req); err != nil {
		t.Fatalf("the first install: %v", err)
	}
	_, err := h.install(t.Context(), nil, req)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a second install answered %v, want a conflict", err)
	}
	if h.Len() != 1 {
		t.Errorf("the host runs %d add-ons after a refused second install", h.Len())
	}
}

// M60's name-collision rule, applied to the runtime path. Two names in a
// `name + "_"` prefix relation share a cookie namespace and a settings namespace,
// so the boot check refuses both; here the *arrival* is refused and the add-on
// already serving is left alone — an API that could unload a running
// authentication provider by uploading a badly-named module would be a denial of
// service against what is already installed.
func TestInstallingANameThatCollidesWithARunningAddonIsRefused(t *testing.T) {
	h, dir, _, _ := lifecycleHost(t)
	if _, err := h.install(t.Context(), nil, uploadOf(t, "oidc")); err != nil {
		t.Fatalf("installing the first add-on: %v", err)
	}

	_, err := h.install(t.Context(), nil, uploadOf(t, "oidc_x"))
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("installing a colliding name answered %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), "oidc") {
		t.Errorf("the refusal does not name what it collides with: %v", err)
	}
	if h.Len() != 1 {
		t.Errorf("the host runs %d add-ons; the one already serving should be untouched", h.Len())
	}
	assertDirectoryHolds(t, dir, "oidc")
	// And the relation holds the other way round too, which is what makes it a
	// relation rather than a rule about which name is longer.
	if _, err := h.remove(t.Context(), nil, "oidc"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.install(t.Context(), nil, uploadOf(t, "oidc_x")); err != nil {
		t.Fatalf("installing oidc_x with nothing beside it: %v", err)
	}
	if _, err := h.install(t.Context(), nil, uploadOf(t, "oidc")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("installing the shorter name beside the longer answered %v", err)
	}
}

// uploadOf is uploadFor without the manifest, for the tests that only need the
// request.
func uploadOf(t *testing.T, name string) InstallRequest {
	t.Helper()
	req, _ := uploadFor(t, name, ClassDegrade)
	return req
}

// A directory the host did not load is an operator's, and installing over it
// would destroy whatever they were about to look at.
func TestInstallingOverADirectoryThisHostDidNotLoadIsRefused(t *testing.T) {
	h, dir, _, _ := lifecycleHost(t)
	if err := os.MkdirAll(filepath.Join(dir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken", "note.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	req, _ := uploadFor(t, "broken", ClassDegrade)
	_, err := h.install(t.Context(), nil, req)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("installing over an unloaded directory answered %v, want a conflict", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "broken", "note.txt")); err != nil {
		t.Errorf("the refused install destroyed what was already there: %v", err)
	}
}

// The collision check reads what the directory *claims*, not what loaded.
//
// A directory whose manifest parses and names its own directory claims that name
// at boot even when the add-on behind it is then refused — [claimants] says so in
// as many words. So `oidc` with a wrong digest is absent from the running set and
// still owns the namespace, and an install of `oidc_x` beside it has to be
// refused here: allowed, it arranges a boot at which [nameCollisions] refuses
// *both*, which for a `required` add-on is an instance that will not start,
// reached through this API by an operator doing nothing wrong.
//
// The test above covers a directory with no manifest, which the exact-name check
// catches on its own. This one is the parsed-but-unloaded case, which nothing
// caught until the check read [Host.collidingNames]'s set.
func TestInstallingANameClaimedByADirectoryThatDidNotLoadIsRefused(t *testing.T) {
	dir := t.TempDir()
	code := fixture(t, "minimal")
	// Valid, `degrade`, and describing a module it does not hash to: it parses, so
	// it claims `oidc`; it fails verification, so nothing runs and the instance
	// carries on without it.
	claiming := manifestFor("oidc", ClassDegrade, code)
	claiming.SHA256 = digest([]byte("some other module entirely"))
	install(t, dir, claiming, code)

	h, err := openHost(t, dir, observability.NewMetrics())
	if err != nil {
		t.Fatalf("opening a directory holding one add-on with a wrong digest: %v", err)
	}
	if h.Len() != 0 {
		t.Fatalf("the host loaded %d add-ons; the digest does not match and none may", h.Len())
	}

	_, err = h.install(t.Context(), nil, uploadOf(t, "oidc_x"))
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("installing beside a directory that claims the colliding name answered %v, "+
			"want a conflict", err)
	}
	if !strings.Contains(err.Error(), `"oidc"`) {
		t.Errorf("the refusal does not name what it collides with: %v", err)
	}
	if h.Len() != 0 {
		t.Errorf("the host runs %d add-ons after a refused install", h.Len())
	}
	// Nothing arrived, so the boot the check protects is the boot that was there
	// before it: one directory, refused on its own terms and on nobody else's.
	assertDirectoryHolds(t, dir, "oidc")
}

// The mirror, and it is what keeps the widened check from being a way to refuse
// an operator an install they are entitled to. A directory discovery ignores
// claims nothing: an unparseable manifest, a manifest naming somewhere else, and
// this host's own staging area are all invisible to the collision check, exactly
// as they are to boot.
// Written after the host is open, because two of the three stop a boot outright
// and the point here is what the *install* check does with them.
func TestADirectoryDiscoveryIgnoresDoesNotRefuseAnInstall(t *testing.T) {
	h, dir, _, _ := lifecycleHost(t)
	// A manifest that parses as nothing.
	if err := os.MkdirAll(filepath.Join(dir, "oidc_broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oidc_broken", ManifestFile),
		[]byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// One that parses and names a directory it is not sitting in — a mis-installed
	// copy, which claimants documents as the case that must not take a neighbour
	// down with it.
	install(t, dir, manifestFor("oidc_typo", ClassDegrade, fixture(t, "minimal")), nil)
	if err := os.Rename(filepath.Join(dir, "oidc_typo"),
		filepath.Join(dir, "oidc_typoo")); err != nil {
		t.Fatal(err)
	}
	// And a file, which is an operator's note and not an add-on either way.
	if err := os.WriteFile(filepath.Join(dir, "oidc_notes"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.install(t.Context(), nil, uploadOf(t, "oidc")); err != nil {
		t.Fatalf("an install was refused by entries discovery ignores: %v", err)
	}
	if h.Len() != 1 {
		t.Errorf("the host runs %d add-ons, want the one just installed", h.Len())
	}
}

// A module that will not load leaves nothing behind: the directory is taken back
// off disk, so the next boot does not meet a refusal the operator was already
// told about — and, for a `required` add-on, an instance that will not start.
func TestAnInstallThatWillNotLoadLeavesNothingOnDisk(t *testing.T) {
	h, dir, _, metrics := lifecycleHost(t)

	code := fixture(t, "minimal")
	m := manifestFor("wrongabi", ClassRequired, code)
	m.ABIVersion = 99
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	_, err = h.install(t.Context(), nil, InstallRequest{Manifest: raw, Module: code})
	if err == nil {
		t.Fatal("a module built against an unknown ABI installed")
	}
	assertDirectoryHolds(t, dir)
	if h.Len() != 0 {
		t.Errorf("the host runs %d add-ons after a failed install", h.Len())
	}
	// The refusal is counted under the same outcome a boot-time refusal is, so an
	// operator watching the series does not have two vocabularies.
	if got := scrape(t, metrics); !strings.Contains(got, `outcome="abi_unsupported"`) {
		t.Errorf("the failed install published no refusal: %s", addonSeries(got))
	}
}

// The atomic step, driven by stopping between the two halves rather than by
// reasoning about them: after staging and before the rename there is nothing in
// the discovery set, and a host opened on the directory finds no add-on.
func TestAnInstallKilledBeforeTheRenameLeavesTheOldWorld(t *testing.T) {
	h, dir, _, _ := lifecycleHost(t)
	req, m := uploadFor(t, "halfway", ClassRequired)

	staged, err := h.stage(m, req.Manifest, req.Module)
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	// Everything an install writes is written, and the rename has not happened.
	// This is the state a crash between the two leaves.
	for _, name := range []string{ManifestFile, m.Module} {
		if _, err := os.Stat(filepath.Join(staged, name)); err != nil {
			t.Fatalf("staging did not write %s: %v", name, err)
		}
	}

	next, err := Open(t.Context(), Options{Dir: dir})
	if err != nil {
		t.Fatalf("a boot over a half-written install failed: %v", err)
	}
	t.Cleanup(func() { _ = next.Close(context.Background()) })
	if next.Len() != 0 {
		t.Errorf("a boot over a half-written install loaded %d add-ons", next.Len())
	}
	// And the staging area is swept, so the leftover is not permanent.
	if _, err := os.Stat(filepath.Join(dir, stagingName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging area survived a boot: %v", err)
	}
}

// The other side of the same step: after the rename the file set is whole, so a
// crash there leaves the new world and the next boot runs the add-on.
func TestAnInstallKilledAfterTheRenameLeavesTheNewWorld(t *testing.T) {
	h, dir, _, _ := lifecycleHost(t)
	req, m := uploadFor(t, "landed", ClassRequired)

	staged, err := h.stage(m, req.Manifest, req.Module)
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if err := os.Rename(staged, filepath.Join(dir, "landed")); err != nil {
		t.Fatalf("the rename: %v", err)
	}
	// Nothing has been loaded into *this* host, which is exactly the state a crash
	// between the rename and the load leaves.
	if h.Len() != 0 {
		t.Fatalf("the host loaded an add-on nobody asked it to")
	}

	next, err := Open(t.Context(), Options{Dir: dir})
	if err != nil {
		t.Fatalf("a boot over a completed install failed: %v", err)
	}
	t.Cleanup(func() { _ = next.Close(context.Background()) })
	if next.Len() != 1 {
		t.Errorf("a boot over a completed install loaded %d add-ons, want 1", next.Len())
	}
}

// --- removal ------------------------------------------------------------------

// The whole of the departure: a running add-on stops running, its files leave the
// directory, and the act is recorded.
func TestARemovedAddonStopsRunningAndLeavesTheDirectory(t *testing.T) {
	h, dir, rec, metrics := lifecycleHost(t)
	req, m := uploadFor(t, "departure", ClassDegrade)
	if _, err := h.install(t.Context(), nil, req); err != nil {
		t.Fatalf("installing: %v", err)
	}

	out, err := h.remove(t.Context(), nil, "departure")
	if err != nil {
		t.Fatalf("removing: %v", err)
	}
	if out.Draining {
		t.Error("a removal with nothing in flight reported that it interrupted something")
	}
	if h.Len() != 0 {
		t.Errorf("the host still runs %d add-ons after a removal", h.Len())
	}
	assertDirectoryHolds(t, dir)
	// The identity series is gone, because it is a statement in the present tense.
	if got := scrape(t, metrics); strings.Contains(got, `linkctrl_addon_info{addon="departure"`) {
		t.Errorf("a removed add-on still publishes an identity: %s", addonSeries(got))
	}
	// The counter is not, because it is a statement about the past.
	if got := scrape(t, metrics); !strings.Contains(got,
		`linkctrl_addon_loads_total{addon="departure",outcome="loaded"} 1`) {
		t.Errorf("removing an add-on erased the record that it was ever loaded: %s",
			addonSeries(got))
	}
	assertAudited(t, rec, audit.ActionAddonRemoved, "departure", m.SHA256)
}

// Removing something that is not installed is a 404 rather than a success that
// did nothing.
func TestRemovingWhatIsNotInstalledIsNotFound(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	if _, err := h.remove(t.Context(), nil, "nosuch"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removing an absent add-on answered %v, want not found", err)
	}
}

// m67.md's `required` bullet, driven end to end: an add-on whose failure class
// would stop a boot is removed, and the boot it was required for starts.
func TestARemovedRequiredAddonCannotBrickTheNextBoot(t *testing.T) {
	h, dir, _, _ := lifecycleHost(t)
	req, _ := uploadFor(t, "mandatory", ClassRequired)
	if _, err := h.install(t.Context(), nil, req); err != nil {
		t.Fatalf("installing a required add-on: %v", err)
	}
	// The premise: while it is installed, a boot that cannot load it stops. Broken
	// by truncating the module, which is a checksum mismatch on a `required`
	// add-on — the exact failure the class is about.
	if err := os.WriteFile(filepath.Join(dir, "mandatory", "mandatory.wasm"),
		[]byte("not a module"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken, err := Open(t.Context(), Options{Dir: dir})
	if err == nil {
		_ = broken.Close(context.Background())
		t.Fatal("a boot over a broken required add-on started; the premise of this test is gone")
	}

	if _, err := h.remove(t.Context(), nil, "mandatory"); err != nil {
		t.Fatalf("removing the required add-on: %v", err)
	}
	next, err := Open(t.Context(), Options{Dir: dir})
	if err != nil {
		t.Fatalf("a boot after removing a required add-on stopped: %v", err)
	}
	t.Cleanup(func() { _ = next.Close(context.Background()) })
	if next.Len() != 0 {
		t.Errorf("a boot after the removal loaded %d add-ons", next.Len())
	}
}

// M66.5's seam, consumed. The pool is warmed by a real redirect, and after the
// removal nothing of that add-on is held anywhere.
func TestRemovalDrainsTheAddonsPool(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	req, _ := uploadFor(t, "pooled", ClassDegrade, PermissionRedirectInline, "config.read")
	if _, err := h.install(t.Context(), nil, req); err != nil {
		t.Fatalf("installing: %v", err)
	}
	if !h.HasInline() {
		t.Fatal("an installed inline add-on is not on the redirect path")
	}

	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	if idle := h.idleInstanceCount(); idle != 1 {
		t.Fatalf("the redirect left %d instances in the pool, want 1", idle)
	}

	if _, err := h.remove(t.Context(), nil, "pooled"); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if idle := h.idleInstanceCount(); idle != 0 {
		t.Errorf("removal left %d instances of the add-on in a pool", idle)
	}
	if h.HasInline() {
		t.Error("a removed add-on is still on the redirect path")
	}
	for key := range h.current().pools {
		if strings.HasPrefix(key, "pooled\x00") {
			t.Errorf("a removed add-on still has a pool: %q", key)
		}
	}
	if h.hostState("pooled") != nil {
		t.Error("a removed add-on can still be answered by the ABI")
	}
}

// The caller hangs up in the middle of a removal, and the removal is complete
// anyway: nothing of the add-on is left instantiated, and the act is in the log.
//
// **Ordinary rather than exotic, which is what makes this a test and not a
// note.** Removal holds the request open for up to [removeGrace] waiting for
// guest calls already inside the module to finish, so the window in which a
// client can time out mid-removal is one this package chose the length of. And
// everything past the rename is unretryable: the add-on is out of the set and
// off the disk, so a close that declined to run on a cancelled context leaves
// resident memory nothing will ever come back for, and a record that declined to
// be written is an act with no trace of it.
//
// [TestRepeatedInstallAndRemovalDoesNotGrowResidentMemory] cannot see any of
// this — it removes on a live context, so it measures the path a caller who
// waits takes.
func TestRemovalAuditsAndReleasesWhenTheCallerHasHungUp(t *testing.T) {
	h, _, rec, _ := lifecycleHost(t)
	req, m := uploadFor(t, "hangup", ClassDegrade, PermissionRedirectInline, "config.read")
	if _, err := h.install(t.Context(), nil, req); err != nil {
		t.Fatalf("installing: %v", err)
	}
	// One redirect, so the pool has an instance for the release path to release.
	h.Inline(t.Context(), decisionFor("quiet", "https://example.test/"))
	if idle := h.idleInstanceCount(); idle != 1 {
		t.Fatalf("the redirect left %d instances in the pool, want 1", idle)
	}
	// Held before the set stops naming it: what is asserted below is the state of
	// the runtime object, not the host's view of which add-ons exist.
	gone := h.current().loaded[0]

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.remove(ctx, nil, "hangup"); err != nil {
		t.Fatalf("removing under a cancelled context: %v", err)
	}

	if gone.module == nil || !gone.module.IsClosed() {
		t.Error("the add-on's module is still instantiated after a removal whose " +
			"caller hung up; it is out of the set and off the disk, so nothing " +
			"will close it again")
	}
	if idle := h.idleInstanceCount(); idle != 0 {
		t.Errorf("a removal whose caller hung up left %d pooled instances", idle)
	}
	if h.hostState("hangup") != nil {
		t.Error("a removed add-on can still be answered by the ABI")
	}
	assertAudited(t, rec, audit.ActionAddonRemoved, "hangup", m.SHA256)
}

// The leak bound. Install and remove the same module repeatedly and the memory
// this process holds afterwards does not grow with the number of cycles.
//
// **The tolerance is stated rather than tight**, and what makes the assertion
// mean something is the ratio rather than the number: the module is 1.8 MB of
// wasm and each load makes an instance holding 8 MiB of guest memory, so a host
// that retained one per cycle would be most of a hundred megabytes up after ten,
// and one that merely fragments is a few. Sixteen MiB sits between those two
// outcomes with room on both sides and does not depend on the machine.
//
// **What this does and does not catch, measured rather than assumed.** Removing
// `l.module.Close` reddens it — wazero refuses the next install outright, because
// a module of that name is still instantiated, which is the leak announcing
// itself. Removing `l.compiled.Close` does **not**: forty cycles without it moved
// the resident set by −208 KiB, because wazero releases a compiled module's
// mapping from a finalizer once nothing references it. So closing the compiled
// form is promptness rather than a leak fix, and this test is honest about
// bounding what the runtime *holds* rather than about every line unload calls.
// Said here because the opposite belief is the one a reader would otherwise take
// from the assertion.
func TestRepeatedInstallAndRemovalDoesNotGrowResidentMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("a memory measurement over ten wasm compilations")
	}
	h, _, _, _ := lifecycleHost(t)
	req, _ := uploadFor(t, "cycling", ClassDegrade)

	cycle := func() {
		if _, err := h.install(t.Context(), nil, req); err != nil {
			t.Fatalf("installing: %v", err)
		}
		if _, err := h.remove(t.Context(), nil, "cycling"); err != nil {
			t.Fatalf("removing: %v", err)
		}
	}
	// One outside the measurement, for the reason every measurement in this package
	// takes one: the first compilation of a module pays for whatever the runtime
	// caches on the way and is not what a steady state costs.
	cycle()

	const (
		cycles    = 10
		tolerance = 16 << 20
	)
	before := heldBytes(t)
	for range cycles {
		cycle()
	}
	after := heldBytes(t)

	grew := int64(after) - int64(before)
	t.Logf("%d install/remove cycles: %d KiB held before, %d KiB after, %+d KiB",
		cycles, before/1024, after/1024, grew/1024)
	if grew > tolerance {
		t.Errorf("ten install/remove cycles grew the heap by %d KiB, past the %d KiB "+
			"tolerance; a compiled module or an instance is not being closed",
			grew/1024, tolerance/1024)
	}
	if h.Len() != 0 {
		t.Errorf("the host runs %d add-ons after the cycles", h.Len())
	}
}

// heldBytes is this process's resident set, which is the measure the bullet
// names and the only one that would have caught the defect it is about.
//
// **`runtime.MemStats.HeapAlloc` is not this, and it was tried first.** wazero
// compiles a module into memory it maps itself, outside the Go heap entirely, so
// a host that never closed a compiled module measured *smaller* after ten cycles
// than before — the test passed under a sabotage that removed the exact line it
// exists to hold. Resident set is what moves when an `mmap` is not unmapped.
//
// Linux only, and the test skips elsewhere rather than measuring something
// weaker: this product ships in a container and `/proc/self/status` is where the
// number is.
func heldBytes(t *testing.T) uint64 {
	t.Helper()
	// The Go side returned to the operating system first, so what is left to
	// measure is what the runtime under test is holding rather than what the
	// allocator has not got round to releasing.
	runtime.GC()
	debug.FreeOSMemory()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc/self/status to read a resident set from: %v", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		rest, ok := strings.CutPrefix(line, "VmRSS:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 || fields[1] != "kB" {
			t.Fatalf("VmRSS is not in the shape this reads: %q", line)
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			t.Fatalf("VmRSS is not a number: %q", line)
		}
		return kb * 1024
	}
	t.Skip("/proc/self/status carries no VmRSS")
	return 0
}

// An invocation already inside a guest call finishes; the removal waits for it
// and says it did not have to interrupt anything.
func TestRemovalWaitsForAnInvocationAlreadyRunning(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	req, _ := uploadFor(t, "busy", ClassDegrade)
	if _, err := h.install(t.Context(), nil, req); err != nil {
		t.Fatalf("installing: %v", err)
	}
	l := h.current().loaded[0]

	// Standing in for an invocation, at exactly the point the serving paths hold
	// the counter: between enter and leave. Driving a real guest call and removing
	// it mid-flight is the integration suite's, where a request can be held open;
	// what this asserts is the discipline, which is this package's.
	if !l.live.enter() {
		t.Fatal("a running add-on refused an invocation")
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(released)
		l.live.leave()
	}()

	out, err := h.remove(t.Context(), nil, "busy")
	if err != nil {
		t.Fatalf("removing: %v", err)
	}
	select {
	case <-released:
	default:
		t.Error("the removal returned before the invocation it should have waited for")
	}
	if out.Draining {
		t.Error("a removal that waited successfully reported that it interrupted something")
	}
}

// And nothing new is admitted once the set has been swapped, which is what makes
// the wait finite rather than a race against arriving traffic.
func TestASealedAddonAdmitsNoFurtherInvocations(t *testing.T) {
	live := newAddonLive()
	if !live.enter() {
		t.Fatal("a fresh counter refused the first invocation")
	}
	quiet := live.seal()
	if live.enter() {
		t.Error("a sealed add-on admitted an invocation")
	}
	select {
	case <-quiet:
		t.Fatal("a counter with one invocation in flight reported quiet")
	default:
	}
	live.leave()
	select {
	case <-quiet:
	case <-time.After(time.Second):
		t.Error("the last invocation left and the counter never reported quiet")
	}
}

// --- the absence ----------------------------------------------------------------

// An instance that configured no add-ons directory has no lifecycle, and says so
// as an unavailability rather than as a 404: the caller did nothing wrong.
func TestALifecycleOnAnInstanceWithNoDirectory(t *testing.T) {
	var h *Host
	req, _ := uploadFor(t, "nowhere", ClassDegrade)
	if _, err := h.install(t.Context(), nil, req); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("installing on a nil host answered %v, want unavailable", err)
	}
	if _, err := h.remove(t.Context(), nil, "nowhere"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("removing on a nil host answered %v, want unavailable", err)
	}
}

// The gate itself, from the side a unit test can reach: no identity holds
// `addons.manage`, and a nil one certainly does not.
func TestTheLifecycleIsRefusedWithoutThePermission(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	req, _ := uploadFor(t, "denied", ClassDegrade)
	if _, err := h.Install(t.Context(), nil, req); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("installing without the permission answered %v, want forbidden", err)
	}
	if _, err := h.Remove(t.Context(), nil, "denied"); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("removing without the permission answered %v, want forbidden", err)
	}
	if !strings.Contains(auth.PermAddonsManage, "addons.manage") {
		t.Fatal("the scope this milestone is gated on has been renamed")
	}
}

// --- helpers --------------------------------------------------------------------

// assertDirectoryHolds fails unless the add-ons directory holds nothing an
// operator did not put there.
func assertDirectoryHolds(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for _, e := range entries {
		if isStaging(e.Name()) || want[e.Name()] {
			continue
		}
		t.Errorf("the add-ons directory holds %q, and it should hold %v", e.Name(), names)
	}
}

// assertAudited fails unless one record names the action, the module and the
// digest — the three m67.md asks for.
func assertAudited(t *testing.T, rec *recordingAuditor, action, module, sha string) {
	t.Helper()
	for _, e := range rec.all() {
		if e.Action != action {
			continue
		}
		if !e.InstanceWide {
			t.Errorf("%s was recorded against a tenant; an add-on belongs to the instance", action)
		}
		if e.Metadata["module"] != module {
			t.Errorf("%s names module %v, want %q", action, e.Metadata["module"], module)
		}
		if e.Metadata["sha256"] != sha {
			t.Errorf("%s names digest %v, want %q", action, e.Metadata["sha256"], sha)
		}
		if e.Metadata["version"] == nil {
			t.Errorf("%s names no version", action)
		}
		return
	}
	t.Errorf("no %s record was written; got %+v", action, rec.all())
}

// addonSeries is the add-on lines of a scrape, so a failure prints what there was
// rather than the whole registry.
func addonSeries(scrape string) string {
	var out []string
	for _, line := range strings.Split(scrape, "\n") {
		if strings.HasPrefix(line, "linkctrl_addon_") {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return "no linkctrl_addon_ series at all"
	}
	return strings.Join(out, "\n")
}
