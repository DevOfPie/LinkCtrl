package addon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// fixtureDir is where `make addon-fixtures` puts the built modules, and
// fixtureSrc is where their sources live. Nothing is committed under fixtureDir:
// m60.md refuses a checked-in binary.
const (
	fixtureDir = "testdata/build"
	fixtureSrc = "testdata/modules"
)

// builds serializes the on-demand builds and remembers each outcome, so a second
// caller neither runs a second `go build` into the same output path nor reports
// success for a build the first caller watched fail.
var builds struct {
	sync.Mutex
	done map[string]error
}

// fixture reads a built test module, building it first if it is not there.
//
// It builds rather than skips, and it builds rather than failing with an
// instruction. A skip would be a green run of a package whose whole subject is
// loading wasm — the failure mode `make ci-integration` was fixed for at F144 —
// and an instruction to run `make addon-fixtures` only helps a caller that came
// through make. Two do not and cannot be made to: `.github/workflows/release.yml`
// runs `go test -race -count=1 ./...` directly at every tag push, and the CI
// `image` job runs a make target that has no Go toolchain of its own. Neither is
// this repository's to edit (F262), so the artifact is this file's to produce.
//
// `make addon-fixtures` still exists and is still what the Makefile's other
// targets depend on: building here is the fallback, so the normal path pays for
// it outside the test binary and a cold cache costs a few seconds inside it.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join(fixtureDir, name+".wasm")
	if code, err := os.ReadFile(path); err == nil && !stale(t, path, name) {
		return code
	}
	buildFixture(t, name, path)
	code, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the %s test module is still not readable after building it: %v", name, err)
	}
	return code
}

// stale reports whether a built module is older than something it was built from.
//
// This is the reader F266 named: fixture() consumed whatever was on disk without
// asking, so a module built before its sources changed was used as though it were
// current. That was theoretical while every fixture was one main.go nothing else
// fed — and it stopped being theoretical the moment the fixtures were rebuilt on
// top of the SDK (M61), which is a shared input that changes for reasons the
// fixture's own directory knows nothing about. The failure it produces is the
// worst available: a load that succeeds against yesterday's bytes, in the package
// whose whole subject is verifying that bytes are the bytes a manifest describes.
//
// The Makefile and the Taskfile carry the same dependency set for the same reason.
// Three mechanisms rather than one is F259's question and not this function's; what
// this function owes is that `go test` alone — which the release workflow and the
// CI image job both run — is not the weakest of the three.
func stale(t *testing.T, path, name string) bool {
	t.Helper()
	built, err := os.Stat(path)
	if err != nil {
		return true
	}
	for _, in := range fixtureInputs(t, name) {
		info, err := os.Stat(in)
		if err != nil {
			t.Fatalf("a fixture input disappeared while being read: %v", err)
		}
		if info.ModTime().After(built.ModTime()) {
			return true
		}
	}
	return false
}

// buildFixture runs the same command `make addon-fixtures` runs, from this
// package's directory. -buildmode=c-shared is load-bearing rather than a flag:
// it makes the entry point _initialize, so the module is a reactor that stays
// instantiated.
func buildFixture(t *testing.T, name, out string) {
	t.Helper()
	builds.Lock()
	defer builds.Unlock()
	if err, built := builds.done[name]; built {
		if err != nil {
			t.Fatalf("the %s test module would not build: %v", name, err)
		}
		return
	}
	err := os.MkdirAll(fixtureDir, 0o755)
	if err == nil {
		cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./"+filepath.Join(fixtureSrc, name))
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if log, buildErr := cmd.CombinedOutput(); buildErr != nil {
			err = fmt.Errorf("%w: %s", buildErr, log)
		}
	}
	if builds.done == nil {
		builds.done = map[string]error{}
	}
	builds.done[name] = err
	if err != nil {
		t.Fatalf("the %s test module is not built and will not build: %v\nbuild it by hand to see why: make addon-fixtures", name, err)
	}
}

// fixtureInputs is every file a test module is built from.
//
// Factored out of [stale] so that the test asserting staleness can measure against
// the same set rather than against a second copy of it.
func fixtureInputs(t *testing.T, name string) []string {
	t.Helper()
	var inputs []string
	for _, pattern := range []string{
		filepath.Join(fixtureSrc, name, "*.go"),
		// The SDK. Every fixture imports it, and a generated SDK changes whenever the
		// ABI does.
		filepath.Join("..", "..", "sdk", "*.go"),
	} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("the fixture input pattern %q is malformed: %v", pattern, err)
		}
		inputs = append(inputs, found...)
	}
	if len(inputs) == 0 {
		t.Fatalf("the %s test module has no inputs, so staleness cannot be decided", name)
	}
	return inputs
}

// newestFixtureInput is the most recent modification time across that set.
func newestFixtureInput(t *testing.T, name string) time.Time {
	t.Helper()
	var newest time.Time
	for _, in := range fixtureInputs(t, name) {
		info, err := os.Stat(in)
		if err != nil {
			t.Fatalf("a fixture input disappeared while being read: %v", err)
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// The F266 fix, asserted rather than assumed: fixture() consumes what is on disk,
// so what stops it consuming yesterday's bytes is this comparison and nothing else.
// The Makefile and the Taskfile carry the same input set; this is the third
// builder, and it is the one the release workflow and the CI image job reach.
func TestAFixtureOlderThanItsInputsIsStale(t *testing.T) {
	artifact := filepath.Join(t.TempDir(), "minimal.wasm")
	if err := os.WriteFile(artifact, []byte("not really wasm"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Older than every input, which is what a fixture built before an edit looks
	// like — including an edit to the SDK, which is an input no fixture's own
	// directory knows anything about.
	//
	// Relative to the newest input rather than to the clock. `time.Now().Add(-time.Hour)`
	// asserted this only while something had been edited within the hour: the test
	// passed for an hour after any edit to a fixture's source or to the SDK and then
	// began failing on its own, with nothing in the tree having changed.
	old := newestFixtureInput(t, "minimal").Add(-time.Second)
	if err := os.Chtimes(artifact, old, old); err != nil {
		t.Fatal(err)
	}
	if !stale(t, artifact, "minimal") {
		t.Error("a module older than its sources was treated as current")
	}

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(artifact, future, future); err != nil {
		t.Fatal(err)
	}
	if stale(t, artifact, "minimal") {
		t.Error("a module newer than every input was rebuilt anyway")
	}

	// A module whose own source is current but whose SDK is not. The SDK is the
	// input F266 stopped being theoretical over.
	sdkFiles, err := filepath.Glob(filepath.Join("..", "..", "sdk", "*.go"))
	if err != nil || len(sdkFiles) == 0 {
		t.Fatalf("the SDK has no files to compare against: %v", err)
	}
	between := time.Now()
	if err := os.Chtimes(artifact, between, between); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sdkFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().After(between) {
		t.Skip("the SDK was regenerated during this test run, so there is no order to assert")
	}
	if stale(t, artifact, "minimal") {
		t.Error("a module newer than the SDK was rebuilt anyway")
	}
}

func digest(code []byte) string {
	sum := sha256.Sum256(code)
	return hex.EncodeToString(sum[:])
}

// install writes one add-on into root, exactly as an operator would: a directory
// named for the add-on, holding addon.json and the module it describes.
func install(t *testing.T, root string, m Manifest, code []byte) {
	t.Helper()
	dir := filepath.Join(root, m.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if code != nil {
		if err := os.WriteFile(filepath.Join(dir, m.Module), code, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// manifestFor is a manifest that describes the given code honestly.
func manifestFor(name string, class FailureClass, code []byte) Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		Name:          name,
		Version:       "1.0.0",
		ABIVersion:    1,
		Module:        name + ".wasm",
		SHA256:        digest(code),
		FailureClass:  class,
	}
}

func openHost(t *testing.T, dir string, m *observability.Metrics) (*Host, error) {
	t.Helper()
	h, err := Open(context.Background(), Options{Dir: dir, Metrics: m})
	if h != nil {
		t.Cleanup(func() { _ = h.Close(context.Background()) })
	}
	return h, err
}

func scrape(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	return rec.Body.String()
}

// --- the unset case: every absence, asserted --------------------------------

// The falsifiable form of "zero added cost". Each of these is one of the four
// absences m60.md names; the fifth — no table — is in absence_test.go, because it
// is a claim about the repository rather than about a running host.
func TestUnsetDirMeansNoHostAtAll(t *testing.T) {
	metrics := observability.NewMetrics()

	before := runtime.NumGoroutine()
	h, err := Open(context.Background(), Options{Metrics: metrics})
	if err != nil {
		t.Fatalf("Open with no directory returned an error: %v", err)
	}
	if h != nil {
		t.Fatal("Open with no directory constructed a host; there is no runtime to construct")
	}

	// No goroutine. Compared after a settle, because an unrelated goroutine from
	// an earlier test can still be exiting — a *growth* here is what would matter
	// and a shrink is somebody else finishing.
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines went from %d to %d; an unconfigured host starts none", before, after)
	}

	// No metric series. A CounterVec with no observation publishes nothing, which
	// is exactly the property being relied on — so it is asserted rather than
	// assumed.
	if body := scrape(t, metrics); strings.Contains(body, "linkctrl_addon_") {
		t.Errorf("the scrape carries add-on series with no add-ons configured:\n%s",
			seriesLike(body, "linkctrl_addon_"))
	}

	// And every method is safe on the nil host, because that is the shipped state.
	if h.Len() != 0 || h.Addons() != nil {
		t.Error("a nil host reports add-ons")
	}
	if err := h.Close(context.Background()); err != nil {
		t.Errorf("closing a nil host: %v", err)
	}
}

// An empty directory is configured-but-empty, which is a different state from
// unconfigured: a runtime exists. It must still publish no series, or a scrape
// would suggest add-ons that are not there.
func TestAnEmptyDirectoryPublishesNoSeries(t *testing.T) {
	metrics := observability.NewMetrics()
	h, err := openHost(t, t.TempDir(), metrics)
	if err != nil {
		t.Fatalf("Open over an empty directory: %v", err)
	}
	if h == nil {
		t.Fatal("Open over an empty directory returned no host; the directory was configured")
	}
	if h.Len() != 0 {
		t.Errorf("%d add-ons loaded from an empty directory", h.Len())
	}
	if body := scrape(t, metrics); strings.Contains(body, "linkctrl_addon_") {
		t.Errorf("the scrape carries add-on series with nothing installed:\n%s",
			seriesLike(body, "linkctrl_addon_"))
	}
}

func seriesLike(body, prefix string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// --- a module loads ---------------------------------------------------------

func TestAModuleLoadsAndStaysInstantiated(t *testing.T) {
	code := fixture(t, "minimal")
	root := t.TempDir()
	install(t, root, manifestFor("minimal", ClassRequired, code), code)

	metrics := observability.NewMetrics()
	h, err := openHost(t, root, metrics)
	if err != nil {
		t.Fatalf("a valid add-on did not load: %v", err)
	}
	if h.Len() != 1 {
		t.Fatalf("%d add-ons loaded, want 1", h.Len())
	}

	got := h.Addons()[0]
	if got.Manifest.Name != "minimal" {
		t.Errorf("loaded %q", got.Manifest.Name)
	}

	// Instantiated, not merely accepted. A command module would have run main and
	// exited, leaving nothing to call — which is the whole reason the fixtures are
	// built with -buildmode=c-shared and started at _initialize.
	fn := got.Module().ExportedFunction("linkctrl_fixture_ok")
	if fn == nil {
		t.Fatal("the module exports nothing callable; it is not live")
	}
	res, err := fn.Call(context.Background())
	if err != nil {
		t.Fatalf("calling into the loaded module: %v", err)
	}
	if len(res) != 1 || res[0] != 1 {
		t.Errorf("the module returned %v, want [1]", res)
	}

	if got.MemorySize() == 0 {
		t.Error("the instance reports no guest memory; it is not instantiated")
	}

	body := scrape(t, metrics)
	for _, want := range []string{
		`linkctrl_addon_loads_total{addon="minimal",outcome="loaded"} 1`,
		// permissions is empty because this manifest declares none, and an add-on that
		// asked for nothing holds nothing (M62). The populated form is asserted in
		// permissions_test.go.
		`linkctrl_addon_info{abi_version="1",addon="minimal",failure_class="required",permissions="",version="1.0.0"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape is missing %q:\n%s", want, seriesLike(body, "linkctrl_addon_"))
		}
	}
}

// Discovery order is the directory's sort order, so a boot log and a scrape name
// the add-ons the same way on every restart.
func TestDiscoveryIsOrdered(t *testing.T) {
	code := fixture(t, "minimal")
	root := t.TempDir()
	for _, name := range []string{"zeta", "alpha", "middle"} {
		m := manifestFor(name, ClassDegrade, code)
		install(t, root, m, code)
	}
	h, err := openHost(t, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range h.Addons() {
		got = append(got, a.Manifest.Name)
	}
	want := []string{"alpha", "middle", "zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("discovery order %v, want %v", got, want)
	}
}

// --- load-time verification -------------------------------------------------

// One byte. The check exists for a module that was replaced after it was
// published, and a single flipped byte is the smallest version of that.
func TestOneCorruptedByteRefusesTheModule(t *testing.T) {
	code := fixture(t, "minimal")
	m := manifestFor("minimal", ClassRequired, code)

	corrupt := make([]byte, len(code))
	copy(corrupt, code)
	corrupt[len(corrupt)/2] ^= 0x01

	root := t.TempDir()
	install(t, root, m, corrupt)

	metrics := observability.NewMetrics()
	_, err := openHost(t, root, metrics)
	if err == nil {
		t.Fatal("a module whose hash does not match its manifest loaded")
	}
	if !strings.Contains(err.Error(), "hashes to") || !strings.Contains(err.Error(), m.SHA256) {
		t.Errorf("the error does not report both digests: %v", err)
	}
	if body := scrape(t, metrics); !strings.Contains(body,
		`linkctrl_addon_loads_total{addon="minimal",outcome="checksum_mismatch"} 1`) {
		t.Errorf("the refusal was not counted:\n%s", seriesLike(body, "linkctrl_addon_"))
	}
}

// The directory is the add-on's identity. Two directories claiming one name would
// be two add-ons contending for one Postgres schema at M63.
//
// A mismatch is a manifest problem, but the manifest *parsed* — so the class it
// declares is readable and is honoured. Both halves are asserted, because keying
// the fatal decision on the outcome rather than on the class would have made a
// `degrade` add-on stop the instance over a renamed directory.
func TestManifestNameMustMatchItsDirectory(t *testing.T) {
	code := fixture(t, "minimal")

	for _, tc := range []struct {
		class     FailureClass
		wantFatal bool
	}{
		{ClassRequired, true},
		{ClassDegrade, false},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			root := t.TempDir()
			install(t, root, manifestFor("minimal", tc.class, code), code)
			if err := os.Rename(filepath.Join(root, "minimal"),
				filepath.Join(root, "renamed")); err != nil {
				t.Fatal(err)
			}

			metrics := observability.NewMetrics()
			h, err := openHost(t, root, metrics)
			switch {
			case tc.wantFatal && err == nil:
				t.Fatal("a required add-on whose manifest disagrees with its directory " +
					"did not stop the instance")
			case !tc.wantFatal && err != nil:
				t.Fatalf("a degrade add-on's renamed directory stopped the instance: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "they must match") {
				t.Errorf("unexpected error: %v", err)
			}
			if h != nil && h.Len() != 0 {
				t.Errorf("%d add-ons loaded despite the mismatch", h.Len())
			}
			if body := scrape(t, metrics); !strings.Contains(body,
				`linkctrl_addon_loads_total{addon="renamed",outcome="manifest_invalid"} 1`) {
				t.Errorf("the refusal was not counted against the directory name:\n%s",
					seriesLike(body, "linkctrl_addon_"))
			}
		})
	}
}

// --- names that stand in a prefix relation ----------------------------------

// prefixRelated is the pair the whole rule is about, built the way an operator
// would install it: `oidc` declaring the prefix `oidc_x`, which is legal because
// it begins with its own name and an underscore, and `oidc_x` declaring
// `oidc_x_state`, which is legal for the same reason.
func prefixRelated(code []byte, class FailureClass) (Manifest, Manifest) {
	a := manifestFor("oidc", class, code)
	a.Permissions = []string{"routes.own_prefix"}
	a.CookiePrefixes = []string{"oidc_x"}

	b := manifestFor("oidc_x", class, code)
	b.Permissions = []string{"routes.own_prefix"}
	b.CookiePrefixes = []string{"oidc_x_state"}
	return a, b
}

// The abuse path the load-time refusal closes, measured rather than asserted.
//
// Both manifests are valid on their own — that is what makes this a question
// about the installed *set* — and while both loaded, `oidc` read and overwrote
// `oidc_x`'s session state, because inbound filtering and outbound authorisation
// are both a prefix test. The same relation makes one environment variable mean
// two settings. Neither namespace is fixed here: what is fixed is that two names
// standing in that relation cannot both load, so no pair of loaded add-ons can
// reach the overlap.
func TestPrefixRelatedNamesCannotBothLoad(t *testing.T) {
	code := fixture(t, "minimal")
	a, b := prefixRelated(code, ClassDegrade)

	// The two overlaps, stated as the facts they are. Both survive this fix and
	// both are unreachable because of it.
	if err := a.Validate(); err != nil {
		t.Fatalf("oidc declaring the prefix oidc_x is refused by Validate, so this "+
			"test no longer measures what it is for: %v", err)
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("oidc_x is refused by Validate: %v", err)
	}
	sent := &http.Cookie{Name: "oidc_x_state", Value: "oidc_x's own session state"}
	if crossed := (RequestIn{Cookies: []*http.Cookie{sent}}).record(a.CookiePrefixes); len(crossed.Cookies) != 1 {
		t.Errorf("oidc's prefixes no longer admit %s, so the cookie half of the "+
			"collision is gone and the refusal below has one reason fewer", sent.Name)
	}
	if err := checkCookie(Cookie{Name: sent.Name, Value: "overwritten"}, a.CookiePrefixes); err != nil {
		t.Errorf("oidc may no longer set %s: %v", sent.Name, err)
	}
	if one, two := config.AddonSettingVar("oidc", "x_key"),
		config.AddonSettingVar("oidc_x", "key"); one != two {
		t.Errorf("%s and %s are no longer one variable, so the settings half of the "+
			"collision is gone", one, two)
	}

	root := t.TempDir()
	install(t, root, a, code)
	install(t, root, b, code)

	metrics := observability.NewMetrics()
	h, err := openHost(t, root, metrics)
	if err != nil {
		t.Fatalf("a degrade-class collision stopped the instance: %v", err)
	}
	// Both, not the shorter and not the longer: there is no principled winner, and
	// picking by sort order would be D234's first-come rule with spelling deciding.
	if h.Len() != 0 {
		t.Errorf("%d add-ons loaded from a colliding pair; neither may", h.Len())
		for _, l := range h.Addons() {
			t.Errorf("loaded %q, whose cookie prefixes are %v", l.Manifest.Name,
				l.Manifest.CookiePrefixes)
		}
	}
	body := scrape(t, metrics)
	for _, name := range []string{"oidc", "oidc_x"} {
		want := `linkctrl_addon_loads_total{addon="` + name + `",outcome="` +
			string(OutcomeNameCollision) + `"} 1`
		if !strings.Contains(body, want) {
			t.Errorf("%q's refusal was not counted as a collision:\n%s", name,
				seriesLike(body, "linkctrl_addon_"))
		}
	}
}

// The underscore is the whole of the relation. `oidcx` concatenates with nothing
// `oidc` can produce, so both names are ordinary neighbours.
func TestNamesThatMerelySharePrefixTextBothLoad(t *testing.T) {
	code := fixture(t, "minimal")
	root := t.TempDir()
	install(t, root, manifestFor("oidc", ClassRequired, code), code)
	install(t, root, manifestFor("oidcx", ClassRequired, code), code)

	h, err := openHost(t, root, nil)
	if err != nil {
		t.Fatalf("two names in no prefix relation stopped the instance: %v", err)
	}
	if h.Len() != 2 {
		t.Errorf("%d add-ons loaded, want 2", h.Len())
	}
}

// A collision is honoured against each add-on's own failure class, which is only
// knowable because the claim is read from the manifest rather than from the
// directory entry. A `required` add-on whose namespace is ambiguous stops the
// instance, for the reason its class exists: sign-in shared with a squatter is
// not a degraded instance.
func TestARequiredCollisionStopsTheInstance(t *testing.T) {
	code := fixture(t, "minimal")
	root := t.TempDir()
	a, b := prefixRelated(code, ClassRequired)
	install(t, root, a, code)
	install(t, root, b, code)

	_, err := openHost(t, root, nil)
	if err == nil {
		t.Fatal("a required add-on in a name collision did not stop the instance")
	}
	if !strings.Contains(err.Error(), "cannot load beside") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A directory that has claimed nothing denies nothing. Here the manifest parses
// and names some other directory, so it can never load — and it is not allowed to
// take the add-on beside it down on the way. That is why the claim is read from
// manifests rather than from directory entries: an entry is a name somebody typed,
// and a manifest agreeing with its directory is the only thing that makes it an
// identity.
func TestADirectoryThatClaimsNothingDeniesNothing(t *testing.T) {
	code := fixture(t, "minimal")
	root := t.TempDir()
	a, b := prefixRelated(code, ClassDegrade)
	install(t, root, a, code)

	// Installed into `oidc_x` while naming `oidc_x_typo`: a real mis-install, and
	// the shape loadOne refuses for disagreeing with its directory.
	install(t, root, b, code)
	b.Name = "oidc_x_typo"
	// Its prefixes go with the name: a manifest is validated against its own name
	// before anything compares it to the directory, so declarations that named the
	// old one would fail for the wrong reason.
	b.CookiePrefixes = nil
	manifest, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oidc_x", ManifestFile), manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	h, openErr := openHost(t, root, nil)
	if openErr != nil {
		t.Fatalf("a mis-installed neighbour stopped the instance: %v", openErr)
	}
	if h.Len() != 1 {
		t.Errorf("%d add-ons loaded, want 1: a manifest that disagrees with its "+
			"directory claims no name", h.Len())
	}
	for _, l := range h.Addons() {
		if l.Manifest.Name != "oidc" {
			t.Errorf("loaded %q rather than oidc", l.Manifest.Name)
		}
	}
}

// --- the failure classes ----------------------------------------------------

func TestRequiredFailureStopsTheInstance(t *testing.T) {
	code := fixture(t, "failing")
	root := t.TempDir()
	install(t, root, manifestFor("failing", ClassRequired, code), code)

	metrics := observability.NewMetrics()
	h, err := openHost(t, root, metrics)
	if err == nil {
		t.Fatal("a required add-on that will not instantiate did not stop the instance")
	}
	if h != nil {
		t.Error("Open returned a host alongside the error; nothing should be left running")
	}
	if !strings.Contains(err.Error(), "failing") {
		t.Errorf("the error does not name the add-on: %v", err)
	}
	if body := scrape(t, metrics); !strings.Contains(body,
		`linkctrl_addon_loads_total{addon="failing",outcome="instantiate_failed"} 1`) {
		t.Errorf("the failure was not counted:\n%s", seriesLike(body, "linkctrl_addon_"))
	}
}

func TestDegradeFailureLetsTheInstanceServe(t *testing.T) {
	failing := fixture(t, "failing")
	minimal := fixture(t, "minimal")
	root := t.TempDir()
	install(t, root, manifestFor("failing", ClassDegrade, failing), failing)
	install(t, root, manifestFor("minimal", ClassDegrade, minimal), minimal)

	metrics := observability.NewMetrics()
	h, err := openHost(t, root, metrics)
	if err != nil {
		t.Fatalf("a degrade-class failure stopped the instance: %v", err)
	}
	// The one that works still loaded. A degrade failure must not take the rest of
	// the directory with it.
	if h.Len() != 1 || h.Addons()[0].Manifest.Name != "minimal" {
		t.Fatalf("loaded %d add-ons, want only minimal", h.Len())
	}

	body := scrape(t, metrics)
	for _, want := range []string{
		`linkctrl_addon_loads_total{addon="failing",outcome="instantiate_failed"} 1`,
		`linkctrl_addon_loads_total{addon="minimal",outcome="loaded"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape is missing %q:\n%s", want, seriesLike(body, "linkctrl_addon_"))
		}
	}
	// And no identity series for the one that did not load: linkctrl_addon_info is
	// what this instance is running, not what it was asked to run.
	if strings.Contains(body, `linkctrl_addon_info{abi_version="1",addon="failing"`) {
		t.Error("an add-on that failed to instantiate has an info series")
	}
}

// The harsh limb, and the one worth stating in a test name: a manifest nobody can
// parse has no failure class, so there is nothing to honour. Assuming `degrade`
// would boot an instance with an authentication add-on silently missing.
func TestAnUnparseableManifestStopsTheInstance(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	metrics := observability.NewMetrics()
	if _, err := openHost(t, root, metrics); err == nil {
		t.Fatal("an add-on with an unparseable manifest was skipped rather than refused")
	}
	if body := scrape(t, metrics); !strings.Contains(body,
		`linkctrl_addon_loads_total{addon="broken",outcome="manifest_invalid"} 1`) {
		t.Errorf("the refusal was not counted:\n%s", seriesLike(body, "linkctrl_addon_"))
	}
}

// The refusal path's label is a directory entry, and a directory entry is
// operator input: this is the one place in the package where a filename reaches a
// metric label, so it is the one place where an unbounded label value could.
//
// The consequence that makes it worth a test is not a mislabelled series. A label
// value that is not valid UTF-8 makes client_golang's WithLabelValues **panic** —
// at the observation, not at the scrape — so without the bound an add-ons
// directory holding one directory named in some other encoding takes the process
// down inside Open, at boot, with nothing serving. A newline is the harmless case
// by comparison: the exposition escapes it. Both are here because only one of them
// is obvious.
func TestARefusalNeverPublishesADirectoryNameAsALabel(t *testing.T) {
	code := fixture(t, "minimal")

	for _, entry := range []string{
		"bad\nname\" quote",
		"\xff\xfe", // not UTF-8: the limb that panics rather than mislabels
		"Not-An-Addon-Name",
	} {
		root := t.TempDir()
		// Written by hand rather than through install(), which names the directory
		// after the manifest: the whole subject here is the two disagreeing.
		dir := filepath.Join(root, entry)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			// Not every filesystem accepts every byte string — APFS refuses a name
			// that is not UTF-8. Skipping the case beats asserting nothing.
			t.Logf("skipping %q: this filesystem will not hold it (%v)", entry, err)
			continue
		}
		// Degrade-class, so Open returns and there is a scrape to look at. The
		// manifest is valid and simply does not agree with the directory it is in,
		// which is the refusal that still knows a failure class.
		b, err := json.Marshal(manifestFor("minimal", ClassDegrade, code))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ManifestFile), b, 0o644); err != nil {
			t.Fatal(err)
		}

		metrics := observability.NewMetrics()
		if _, err := openHost(t, root, metrics); err != nil {
			t.Fatalf("%q: a degrade-class refusal stopped the instance: %v", entry, err)
		}

		body := scrape(t, metrics)
		want := `linkctrl_addon_loads_total{addon="` + InvalidName + `",outcome="manifest_invalid"} 1`
		if !strings.Contains(body, want) {
			t.Errorf("%q: the scrape is missing %q:\n%s", entry, want,
				seriesLike(body, "linkctrl_addon_"))
		}
		if strings.Contains(body, entry) {
			t.Errorf("%q: the raw directory name reached the exposition:\n%s", entry,
				seriesLike(body, "linkctrl_addon_"))
		}
	}
}

// labelFor's whole job, over the shapes an operator's filesystem allows and
// nameRe does not.
func TestLabelForBoundsWhatNameReRefuses(t *testing.T) {
	for _, tc := range []struct{ entry, want string }{
		{"minimal", "minimal"},
		{"oidc_provider", "oidc_provider"},
		{"", InvalidName},
		{"a", InvalidName},            // one character: nameRe wants at least two
		{"Minimal", InvalidName},      // uppercase
		{"has-a-hyphen", InvalidName}, // hyphen is a directory name and not a name
		{"1leading_digit", InvalidName},
		{"bad\nname", InvalidName}, // the one that would break the exposition
		{"quote\"name", InvalidName},
		{"\xff\xfe", InvalidName},              // not UTF-8 at all
		{strings.Repeat("a", 32), InvalidName}, // one past nameRe's ceiling
	} {
		if got := labelFor(tc.entry); got != tc.want {
			t.Errorf("labelFor(%q) = %q, want %q", tc.entry, got, tc.want)
		}
	}
}

// A stray file is not an add-on and is not a reason to stop. It is said out loud
// instead, because "nothing loaded" with no explanation is the worse outcome.
func TestAStrayFileIsIgnored(t *testing.T) {
	code := fixture(t, "minimal")
	root := t.TempDir()
	install(t, root, manifestFor("minimal", ClassRequired, code), code)
	if err := os.WriteFile(filepath.Join(root, "README.txt"), []byte("put add-ons here"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := openHost(t, root, nil)
	if err != nil {
		t.Fatalf("a stray file stopped the instance: %v", err)
	}
	if h.Len() != 1 {
		t.Errorf("%d add-ons loaded, want 1", h.Len())
	}
}

// A directory that was configured and is not there is a configuration error, not
// an absence: the operator believes this instance is running modules it has never
// seen. config.Validate says the same thing before boot reaches here.
func TestAMissingDirectoryIsAnError(t *testing.T) {
	if _, err := openHost(t, filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Fatal("a configured directory that does not exist was treated as no add-ons")
	}
}

// --- what M66 will price against -------------------------------------------

// The numbers, measured rather than assumed, because m60.md's first risk is that
// wazero's instantiation cost and memory model are unmeasured and M66 inherits
// whatever they are.
//
// **Compilation and instantiation are timed separately**, and the split is the
// whole of what makes the numbers usable: compiling happens once per module at
// boot and instantiation is the only one of the two a per-request budget would
// ever pay, so a single duration covering both prices neither. Open's own
// duration is timed too, and it is a third number — what a boot costs — rather
// than a substitute for either.
//
// In-package, so the two steps loadOne runs can be run one at a time against the
// runtime configuration Open builds. A cost measured against a different
// configuration prices nothing the host will pay.
//
// The assertions are ceilings loose enough not to flake on a shared runner; the
// t.Log lines are the actual point.
func TestInstantiationCostIsMeasured(t *testing.T) {
	ctx := context.Background()
	code := fixture(t, "minimal")

	rt := wazero.NewRuntimeWithConfig(ctx,
		wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
	t.Cleanup(func() { _ = rt.Close(ctx) })
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		t.Fatal(err)
	}
	// The ABI, because Open registers it and a cost measured without it is a cost
	// no instance pays — the fixture imports the host module, so a runtime without
	// it cannot instantiate the fixture at all. State for each instance name too:
	// the fixture logs from package initialization, and a host function answers the
	// calling module.
	direct := &Host{runtime: rt, log: slog.New(slog.DiscardHandler)}
	if err := direct.registerABI(ctx); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	compiled, err := rt.CompileModule(ctx, code)
	compile := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}

	// Twice, from the one compiled module, because that is the operation M66
	// repeats: the second instance is what a per-request instantiation would be,
	// with the compile already paid for.
	var (
		instantiate [2]time.Duration
		mem         uint32
		heap        uint64
	)
	for i := range instantiate {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		name := fmt.Sprintf("minimal_%d", i)
		m := manifestFor(name, ClassRequired, code)
		grants, _ := resolveGrants(m)
		direct.registerState(m, grants, nil, nil)

		start = time.Now()
		mod, err := rt.InstantiateModule(ctx, compiled,
			wazero.NewModuleConfig().
				WithName(name).
				WithStartFunctions(StartFunction))
		instantiate[i] = time.Since(start)
		if err != nil {
			t.Fatalf("instance %d did not instantiate: %v", i, err)
		}

		runtime.ReadMemStats(&after)
		if i == 0 {
			mem = mod.Memory().Size()
			// Indicative and deliberately unasserted: the guest's linear memory is
			// a Go allocation, so this is what holding one instance costs the host
			// process, measured across an allocator that is free to move under it.
			heap = after.HeapAlloc - before.HeapAlloc
		}
	}

	// The same lifecycle through the exported path, for the third number and to
	// keep MemorySize — which is what m60.md's resident-cost claim is about — the
	// thing the memory assertion below reads.
	root := t.TempDir()
	install(t, root, manifestFor("minimal", ClassRequired, code), code)
	start = time.Now()
	h, err := openHost(t, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	whole := time.Since(start)
	if got := h.Addons()[0].MemorySize(); got != mem {
		t.Errorf("Open's instance holds %d bytes of guest memory, the direct one %d; "+
			"the two paths are meant to be the same instantiation", got, mem)
	}

	t.Logf("module %d bytes; compile %v; instantiate %v then %v; "+
		"guest memory %d bytes; host heap +%d bytes; whole of Open %v",
		len(code), compile, instantiate[0], instantiate[1], mem, heap, whole)

	slowest := instantiate[0]
	if instantiate[1] > slowest {
		slowest = instantiate[1]
	}

	// Measured on this machine at the numbers D225 records. The ceilings are two
	// or three orders of magnitude above them: what they catch is a regression
	// that changes the shape of the cost — instantiation doing compilation's work,
	// or a guest that allocates by the tens of megabytes — and never jitter.
	if slowest > time.Second {
		t.Errorf("instantiating one module took %v; M66's per-request budget "+
			"cannot be priced against this", slowest)
	}
	if compile > 30*time.Second {
		t.Errorf("compiling one module took %v; that is not a boot-time cost any more", compile)
	}
	if whole > 30*time.Second {
		t.Errorf("loading one add-on took %v; that is not a boot-time cost any more", whole)
	}
	if mem > 64<<20 {
		t.Errorf("one instance holds %d bytes of guest memory; M66's per-request "+
			"budget cannot be priced against this", mem)
	}
}
