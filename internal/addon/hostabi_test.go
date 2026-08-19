package addon

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero/api"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// logSink captures what the host logged, which is how an add-on's own words reach
// a test: the ABI's log function is the only way out of a module, since stdout
// and stderr are discarded.
type logSink struct {
	mu    sync.Mutex
	lines strings.Builder
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines.String()
}

func openHostWithLog(t *testing.T, dir string) (*Host, *logSink, error) {
	t.Helper()
	sink := &logSink{}
	h, err := Open(context.Background(), Options{
		Dir:    dir,
		Logger: slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if h != nil {
		t.Cleanup(func() { _ = h.Close(context.Background()) })
	}
	return h, sink, err
}

// --- the surface the runtime actually publishes -----------------------------

// The declared set and the registered set are the same set, checked against the
// runtime rather than against the map in hostabi.go. A function abi.Functions
// declares and the host does not register is a link failure for every module that
// imports it; one the host registers and the ABI does not declare is a capability
// nothing documents.
func TestTheHostModuleExportsExactlyTheABI(t *testing.T) {
	h, _, err := openHostWithLog(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mod := h.runtime.Module(abi.HostModule)
	if mod == nil {
		t.Fatalf("the runtime has no %q module", abi.HostModule)
	}

	got := mod.ExportedFunctionDefinitions()
	if len(got) != len(abi.Functions) {
		t.Errorf("the host module exports %d functions and the ABI declares %d", len(got), len(abi.Functions))
	}
	for _, f := range abi.Functions {
		def, ok := got[f.Name]
		if !ok {
			t.Errorf("%q is declared by the ABI and not exported by the host module", f.Name)
			continue
		}
		params, results := hostSignature(f)
		if len(def.ParamTypes()) != len(params) {
			t.Errorf("%q takes %d wasm parameters and its signature says %d",
				f.Name, len(def.ParamTypes()), len(params))
		}
		if len(def.ResultTypes()) != len(results) || def.ResultTypes()[0] != api.ValueTypeI32 {
			t.Errorf("%q returns %v, and every ABI function returns one i32", f.Name, def.ResultTypes())
		}
	}
	for name := range got {
		if !containsFunc(name) {
			t.Errorf("the host module exports %q, which the ABI does not declare", name)
		}
	}
}

func containsFunc(name string) bool {
	for _, f := range abi.Functions {
		if f.Name == name {
			return true
		}
	}
	return false
}

// registerABI refuses to build a host whose implementation map disagrees with the
// ABI, in both directions. It is a panic rather than an error because neither
// half depends on anything an operator did: a milestone that implements a limb
// and forgets to mark it Live, or marks one Live and implements nothing, is a
// build that should not have linked — and the first symptom otherwise is a
// function the documentation calls refused answering for real, or the reverse.
func TestRegisterABIRefusesAMapThatDisagreesWithTheABI(t *testing.T) {
	t.Run("live with no implementation", func(t *testing.T) {
		var live string
		for _, f := range abi.Functions {
			if f.Live {
				live = f.Name
				break
			}
		}
		if live == "" {
			t.Fatal("no ABI function is live, so this test has nothing to remove")
		}
		impl := hostFuncs[live]
		delete(hostFuncs, live)
		defer func() { hostFuncs[live] = impl }()
		assertPanics(t, live, func() { _, _, _ = openHostWithLog(t, t.TempDir()) })
	})

	t.Run("implemented and not live", func(t *testing.T) {
		var refused string
		for _, f := range abi.Functions {
			if !f.Live {
				refused = f.Name
				break
			}
		}
		if refused == "" {
			t.Skip("every ABI function is live, so there is no refusal left to mis-implement")
		}
		hostFuncs[refused] = func(context.Context, *hostState, api.Module, []uint64) int32 { return 0 }
		defer delete(hostFuncs, refused)
		assertPanics(t, refused, func() { _, _, _ = openHostWithLog(t, t.TempDir()) })
	})
}

func assertPanics(t *testing.T, naming string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("the host was constructed and should have panicked")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, naming) {
			t.Fatalf("the panic %v does not name %q", r, naming)
		}
	}()
	f()
}

// --- what a real module gets ------------------------------------------------

// The probe fixture is a consumer of the generated SDK, compiled the way a
// published add-on is compiled, and it calls the host across every class of
// answer the ABI can give. Loading it at all is the assertion: a mismatch panics
// inside package initialization, which fails instantiation.
//
// This is the test m61.md's "the refusal is asserted by test, not assumed" names,
// and it is also the first proof that the SDK compiles a working consumer.
func TestTheProbeFixtureGetsEveryClassOfAnswer(t *testing.T) {
	code := fixture(t, "probe")
	dir := t.TempDir()
	m := manifestFor("probe", ClassRequired, code)
	m.Settings = []Setting{
		{Name: "retention_days", Type: SettingText, Default: "30"},
		{Name: "api_token", Type: SettingSecret},
	}
	install(t, dir, m, code)

	h, sink, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatalf("the probe fixture did not load, so one of its checks failed: %v", err)
	}
	if h.Len() != 1 {
		t.Fatalf("loaded %d add-ons, want 1", h.Len())
	}

	logs := sink.String()
	for _, check := range []string{
		"probe: abi_version=ok",
		"probe: config_declared=ok",
		"probe: config_empty=ok",
		"probe: config_undeclared=ok",
		"probe: declared_but_refused=ok",
		"probe: storage_exec_refused=ok",
		"probe: http_request_refused=ok",
		"probe: http_response_refused=ok",
		"probe: template_refused=ok",
		"probe: session_refused=ok",
		"probe: redirect_refused=ok",
		"probe: bad_level=ok",
	} {
		if !strings.Contains(logs, check) {
			t.Errorf("the probe did not report %q\n%s", check, logs)
		}
	}
	if strings.Contains(logs, "MISMATCH") {
		t.Errorf("the probe reported a mismatch\n%s", logs)
	}
	// Every refused function in the ABI is probed, so a limb added to the ABI
	// without a line in the fixture does not quietly go unexercised.
	refused := 0
	for _, f := range abi.Functions {
		if !f.Live {
			refused++
		}
	}
	// declared_but_refused ends in _refused=ok too, which is what makes this count
	// the whole set rather than the set minus the one named differently.
	if reported := strings.Count(logs, "_refused=ok"); reported != refused {
		t.Errorf("the ABI declares %d refused functions and the probe reported %d", refused, reported)
	}
}

// A host function answers the add-on that called it, not whichever one loaded
// first. The same module is installed twice with two different declared defaults;
// each instance reads its own.
//
// This is the property one host module per runtime makes non-obvious: wazero
// resolves imports by module name, so both add-ons import the same "linkctrl"
// module and the scoping has to come from the calling module's identity.
func TestAHostFunctionAnswersTheCallingAddon(t *testing.T) {
	code := fixture(t, "probe")
	dir := t.TempDir()
	for name, retention := range map[string]string{"probe": "30", "probe_two": "90"} {
		m := manifestFor(name, ClassRequired, code)
		m.Settings = []Setting{
			{Name: "retention_days", Type: SettingText, Default: retention},
			{Name: "api_token", Type: SettingSecret},
		}
		install(t, dir, m, code)
	}

	h, sink, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if h.Len() != 2 {
		t.Fatalf("loaded %d add-ons, want 2", h.Len())
	}
	logs := sink.String()
	for _, want := range []string{
		`msg="probe: retention_days=30" addon=probe source=addon`,
		`msg="probe: retention_days=90" addon=probe_two source=addon`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the logs do not carry %s\n%s", want, logs)
		}
	}
}

// A line an add-on logs is attributed to the add-on and marked as its own, so an
// operator reading a log can tell this product's words from a module's.
func TestAnAddonsLogLineIsAttributedToIt(t *testing.T) {
	code := fixture(t, "minimal")
	dir := t.TempDir()
	install(t, dir, manifestFor("minimal", ClassRequired, code), code)

	_, sink, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := `level=INFO msg="minimal fixture initialized against ABI ` + abi.Version +
		`" addon=minimal source=addon`

	if !strings.Contains(sink.String(), want) {
		t.Errorf("the log does not carry %q\n%s", want, sink.String())
	}
}

// --- the load-time version check --------------------------------------------

// A module built against a generation this host does not implement is refused
// before its bytes are read, which is why this add-on has no module file at all:
// if the check ran later the outcome would be module_unreadable.
func TestANewerABIIsRefusedBeforeTheModuleIsRead(t *testing.T) {
	dir := t.TempDir()
	m := manifestFor("futuristic", ClassDegrade, nil)
	m.ABIVersion = abi.Generation + 1
	install(t, dir, m, nil)

	metrics := observability.NewMetrics()
	h, err := openHost(t, dir, metrics)
	if err != nil {
		t.Fatalf("a degrade add-on with an unsupported ABI stopped the instance: %v", err)
	}
	if h.Len() != 0 {
		t.Fatalf("loaded %d add-ons, want 0", h.Len())
	}
	series := `linkctrl_addon_loads_total{addon="futuristic",outcome="abi_unsupported"} 1`
	if got := scrape(t, metrics); !strings.Contains(got, series) {
		t.Errorf("the scrape does not carry %s", series)
	}
}

// The same refusal on a `required` add-on stops the instance, and the error names
// both versions — an operator has to know whether to upgrade LinkCtrl or rebuild
// the add-on.
func TestARequiredAddonWithAnUnsupportedABIStopsTheInstance(t *testing.T) {
	dir := t.TempDir()
	m := manifestFor("futuristic", ClassRequired, nil)
	m.ABIVersion = abi.Generation + 1
	install(t, dir, m, nil)

	_, err := openHost(t, dir, observability.NewMetrics())
	if err == nil {
		t.Fatal("the instance started without a required add-on")
	}
	for _, want := range []string{"futuristic", string(OutcomeABIUnsupported), abi.Version} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %q", err, want)
		}
	}
}

// --- the memory boundary ----------------------------------------------------

// readBytes refuses what it cannot safely copy rather than trusting the guest's
// arithmetic, and it bounds the size a single call may move into host memory. A
// module can address as much as the runtime lets it, so a length is an argument
// from code the operator did not write.
func TestReadingGuestMemoryIsBounded(t *testing.T) {
	code := fixture(t, "minimal")
	dir := t.TempDir()
	install(t, dir, manifestFor("minimal", ClassRequired, code), code)
	h, _, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	mod := h.Addons()[0].Module()

	if _, ok := readBytes(mod, 0, 0); !ok {
		t.Error("a zero length was refused, and an empty value is legal")
	}
	if _, ok := readBytes(mod, 0, maxStringIn+1); ok {
		t.Errorf("a length of %d was accepted, and the bound is %d", maxStringIn+1, maxStringIn)
	}
	beyond := uint64(mod.Memory().Size()) + 1
	if _, ok := readBytes(mod, beyond, 8); ok {
		t.Errorf("a read at offset %d was accepted, and the guest's memory is %d bytes",
			beyond, mod.Memory().Size())
	}
	if s, ok := readString(mod, 0, 0); !ok || s != "" {
		t.Errorf("an empty string read as (%q, %v)", s, ok)
	}
}

// writeOut answers the size and writes nothing when the guest's buffer is too
// small, which is the whole of the grow-and-retry convention: a truncated JSON
// record is a parse error a publisher would debug as a host bug.
func TestWriteOutNeverTruncates(t *testing.T) {
	code := fixture(t, "minimal")
	dir := t.TempDir()
	install(t, dir, manifestFor("minimal", ClassRequired, code), code)
	h, _, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	mod := h.Addons()[0].Module()

	value := []byte("0123456789")
	// A buffer one byte short. The offset is inside the guest's memory and is a
	// place nothing else is using; what matters is that it stays untouched.
	const offset = 1024
	before, ok := mod.Memory().Read(offset, uint32(len(value)))
	if !ok {
		t.Fatal("the fixture's memory is smaller than this test assumes")
	}
	snapshot := append([]byte(nil), before...)

	if got := writeOut(mod, offset, uint64(len(value)-1), value); got != int32(len(value)) {
		t.Errorf("writeOut answered %d for a %d-byte value, and the convention is the size", got, len(value))
	}
	after, _ := mod.Memory().Read(offset, uint32(len(value)))
	if string(after) != string(snapshot) {
		t.Errorf("writeOut wrote %q into a buffer too small for it", after)
	}

	if got := writeOut(mod, offset, uint64(len(value)), value); got != int32(len(value)) {
		t.Errorf("writeOut answered %d for a value that fits", got)
	}
	written, _ := mod.Memory().Read(offset, uint32(len(value)))
	if string(written) != string(value) {
		t.Errorf("writeOut left %q where %q belonged", written, value)
	}
}

// Every level the ABI publishes has a mapping, so a level added to the vocabulary
// without one cannot land silently at debug.
func TestEveryABILevelMapsToASlogLevel(t *testing.T) {
	for _, level := range abi.LogLevels {
		if _, ok := slogLevels[level]; !ok {
			t.Errorf("the ABI accepts level %q and the host has no slog level for it", level)
		}
	}
	if len(slogLevels) != len(abi.LogLevels) {
		t.Errorf("the host maps %d levels and the ABI publishes %d", len(slogLevels), len(abi.LogLevels))
	}
}
