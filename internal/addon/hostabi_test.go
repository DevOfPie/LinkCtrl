package addon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

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
	// Every grantable permission, because probe calls every function and a call
	// whose grant is not declared is Denied before the host gets as far as saying
	// whether it implements the function (M62). That ordering is what the
	// `undeclared` fixture asserts from the other side.
	m.Permissions = grantable()
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
		"probe: storage_query_no_database=ok",
		"probe: storage_exec_no_database=ok",
		"probe: http_request_outside_request=ok",
		"probe: http_response_outside_request=ok",
		"probe: session_context_outside_request=ok",
		"probe: random_bytes=ok",
		"probe: crypto_rand=ok",
		"probe: random_differs_from_stdlib=ok",
		"probe: time_now=ok",
		"probe: time_now_parses=ok",
		"probe: time_now_is_not_the_fake_clock=ok",
		"probe: time_now_is_utc=ok",
		"probe: std_clock_agrees=ok",
		"probe: random_zero_invalid=ok",
		"probe: random_over_bound_invalid=ok",
		"probe: session_mint_outside_request=ok",
		"probe: identity_link_outside_request=ok",
		"probe: redirect_event_outside_invocation=ok",
		"probe: redirect_decision_outside_invocation=ok",
		"probe: redirect_answer_outside_invocation=ok",
		"probe: template_refused=ok",
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
	// Every refused check is named `<something>_refused=ok`, so counting the suffix
	// counts the set. The checks for functions that are implemented are deliberately
	// named otherwise — the two storage ones because this host has no database
	// rather than no implementation, and M64's three because "outside a request" is
	// a state and not a refusal — which is what keeps this count honest as limbs
	// land. It has moved twice now, from five to three, without this line changing.
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
	// `probetwo` rather than `probe_two`: two names standing in a `name + "_"`
	// prefix relation are refused at load (nameCollisions), which is what this test
	// installed before D267 and is nothing to do with what it measures.
	for name, retention := range map[string]string{"probe": "30", "probetwo": "90"} {
		m := manifestFor(name, ClassRequired, code)
		m.Permissions = grantable()
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
		`msg="probe: retention_days=90" addon=probetwo source=addon`,
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

// --- what an add-on writes to the log ---------------------------------------

// m62.md's sanitization bullet, asserted end to end: the bytes leave a module,
// cross the ungated log function, and what reaches the logger is neutralized.
//
// The `undeclared` fixture is the module on purpose. log costs no permission, so
// the reach of this function is *every* loaded module including one that declared
// nothing, which is what makes it the widest untrusted input this host has and a
// module holding no grant the honest place to test it from.
func TestWhatAnAddonWritesToTheLogIsNeutralizedAtTheBoundary(t *testing.T) {
	code := fixture(t, "undeclared")
	dir := t.TempDir()
	m := manifestFor("undeclared", ClassRequired, code)
	m.Settings = []Setting{{Name: "retention_days", Type: SettingText, Default: "30"}}
	if len(m.Permissions) != 0 {
		t.Fatal("this manifest declares a permission, and the claim is about a module that declared none")
	}
	install(t, dir, m, code)

	_, sink, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatalf("the undeclared fixture did not load, so one of its checks failed: %v", err)
	}
	logs := sink.String()

	// slog's handler quotes what it is given, so a raw control character would reach
	// this sink behind one backslash and a host-escaped one reaches it behind two:
	// the doubled backslash is the host's own escape surviving the handler's, and it
	// is the only thing that tells the two apart. Which is the point — sanitizing
	// inside the logger, or trusting the logger to do it, is what this rules out.
	// The last is a backslash the module itself wrote: doubled by the host and quoted
	// again by the handler, it arrives behind four, where a real newline arrives behind
	// two. That difference is the escaping being injective (D244) — before it, the two
	// were the same line.
	for _, want := range []string{`\\n`, `\\u001b`, `\\u202e`, `\\u200b`, `\\u061c`, `\\\\nliteral`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log does not carry %s, so that code point reached the logger as itself\n%s",
				want, logs)
		}
	}
	// The channel rather than a spelling, and it is the one F285 filed: **no variation
	// selector reaches this log at all**, by any route. Walked over Unicode's property
	// rather than over the code points the fixture wrote, so a scheme the fixture does
	// not carry fails here too — and asserted alongside the escape spellings, because
	// *the payload is gone* and *the selectors were deleted rather than escaped* are two
	// claims and neither implies the other.
	for r := rune(0); r <= 0x10FFFF; r++ {
		if strippedRune(r) && strings.ContainsRune(logs, r) {
			t.Errorf("U+%04X reached the log as itself, so a module carried a covert bit past the boundary\n%s", r, logs)
		}
	}
	for _, escaped := range []string{`\\ufe0f`, `\\ufe0e`, `\\ufe00`, `\\U000e0100`} {
		if strings.Contains(logs, escaped) {
			t.Errorf("the log carries %s, so a selector was escaped where D283 says it is deleted\n%s", escaped, logs)
		}
	}
	// The other half of stripping: the line survives its carrier being removed. The
	// bar's visible text is there, and the heart is a heart rather than a heart with an
	// escape through it — which is the whole reason this boundary deletes instead of
	// escaping.
	if !strings.Contains(logs, "migrating: [") || !strings.Contains(logs, "everything is fine") {
		t.Errorf("the progress bar's visible text did not reach the log, so the assertion above is about nothing\n%s", logs)
	}
	if !strings.Contains(logs, "\u2764") || !strings.Contains(logs, "\u6f22") {
		t.Errorf("a base lost more than its selector\n%s", logs)
	}
	// The forged record itself, stated as the harm rather than as an escape: no
	// second record begins inside a module's message.
	if strings.Contains(logs, "\nlevel=ERROR") {
		t.Errorf("a module's message opened a record of its own\n%s", logs)
	}
	// Sanitizing is not refusing. A module whose message needed neutralizing still
	// gets to speak, which is the whole reason log is ungated.
	if !strings.Contains(logs, "undeclared: hostile_message_accepted=ok") {
		t.Errorf("the hostile message was refused rather than neutralized\n%s", logs)
	}
}

// Every class of byte, at the boundary itself, because the end-to-end test above
// carries one message and one message cannot carry a class.
//
// These are examples, and examples are no longer how the set is pinned — D242 made
// the set a default-deny, so the claim about it is asserted from Unicode's own
// categories in TestTheEscapedSetIsDefaultDeny below. What this table is for is the
// spelling: that a NUL reads `\u0000` and a tag character reads `\U000e0041`, which
// a category test cannot say.
func TestSanitizeLogMessageNeutralizesEveryClassOfByte(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"ordinary text is untouched", "loaded 3 add-ons in 12ms", "loaded 3 add-ons in 12ms"},
		{"non-ASCII is text and not a control", "café 日本語 ok", "café 日本語 ok"},
		{"a newline cannot close the record", "a\nb", `a\nb`},
		{"a carriage return and a tab", "a\r\tb", `a\r\tb`},
		{"a NUL", "a\x00b", `a\u0000b`},
		{"an ANSI escape", "a\x1b[2Kb", `a\u001b[2Kb`},
		{"DEL", "a\x7fb", `a\u007fb`},
		{"a C1 control", "a\u0085b", `a\u0085b`},
		{"a bidi override", "a\u202eb", `a\u202eb`},
		{"a bidi isolate", "a\u2066b", `a\u2066b`},
		{"a zero-width space", "a\u200bb", `a\u200bb`},
		{"a byte order mark", "\ufeffa", `\ufeffa`},
		{"a soft hyphen", "a\u00adb", `a\u00adb`},
		{"a tag character, above the BMP", "a\U000e0041b", `a\U000e0041b`},
		{"the Arabic letter mark, which the enumeration missed", "a\u061cb", `a\u061cb`},
		{"the Mongolian vowel separator", "a\u180eb", `a\u180eb`},
		{"interlinear annotation, which hides the run it wraps", "a\ufff9b\ufffbc", `a\ufff9b\ufffbc`},
		{"a musical format character", "a\U0001d173b", `a\U0001d173b`},
		{"an Egyptian hieroglyph format control", "a\U00013430b", `a\U00013430b`},
		{"the Hangul filler, a letter that renders as nothing", "a\u3164b", `a\u3164b`},
		{"the Braille blank, a symbol that renders as nothing", "a\u2800b", `a\u2800b`},
		{"an unassigned code point", "a\ufff0b", `a\ufff0b`},
		{"a private use code point", "a\ue000b", `a\ue000b`},
		{"a line separator", "a\u2028b", `a\u2028b`},
		{"the Arabic number sign is meaning, not concealment", "\u0600\u0661\u0662", "\u0600\u0661\u0662"},
		{"so is the end of ayah", "\u06dd\u0661", "\u06dd\u0661"},
		{"and the Kaithi number sign, above the BMP", "\U000110bd\u0661", "\U000110bd\u0661"},
		{"a backslash is doubled, so no escape can be spelled by a module", `a\nb`, `a\\nb`},
		{"a module's copy of the truncation mark is not the host's", `done …\(truncated)`, `done …\\(truncated)`},
		{"the two Arabic marks the hand-copied allowlist missed", "\u0890\u0891\u0661", "\u0890\u0891\u0661"},
		{"a non-breaking space is a space", "a\u00a0b", "a\u00a0b"},
		// D283: every variation selector is deleted, and it is the one class this
		// boundary removes rather than escapes. The heart survives as a heart, which is
		// what makes the removal cost a reader nothing, and no spelling of a selector
		// appears anywhere — asserted as an equality against the whole output, so a
		// selector turning into `\\ufe0f` fails here as loudly as one surviving.
		{"an emoji presentation selector is deleted and the emoji stays", "\u2764\ufe0f", "\u2764"},
		{"a text presentation selector too", "\u2764\ufe0e", "\u2764"},
		{"an emoji with no selector to lose is untouched", "\U0001F600", "\U0001F600"},
		{"a keycap loses its selector and keeps its enclosure", "1\ufe0f\u20e3", "1\u20e3"},
		// F285's three reproduction schemes, at the boundary function. Each is a message
		// a reader sees as ordinary text with a payload hanging off it.
		{"the nibble scheme: selectors after letters are payload", "ok\ufe01\ufe07\ufe00", "ok"},
		{"a selector after a space", "a \ufe0fb", "a b"},
		{"the byte-per-selector scheme, above the BMP", "hi\U000e0100\U000e0101", "hi"},
		// The scheme that defeated the first attempt's carve-out: Block Elements are
		// category So and Unicode registers a sequence for none of them.
		{"a block element carries no selector out", "\u2588\ufe0f\u2591\ufe0e", "\u2588\u2591"},
		{"nor a box-drawing character", "\u2501\ufe0f\u2502\ufe0f", "\u2501\u2502"},
		// And the ones that defeated the second attempt's: a registered base is not a
		// safe base, because a renderer that ignores one of the pair makes both states of
		// the bit the same pixels. There is no base set, so there is no exemption.
		{"a registered base does not launder a selector either", "\u2764\ufe0e\u26a0\ufe0f", "\u2764\u26a0"},
		{"nor does stacking them on one", "\u2764\ufe0f\ufe0f\ufe0f", "\u2764"},
		{"an ideographic variation sequence is not an emoji one", "\u6f22\U000e0100", "\u6f22"},
		{"a Mongolian free variation selector", "\u1820\u180b", "\u1820"},
		{"a selector with nothing before it", "\ufe0fa", "a"},
		{"a selector after the replacement character invalid UTF-8 became", "a\xff\ufe0f", "a\ufffd"},
		{"invalid UTF-8 becomes the replacement character", "a\xffb", "a\ufffdb"},
		// The three sentences docs/addon-abi.md makes about emoji sequences, pinned
		// because one of them was wrong when M61 published it: it said a flag sequence
		// survives, being made of ordinary graphic code points, and that is true of a
		// national flag and false of a subdivision one, which is built out of tag
		// characters and is therefore category Cf from the second code point on.
		{"a national flag is two regional indicators and survives", "\U0001F1EC\U0001F1E7", "\U0001F1EC\U0001F1E7"},
		{"a skin tone modifier is Sk and survives", "\U0001F44D\U0001F3FD", "\U0001F44D\U0001F3FD"},
		{"a subdivision flag is tag characters and does not", "\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F",
			"\U0001F3F4" + `\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeLogMessage(tc.in); got != tc.want {
				t.Errorf("sanitizeLogMessage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The claim three documents make about this function, asserted from Unicode's own
// categories rather than from a list beside the implementation's list (D242).
//
// The previous form of this test enumerated code points and passed while U+061C
// reached the logger as itself, because a test that lists what the code lists agrees
// with the code by construction. So the assertions here are the documents' words:
// what is not graphic is escaped, what is graphic is not — and the exceptions to
// each are counted, so widening either one is a deliberate change to this test.
func TestTheEscapedSetIsDefaultDeny(t *testing.T) {
	// The allowlist is Unicode's Prepended_Concatenation_Mark property (D243), and this
	// test names the property rather than its members for the reason the code does: the
	// previous form of both was a hand-copy, eleven of thirteen, and the two it dropped
	// were escaped while a test listing the same eleven agreed. What is pinned instead is
	// below — that every member is a real carve-out, and that the property has not
	// shrunk from the thirteen members it carried when this was written.
	allowed := func(r rune) bool { return unicode.Is(unicode.Prepended_Concatenation_Mark, r) }
	// Graphic and still escaped: the seven graphic members of Unicode's
	// Other_Default_Ignorable_Code_Point, plus the Braille blank cell, which is neither
	// non-graphic nor default-ignorable and renders as nothing anyway. **Seven is this
	// map's count and not the count of graphic default-ignorables**, which is 267: the
	// other 260 are the variation selectors, they were the whole of F285, and they are
	// deleted rather than escaped, so they get a case of their own below.
	blank := map[rune]bool{
		'\u034f': true, '\u115f': true, '\u1160': true, '\u17b4': true, '\u17b5': true,
		'\u3164': true, '\uffa0': true, '\u2800': true,
	}
	// Graphic, escaped, and for neither of those reasons: backslash introduces every
	// escape this function writes, so leaving it would make `\` `n` and a newline the
	// same two bytes in the line (D244).
	introducer := map[rune]bool{'\\': true}

	var allowedSeen, blankSeen, introducerSeen, selectorSeen int
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // a surrogate is not a rune a string can carry
		}
		esc := escapeLogRune(r) != ""
		switch {
		case strippedRune(r):
			// Decided before every other limb, because a selector never reaches this
			// function on the sanitizer's path (D283). Both halves are asserted: it is
			// stripped, and escapeLogRune answers *escape* if it is ever asked anyway, so
			// removing the strip fails loudly rather than reopening F285.
			selectorSeen++
			if !esc {
				t.Errorf("U+%04X would reach the line as itself if the strip were removed", r)
			}
		case allowed(r):
			allowedSeen++
			if esc {
				t.Errorf("U+%04X carries meaning in a message and was escaped", r)
			}
			// Each member is a carve-out from default-deny and not a coincidence of it: a
			// graphic one would be unescaped whether the allowlist named it or not, and the
			// carve-out is only ever about category Cf.
			if unicode.IsGraphic(r) {
				t.Errorf("U+%04X is graphic, so the allowlist is not what keeps it unescaped", r)
			}
		case blank[r]:
			blankSeen++
			if !esc {
				t.Errorf("U+%04X renders as nothing and was not escaped", r)
			}
		case introducer[r]:
			introducerSeen++
			if !esc {
				t.Errorf("U+%04X introduces every escape and was not escaped, so the escaping is not injective", r)
			}
		case !unicode.IsGraphic(r):
			if !esc {
				t.Errorf("U+%04X is not a graphic character and was not escaped", r)
			}
		default:
			if esc {
				t.Errorf("U+%04X is a graphic character and was escaped", r)
			}
		}
	}
	// Thirteen is what the property carries on Unicode 15.0, the tables this tree's Go
	// was built with, and thirteen is the number docs/SECURITY.md and docs/addon-abi.md
	// print. An equality, not a floor: a floor agrees with a document that says twelve
	// or fourteen, and F285's fourth round was lost to exactly that — a wrong constant
	// sitting unnoticed inside an enforcing test. A toolchain that widens the property
	// fails here, and the failure is the instruction to move both documents.
	if allowedSeen != 13 {
		t.Errorf("Prepended_Concatenation_Mark walked %d members and the documents say thirteen (Unicode %s)",
			allowedSeen, unicode.Version)
	}
	if blankSeen != len(blank) || introducerSeen != len(introducer) {
		t.Fatalf("walked %d of %d blank and %d of %d introducer code points",
			blankSeen, len(blank), introducerSeen, len(introducer))
	}
	// 260 on Unicode 15.0: the three Mongolian free variation selectors, U+180F, the
	// sixteen at FE00 and the 240 ideographic ones. Every one of them is graphic, so
	// before this class existed every one of them reached the log as itself. An
	// equality, like the allowlist's above and for the same reason: the documents print
	// 260 and none of them may drift from the tables without this failing.
	if selectorSeen != 260 {
		t.Errorf("Variation_Selector walked %d members and the documents say 260 (Unicode %s)",
			selectorSeen, unicode.Version)
	}

	// Category Cf was D241's rejected candidate for the test, and default-deny subsumes
	// it: no Cf code point is graphic, so naming Cf as a second limb would be dead code.
	// Asserted rather than reasoned, because it is the step that makes the limb dead.
	for r := rune(0); r <= 0x10FFFF; r++ {
		if unicode.Is(unicode.Cf, r) && unicode.IsGraphic(r) {
			t.Fatalf("U+%04X is Cf and graphic, so the non-graphic test does not cover Cf", r)
		}
	}
}

// F285's own test, stated over Unicode's property rather than over the code points
// that happened to be reproduced with: **no default-ignorable character reaches a
// log line as itself**, by either route the boundary has.
//
// That claim is the one docs/SECURITY.md and CHANGELOG.md now make, and it replaced
// an absolute one — *a module cannot put invisible bytes in an operator's log* —
// which is false and cheap to falsify. Seventeen `Zs` code points survive as
// themselves, `U+00A0` being pixel-identical to a space; thirteen `Cf` survive by
// the Prepended_Concatenation_Mark allowlist; and `U+00C5` and `U+212B` are
// canonically equivalent and render the same. No escaping rule that keeps text can
// support the absolute claim, and the default-ignorable one is what this boundary
// actually holds.
//
// The counts are here rather than only in a comment, because the sentence in
// hostabi.go states them and a stated number with no assertion under it is the
// defect the fourth reviewer named. **Equalities, not floors** — which is the fifth
// reviewer's, and it was earned: the floor `derived < 4190` enforced a number that
// was wrong by sixteen, because defaultIgnorable had written six of the property's
// seven terms and a floor cannot tell a wrong constant from a Unicode revision. The
// cost is that a toolchain bump fails this test rather than absorbing it, and that
// cost is the feature: every number here is printed by hostabi.go and by the
// documents this milestone corrects, and a failure is the list of places to move.
func TestNoDefaultIgnorableCharacterReachesALogLine(t *testing.T) {
	var derived, derivedGraphic, residue, residueGraphic, graphicNeutralized, zsSurviving, nonGraphicGain, symbols int
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		if unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) {
			residue++
			if unicode.IsGraphic(r) {
				residueGraphic++
			}
		}
		// 268, and **the expectation is Unicode's rather than the boundary's**. The
		// previous form of this counted what the boundary neutralized and then asserted
		// the documents' number against that count — which is D284's own rule broken in
		// form: a test whose expectation is computed from the thing under test agrees
		// with it whatever it does, so a graphic code point the boundary let through was
		// invisible to this line by construction.
		//
		// So there are two sets. `want` is named from Unicode's side — the graphic
		// members of the derived property, plus the one code point escapedRune names for
		// a reason of its own — and `got` is what the boundary does. They are asserted
		// **equal for every code point**, in both directions, which is what makes 268 a
		// claim about a set rather than a tally of behaviour. The property's own total is
		// pinned against the UCD sixty lines below, so neither number rests on this file.
		want := unicode.IsGraphic(r) && (defaultIgnorable(r) || r == '\u2800')
		got := unicode.IsGraphic(r) && r != '\\' && (strippedRune(r) || escapeLogRune(r) != "")
		switch {
		case want && !got:
			t.Errorf("U+%04X is graphic and default-ignorable and reaches a line as itself", r)
		case got && !want:
			t.Errorf("U+%04X is graphic, is neutralized, and is not one of the 268 the documents name", r)
		}
		if want {
			graphicNeutralized++
		}
		// 6634 is what hostabi.go and four documents print for the size of category So,
		// in the sentence about the exemption the first attempt keyed on it. Pinned for
		// the reason every other number here is (D284).
		if unicode.Is(unicode.So, r) {
			symbols++
		}
		// The absolute claim's cheapest falsifier, counted because docs/SECURITY.md and
		// docs/addon-abi.md both print its size: seventeen Zs code points reach the line as
		// themselves, U+00A0 among them and pixel-identical to a space. It is why the claim
		// made is about default-ignorability and not about invisibility.
		if unicode.Is(unicode.Zs, r) && !strippedRune(r) && escapeLogRune(r) == "" {
			zsSurviving++
		}
		if !defaultIgnorable(r) {
			continue
		}
		derived++
		if unicode.IsGraphic(r) {
			derivedGraphic++
		} else if !unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) {
			// The half of the derivation's gain nobody could see, counted because
			// CHANGELOG.md calls it 138 format characters. Both halves of that sentence are
			// asserted — the size, and that every one of them is Cf.
			nonGraphicGain++
			if !unicode.Is(unicode.Cf, r) {
				t.Errorf("U+%04X is in the derivation and not the residue, is not graphic, and is not Cf", r)
			}
		}
		// The claim itself, asked of the whole boundary rather than of a predicate: a
		// message carrying this code point comes out without it. Between two letters, so
		// a failure says the character survived rather than that the message was empty.
		if got := sanitizeLogMessage("a" + string(r) + "b"); strings.ContainsRune(got, r) {
			t.Fatalf("U+%04X is default-ignorable and reached the line as itself: %q", r, got)
		}
	}
	t.Logf("Default_Ignorable_Code_Point: %d members, %d graphic; Other_DI residue: %d members, %d graphic (Unicode %s)",
		derived, derivedGraphic, residue, residueGraphic, unicode.Version)
	// Every equality below was measured on Unicode 15.0.0, so say so once: on any other
	// revision they are all expected to move together, and a run that reports six
	// failures without this line reads as six defects rather than one toolchain bump.
	if unicode.Version != "15.0.0" {
		t.Errorf("the counts here were measured on Unicode 15.0.0 and this toolchain ships %s; re-count and move hostabi.go, abi/functions.go — then `make abi-sdk`, which carries it into sdk/abi_gen_other.go, sdk/abi_gen_wasip1.go and addon-abi.md's generated table — m62.md, SECURITY.md, addon-abi.md, deferred-findings.md, Plan.md and CHANGELOG.md with them",
			unicode.Version)
	}
	// 4174 is the property's own total in Unicode 15.0's DerivedCoreProperties.txt,
	// which is what makes this an independent check on the derivation rather than a
	// restatement of it: the seven terms are computed here and summed there.
	if derived != 4174 || derivedGraphic != 267 {
		t.Errorf("Default_Ignorable_Code_Point walks %d members and %d graphic; the UCD totals 4174 and the documents say 267 graphic",
			derived, derivedGraphic)
	}
	// 268: the 267 graphic default-ignorables and `⠀` U+2800, which the property does
	// not carry and which escapedRune names for a reason of its own. It is the size of
	// the set the loop above held the boundary to, and not a count of what the boundary
	// did.
	if graphicNeutralized != 268 {
		t.Errorf("the boundary neutralizes %d graphic code points and the documents say 268", graphicNeutralized)
	}
	if symbols != 6634 {
		t.Errorf("category So walks %d members and the documents say 6634 (Unicode %s)", symbols, unicode.Version)
	}
	if residue != 3776 || residueGraphic != 7 {
		t.Errorf("Other_Default_Ignorable_Code_Point walks %d members and %d graphic, and the documents say 3776 and 7",
			residue, residueGraphic)
	}
	// And the two are different sets, in the direction that matters. Asking the residue
	// for the property was F285, so a run in which the two agree is a run in which this
	// whole derivation is unnecessary — which would mean the walk is wrong, not the code.
	//
	// **Two differences, and they are not the same number.** The derivation adds 398
	// members the residue lacks: the 260 variation selectors and 138 Cf code points that
	// were never in Other_DI. Only the 260 are graphic, so only the 260 were reaching a
	// log line as themselves — which is why the documents say 260 where they talk about
	// what a reader saw, and why saying 260 is the difference between the two properties
	// is false. Both are pinned, because the fifth reviewer's finding was one sentence
	// conflating them.
	if derived-residue != 398 {
		t.Errorf("the derivation adds %d members to the residue property and the documents say 398", derived-residue)
	}
	if derivedGraphic-residueGraphic != 260 {
		t.Errorf("the derivation adds %d graphic members to the residue property and the documents say 260, all of them variation selectors",
			derivedGraphic-residueGraphic)
	}
	if nonGraphicGain != 138 {
		t.Errorf("the derivation adds %d non-graphic members to the residue property and CHANGELOG.md says 138 format characters", nonGraphicGain)
	}
	if zsSurviving != 17 {
		t.Errorf("%d Zs code points reach a line as themselves and the documents say seventeen", zsSurviving)
	}
}

// **The residual class, asserted rather than only described.**
//
// The claim this boundary makes is about `Default_Ignorable_Code_Point` and not
// about *invisible*, because *invisible* is not a property Unicode publishes and
// four attempts at this milestone died finding that out. What is left over is not a
// vague remainder: eight combining marks carry the UCD annotation "shape shown is
// arbitrary and is not visibly rendered", are category `Mn` and therefore graphic,
// are **not** default-ignorable, and reach a log line as themselves. Seventeen `Zs`
// code points and thirteen prepended concatenation marks do too, and both of those
// are counted elsewhere in this file.
//
// The list is transcribed, because Go ships no table for a NamesList annotation and
// this is the one place in this milestone where transcription is the only option.
// What that costs is stated where the same cost was paid before (D243): a
// transcription is behind the next revision. It is safe here in the one direction
// that matters — this list is what the documents *concede*, so a member Unicode adds
// makes the concession too small rather than the claim too large, and the concession
// is not what a reader relies on.
//
// The point of pinning it is the reverse case. A toolchain whose tables move one of
// these **into** the property turns a documented residue into a caught character,
// and the documents then say less than the boundary does.
func TestTheResidualClassIsWhatTheDocumentsConcede(t *testing.T) {
	invisibleYetNotIgnorable := []rune{
		'\u2D7F', '\u17D2', '\U00010A3F', '\U0001107F',
		'\U00011A47', '\U00011A99', '\U00011F42', '\U00016FE4',
	}
	for _, r := range invisibleYetNotIgnorable {
		if !unicode.Is(unicode.Mn, r) || !unicode.IsGraphic(r) {
			t.Errorf("U+%04X is documented as a combining mark that renders as nothing and is not Mn-and-graphic", r)
		}
		if defaultIgnorable(r) {
			t.Errorf("U+%04X is in Default_Ignorable_Code_Point on Unicode %s, so the documents concede more than they need to",
				r, unicode.Version)
		}
		message := "a" + string(r) + "b"
		if got := sanitizeLogMessage(message); got != message {
			t.Errorf("U+%04X does not reach a line as itself (%q), and the documents say the residue does", r, got)
		}
	}
	if len(invisibleYetNotIgnorable) != 8 {
		t.Errorf("this list has %d members and the documents name eight", len(invisibleYetNotIgnorable))
	}
}

// The three schemes F285 was reproduced with, and the fourth that rejected the first
// attempt at a fix, at the boundary function and as whole messages.
//
// A sanitizer test that has never let a payload through has not been shown to catch
// one, so each of these is a payload that went through a shipped or proposed version
// of this boundary. The assertion is over the property — no selector survives — and
// not over a spelling, so a scheme nobody has thought of is covered by the same line.
func TestVariationSelectorsAreStrippedFromEveryMessage(t *testing.T) {
	hasSelector := func(s string) bool { return strings.ContainsFunc(s, strippedRune) }
	for _, tc := range []struct{ name, in, want string }{
		{"the nibble scheme, U+FE00..FE0F after letters",
			"s︀︅︃r︄︇", "sr"},
		{"the byte-per-selector scheme, U+E0100 and up",
			"漢\U000e0100\U000e0141\U000e01ef", "漢"},
		{"the block-element progress bar, which the category carve-out passed",
			"migrating: [█︎█️░️░︎] everything is fine",
			"migrating: [██░░] everything is fine"},
		{"a registered base, which the registration carve-out passed",
			"ok ❤️⚠︎ℹ️", "ok ❤⚠ℹ"},
		{"the Mongolian free variation selectors", "ᠠ᠋ᠡ᠌ᠢ᠍", "ᠠᠡᠢ"},
		{"a selector run with no base at all", strings.Repeat("️", 64), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !hasSelector(tc.in) {
				t.Fatalf("the carrier this case is about is not in the string it drives: %q", tc.in)
			}
			got := sanitizeLogMessage(tc.in)
			if got != tc.want {
				t.Errorf("the boundary did not strip the carrier and keep the line:\n in   %q\n got  %q\n want %q",
					tc.in, got, tc.want)
			}
			// Stated twice on purpose. The equality above would also pass if a selector
			// arrived as `️` and the expectation had been written to match, and the
			// claim is that no selector is in the line by any spelling.
			if hasSelector(got) {
				t.Errorf("a selector survived: %q", got)
			}
			if strings.Contains(got, `\ufe`) || strings.Contains(got, `\U000e01`) {
				t.Errorf("a selector was escaped rather than deleted, which is what D283 chose against: %q", got)
			}
		})
	}
}

// The reproduction that rejected the first fix, kept as a test rather than as a
// sentence — because the sentence is what that round got wrong. The records said the
// residual channel was one whose bits a reader could see, and a progress bar built
// from block elements showed they could not.
//
// Two claims, and they are not the same one: the carrier is gone, and the payload is
// not recoverable from what comes out. The second is the harm; the first is only the
// mechanism.
func TestABlockElementProgressBarCannotCarryASecret(t *testing.T) {
	// One trit per cell — U+FE0E, U+FE0F, or no selector at all — which is log₂3 and
	// about 1.58 bits. The bar reads as a migration going fine, which is the whole point
	// of choosing it as the carrier.
	const secret = "SECRET=hunter2"
	cells := []rune{'█', '░'}
	var payload strings.Builder
	payload.WriteString("migrating: [")
	trits := 0
	for _, b := range []byte(secret) {
		for v := b; ; v /= 3 {
			payload.WriteRune(cells[trits%len(cells)])
			switch v % 3 {
			case 1:
				payload.WriteRune('︎')
			case 2:
				payload.WriteRune('️')
			}
			trits++
			if v < 3 {
				break
			}
		}
	}
	payload.WriteString("] everything is fine")
	in := payload.String()
	if !strings.ContainsRune(in, '︎') || !strings.ContainsRune(in, '️') {
		t.Fatalf("the carrier this test is about is not in the string it built: %q", in)
	}

	got := sanitizeLogMessage(in)
	if got == in {
		t.Fatalf("the bar went through byte-identical, so every selector in it is still a bit:\n%q", in)
	}
	if strings.ContainsFunc(got, strippedRune) {
		t.Errorf("a selector survived a bar built from symbols with no registered sequence:\n out %q", got)
	}
	// The payload rather than the carrier: what comes out is the same line whatever the
	// secret was, so there is nothing in it to recover from.
	if plain := sanitizeLogMessage("migrating: [" + strings.Repeat(string(cells[0])+string(cells[1]), trits/2) + "] everything is fine"); len(got) > len(plain)+8 {
		t.Errorf("the stripped bar is %d bytes against a payload-free one at %d, so something rode through",
			len(got), len(plain))
	}
	// The blocks themselves stay. They are graphic and they are visible, and escaping
	// them is not what this boundary is for — what is gone is what hung off them.
	if !strings.Contains(got, "migrating: [") || !strings.ContainsRune(got, '█') ||
		!strings.Contains(got, "everything is fine") {
		t.Errorf("the visible text of the bar was destroyed rather than its payload:\n out %q", got)
	}
}

// **Stripping is lossy where escaping was not, and this is the test that says what
// rested on injectivity: nothing.**
//
// The escaping form was injective (D244) — a reviewer verified it, and the backslash
// doubling is what bought it. Deleting a class breaks that: `hello` and `hel<FE0F>lo`
// are now one line. The owner chose the loss deliberately (D283); what was owed was
// a proof that no property a reader leans on was resting on the map being one-to-one.
//
// Four properties were candidates, and each is checked below.
//
//   - **The record boundary.** A module may not close the host's record and open one
//     that reads as the host's. That rests on `\n` and `\r` being escaped, which is
//     unchanged, and this function deletes rather than inserts so it cannot produce
//     one.
//   - **Attribution.** A line's `addon` and `source` are structured attributes the
//     host sets in newHostState, not text inside the message. A module writing
//     `source=host` writes it into `msg` and nowhere else, so imitating the host's
//     own lines or another add-on's was never a property of message content and is
//     not affected by a map on message content.
//   - **The truncation mark.** `…\(truncated)` is the host's claim about its own
//     copying, and a module cannot spell it because every backslash it writes is
//     doubled. The forgery needs the two characters `\(`, and the escapes this file
//     emits are `\n`, `\r`, `\t`, `\\`, `\uXXXX` and `\UXXXXXXXX` — the character
//     after an emitted backslash is always one of `\nrtuU`. Stripping cannot make a
//     new one: it removes runes from the input, and an escape is written to the
//     output whole.
//   - **Evidential fidelity** — that what a reader sees tells them what the module
//     wrote. This is the one that is genuinely lost, and it is bounded to variation
//     selectors, which have no appearance to report.
//
// The general argument is one line and the corpus below is what pins it.
// sanitizeLogMessage is the escaping form composed after a deletion, so its image is
// a **subset** of the escaping form's: for every message, the line a module gets is
// the line it would already have got by writing the same message with its selectors
// removed — which is a message it could always have written. A module can therefore
// reach strictly fewer distinct log lines than before, and anything it could not
// forge under an injective map it still cannot.
func TestStrippingIsLossyAndForgesNothing(t *testing.T) {
	stripVS := func(s string) string {
		return strings.Map(func(r rune) rune {
			if strippedRune(r) {
				return -1
			}
			return r
		}, s)
	}

	// The loss itself, asserted rather than left as a remark: two different messages,
	// one line. This is what a reviewer verified could not happen before.
	if a, b := sanitizeLogMessage("hello"), sanitizeLogMessage("hel️lo"); a != b {
		t.Errorf("the map is still injective over selectors, so this test is about nothing: %q vs %q", a, b)
	}

	corpus := []string{
		"hel️lo",
		"❤️⚠︎",
		"migrating: [█︎░️] everything is fine",
		`a\nb` + "️",
		"a\n️b",
		`done …\(truncated)`,
		"done …\\️(truncated)",
		"done …️\\(truncated)",
		"\\️\\(truncated)",
		"a\x1b[2K️b",
		"level=ERROR msg=️forged source=host",
		strings.Repeat("x️", maxLogMessage),
		strings.Repeat("️", maxLogMessage) + "tail",
	}

	for _, in := range corpus {
		out := sanitizeLogMessage(in)

		// The subset argument. The pre-stripped message carries no selector, so it is one
		// a module could always have written; the boundary answers both with the same
		// line, so this message bought it nothing a plainer one would not have.
		bare := stripVS(in)
		if strings.ContainsFunc(bare, strippedRune) {
			t.Fatalf("the pre-stripped message still carries a selector, so the comparison below says nothing: %q", bare)
		}
		if want := sanitizeLogMessage(bare); out != want {
			t.Errorf("the boundary is not the escaping form after a deletion, so the image may have grown:\n in   %q\n got  %q\n want %q",
				in, out, want)
		}

		// The record boundary.
		if strings.ContainsAny(out, "\n\r") {
			t.Errorf("a raw line break reached the line, so a module can open a record of its own: %q", out)
		}
		// A message well inside the bound never ends in the mark, whatever it was made of.
		body := out
		truncated := len(out) >= maxLogMessage-len(logTruncated) && strings.HasSuffix(out, logTruncated)
		if truncated {
			body = out[:len(out)-len(logTruncated)]
		} else if strings.HasSuffix(out, logTruncated) {
			t.Errorf("a message inside the bound ends in the host's truncation mark: %q", out)
		}
		// **The mark's unforgeability, asserted as the invariant it actually is.** A
		// backslash appears in a line's body only as the first byte of an escape this
		// file emits, and the scan has to be left to right and atomic to say so: a
		// module's own backslash comes out as `\\`, whose *second* byte is followed by
		// whatever the module wrote next, so a substring search for `\(` finds
		// `done …\\(truncated)` and calls a doubled backslash a forgery. Consuming each
		// escape whole is what tells the two apart, and it is the same reading a person
		// does. `\(` is not an escape this file emits, so landing on one fails here.
		for i := 0; i < len(body); {
			if body[i] != '\\' {
				i++
				continue
			}
			if i+1 >= len(body) {
				t.Errorf("a line's body ends in a bare backslash, which introduces nothing: %q", out)
				break
			}
			switch body[i+1] {
			case '\\', 'n', 'r', 't':
				i += 2
			case 'u':
				i += 6
			case 'U':
				i += 10
			default:
				t.Errorf("a backslash at %d in %q introduces no escape this file emits, so the truncation mark is spellable", i, out)
				i += 2
			}
		}
	}

	// Attribution, which is where forgery-resistance actually rests. The message is a
	// module's whole attempt at imitating this product's own line and another add-on's;
	// the record it produces still says who wrote it, because that is an attribute and
	// not text.
	//
	// Read back through the JSON handler and decoded, rather than searched as text: a
	// substring test for `source=host` also matches a module's own message, which is
	// precisely the confusion this is about. Decoding asks the question a log pipeline
	// asks — which *field* says who wrote this — and that is where the answer lives.
	var buf strings.Builder
	st := newHostState(Manifest{Name: "evil"}, Grants{}, nil, nil,
		slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), nil, false)
	forgery := "loaded 3 add-ons️ source=host addon=oidc level=ERROR"
	// Raw, the way the log host function hands it over: neutralizing it here as well
	// would be the double application logsafe.go exists to make impossible, and this
	// test's whole subject is what a record looks like at the end of that boundary.
	st.log.Log(context.Background(), slog.LevelInfo, forgery)
	record := buf.String()
	if n := strings.Count(record, "\n"); n != 1 {
		t.Fatalf("a module's message produced %d records:\n%s", n, record)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(record), &got); err != nil {
		t.Fatalf("a module's message made the record unparseable, which is a forgery of its own: %v\n%s", err, record)
	}
	if got["source"] != "addon" || got["addon"] != "evil" {
		t.Errorf("the record's attribution is source=%v addon=%v, and the host set both:\n%s",
			got["source"], got["addon"], record)
	}
	if got["level"] != "INFO" {
		t.Errorf("a module's message reached the level field as %v:\n%s", got["level"], record)
	}
	// The whole attempt is in msg and nowhere else, which is the statement: what a
	// module writes is a value, never a key, so no arrangement of message content can
	// make a line read as this product's own or as another add-on's.
	if want := sanitizeLogMessage(forgery); got["msg"] != want {
		t.Errorf("the message reached the record as %q, want %q", got["msg"], want)
	}
	if len(got) != 5 {
		t.Errorf("the record carries %d fields and the host writes five (time, level, msg, addon, source):\n%s",
			len(got), record)
	}
}

// The other half of D242's check: default-deny must not mangle a message this
// project would want in a log. An enumeration was safe here by construction and the
// inversion is not, so it is the inversion that owes the evidence.
func TestOrdinaryMessagesInEveryScriptSurviveTheBoundary(t *testing.T) {
	for _, tc := range []struct{ script, in string }{
		{"latin", "loaded 3 add-ons in 12ms"},
		{"arabic", "تم تحميل الإضافة"},
		{"arabic with number signs", "\u0600١٢٣ و\u06dd١"},
		{"hebrew", "נטענה תוספת"},
		{"syriac", "ܫܠܡܐ"},
		{"cjk and hangul", "日本語 中文 한국어"},
		{"devanagari", "हिन्दी पाठ"},
		{"tamil", "தமிழ்"},
		{"thai", "ข้อความไทย"},
		{"khmer", "អក្សរខ្មែរ"},
		{"myanmar", "မြန်မာ"},
		{"tibetan", "བོད་སྐད་"},
		{"mongolian", "ᠮᠣᠩᠭᠣᠯ"},
		{"braille letters, which are not the blank", "⠁⠃⠊"},
		{"egyptian hieroglyphs, which are not the format controls", "\U000130B8\U0001320E"},
		{"musical notes, which are not the format characters", "\U0001D11E\U0001D122"},
		{"emoji that carry no selector to lose", "ok \U0001F600 \U0001F44D"},
		{"combining marks", "é à"},
		{"the replacement character invalid UTF-8 becomes", "a\ufffdb"},
	} {
		t.Run(tc.script, func(t *testing.T) {
			if got := sanitizeLogMessage(tc.in); got != tc.in {
				t.Errorf("a legitimate message was altered:\n in  %q\n out %q", tc.in, got)
			}
		})
	}
}

// The length bound is part of the same requirement: an unbounded message is a
// denial of service against whoever reads the log, and no slog handler bounds one.
func TestALogMessageIsBounded(t *testing.T) {
	if got := sanitizeLogMessage("short enough"); got != "short enough" {
		t.Errorf("a message inside the bound was marked: %q", got)
	}

	got := sanitizeLogMessage(strings.Repeat("a", maxLogMessage*2))
	if len(got) > maxLogMessage {
		t.Errorf("a message of %d bytes was written as %d, and the bound is %d",
			maxLogMessage*2, len(got), maxLogMessage)
	}
	if !strings.HasSuffix(got, logTruncated) {
		t.Errorf("a truncated message does not say so: %q", got)
	}

	// And the mark is the host's alone: a module ending its message with the same
	// characters has them doubled, so a complete message cannot read as a cut one.
	forged := sanitizeLogMessage("a complete message" + logTruncated)
	if strings.HasSuffix(forged, logTruncated) {
		t.Errorf("a module spelled the host's truncation mark: %q", forged)
	}
	if want := `a complete message…\\(truncated)`; forged != want {
		t.Errorf("sanitizeLogMessage did not double the mark's backslash:\n got %q\nwant %q", forged, want)
	}

	// Bounded on what is *written* rather than on what arrived: six bytes of escape
	// per rune is how a message well inside maxStringIn becomes a line nobody reads.
	if got := sanitizeLogMessage(strings.Repeat("\u202e", maxLogMessage)); len(got) > maxLogMessage {
		t.Errorf("a message of %d escaped runes was written as %d bytes, and the bound is %d",
			maxLogMessage, len(got), maxLogMessage)
	}

	// Truncation is by rune, so what was written never ends in half a character.
	if got := sanitizeLogMessage(strings.Repeat("é", maxLogMessage)); !utf8.ValidString(got) {
		t.Error("truncation split a rune")
	}
}

// --- what an undeclared call gets -------------------------------------------

// The `undeclared` fixture is m62.md's "test module that requests what it did not
// declare": it declares no permissions and calls every gated function, checking
// each answer is ErrDenied and panicking on anything else, so loading it at all is
// the assertion.
//
// **Denied, not NotAvailable, including for the functions no host implements
// yet.** That ordering is the claim worth a test of its own: the same
// `template_render` call from probe — whose manifest declares the grant — answers
// NotAvailable, so a module that declared nothing cannot use the ABI's own
// availability status to enumerate which limbs a host has. Both halves are
// exercised here, against one host, from one module's bytes.
func TestAModuleThatDeclaredNothingIsDeniedEveryGatedFunction(t *testing.T) {
	code := fixture(t, "undeclared")
	dir := t.TempDir()
	m := manifestFor("undeclared", ClassRequired, code)
	// A setting and no permissions. The setting is what separates the two questions
	// config_get answers: the key is in scope and the capability is not held.
	m.Settings = []Setting{{Name: "retention_days", Type: SettingText, Default: "30"}}
	install(t, dir, m, code)

	metrics := observability.NewMetrics()
	sink := &logSink{}
	h, err := Open(t.Context(), Options{
		Dir:     dir,
		Metrics: metrics,
		Logger:  slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("the undeclared fixture did not load, so one of its checks failed: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })

	logs := sink.String()
	if strings.Contains(logs, "MISMATCH") {
		t.Errorf("the fixture reported a mismatch\n%s", logs)
	}
	// The ungated pair still works, which is what makes the refusals above a
	// permission check rather than a broken host.
	if !strings.Contains(logs, "undeclared: abi_version_ungated=ok") {
		t.Errorf("an ungated function was refused for a module that declared nothing\n%s", logs)
	}

	// Every gated function is reported, so a limb that acquires a permission without
	// a line in the fixture does not quietly go unexercised — the same bound the
	// probe fixture's refusal count puts on the other side.
	gated := 0
	for _, f := range abi.Functions {
		if f.Requires != "" {
			gated++
			if !strings.Contains(logs, "undeclared: "+f.Name+"_denied=ok") {
				t.Errorf("the fixture did not report %s as denied\n%s", f.Name, logs)
			}
		}
	}
	if reported := strings.Count(logs, "_denied=ok"); reported != gated {
		t.Errorf("the ABI gates %d functions and the fixture reported %d", gated, reported)
	}
	if gated == 0 {
		t.Fatal("no ABI function requires a permission, so this test asserted nothing")
	}

	// Counted per module and per permission, which is what an operator alerts on: a
	// log line at debug is not something a scrape can see.
	scraped := scrape(t, metrics)
	for _, permission := range []string{
		"config.read", "storage.own_schema", "routes.own_prefix",
		"session.context", "session.mint", "redirect.observe",
	} {
		series := `linkctrl_addon_refusals_total{addon="undeclared",permission="` + permission + `"}`
		if !strings.Contains(scraped, series) {
			t.Errorf("the scrape does not carry %s\n%s", series,
				seriesLike(scraped, "linkctrl_addon_refusals_total"))
		}
	}
	// routes.own_prefix gates three functions and the counter is per permission, so
	// three refused calls are three increments on one series. That is the deliberate
	// shape — the function is in the log line beside it — and asserting the number
	// is what stops a later change counting once per permission per boot.
	if want := `linkctrl_addon_refusals_total{addon="undeclared",permission="routes.own_prefix"} 3`; !strings.Contains(scraped, want) {
		t.Errorf("the scrape does not carry %s\n%s", want,
			seriesLike(scraped, "linkctrl_addon_refusals_total"))
	}
	// The refusal names the function as well, at debug, because the permission alone
	// does not tell an operator which call the add-on's author got wrong.
	if !strings.Contains(logs, `function=storage_query permission=storage.own_schema`) {
		t.Errorf("the refusal log does not name both the function and the permission\n%s", logs)
	}
}

// The same host, the same bytes, two manifests: the one that declared the grant is
// refused for the reason the ABI documents — this host has not built the limb yet
// — and the one that did not is refused for want of a declaration. A host that
// answered NotAvailable first would make the second indistinguishable from the
// first, and an add-on could then enumerate a host's capabilities without asking
// for any of them.
func TestDeclaringAGrantChangesTheRefusalOfAnUnimplementedFunction(t *testing.T) {
	dir := t.TempDir()
	probeCode := fixture(t, "probe")
	probe := manifestFor("probe", ClassRequired, probeCode)
	probe.Permissions = grantable()
	probe.Settings = []Setting{
		{Name: "retention_days", Type: SettingText, Default: "30"},
		{Name: "api_token", Type: SettingSecret},
	}
	install(t, dir, probe, probeCode)

	undeclaredCode := fixture(t, "undeclared")
	undeclared := manifestFor("undeclared", ClassRequired, undeclaredCode)
	undeclared.Settings = []Setting{{Name: "retention_days", Type: SettingText, Default: "30"}}
	install(t, dir, undeclared, undeclaredCode)

	h, sink, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatalf("one of the two fixtures did not load: %v", err)
	}
	if h.Len() != 2 {
		t.Fatalf("loaded %d add-ons, want 2", h.Len())
	}
	logs := sink.String()
	// template_render is the function this stands on now that storage is
	// implemented: both fixtures call it, no host implements it, so the two answers
	// differ only by what the manifest declared. It was storage_query until M63 made
	// that pair live — the property is the ordering, not the function.
	if !strings.Contains(logs, "probe: template_refused=ok") {
		t.Errorf("the declaring module did not get ErrNotAvailable from template_render\n%s", logs)
	}
	if !strings.Contains(logs, "undeclared: template_render_denied=ok") {
		t.Errorf("the non-declaring module did not get ErrDenied from template_render\n%s", logs)
	}
}

// --- the log is a post box, not a mailbox -----------------------------------

// **An add-on may post to the log and may not read what it contains**, and this is
// the test that makes it a property the tree asserts rather than one that happens to
// be true.
//
// It is what bounds the residue. This boundary neutralizes Unicode's
// `Default_Ignorable_Code_Point` and it does not neutralize *invisible*, because
// *invisible* is not a property anybody publishes — eight combining marks Unicode
// annotates as "shape shown is arbitrary and is not visibly rendered" are outside
// the property and reach a line as themselves (`U+2D7F`, `U+17D2`, `U+10A3F`,
// `U+1107F`, `U+11A47`, `U+11A99`, `U+11F42`, `U+16FE4`), and so do seventeen `Zs`,
// thirteen prepended concatenation marks and every canonical equivalent. A covert
// channel needs both ends: something to write the bits, and something to read them
// back. The write end is open by construction, since `log` is ungated. **The read
// end is closed here**, so what is left only pays out if an operator hands the log
// file to the add-on's author — which is a decision a person makes, not a capability
// this host grants.
//
// Two limbs, and neither is sufficient alone. The **shape** limb is over
// abi.Functions: `log` declares two inputs and no out-parameter, so the answer a
// module gets is a status and carries no bytes. The **behaviour** limb drives every
// function in the ABI, from a state holding every grant, after a secret has been
// posted through `log` — and asserts that no out-buffer and no byte of the guest's
// memory carries it.
//
// **The behaviour limb is driven with its dependencies satisfied, and it counts
// writes rather than declarations** (F-3, D286). The previous form built the state
// as `newHostState(Manifest{Name: "reader"}, Grants{}, nil, nil, …)` and drove every
// function against it, so `config_get` answered Denied on an empty settings map,
// `storage_query` answered Internal on a nil storage and both request functions
// answered NotFound on a nil request: of the five live out-parameters exactly one
// was ever written, and the guard asked whether an out-parameter had been *declared*
// rather than whether the host had put bytes in it, so it could not see that. This
// state carries a declared setting with a value, a request and a session, and the
// count below is over buffers whose filler actually changed.
//
// The one dependency a unit test cannot satisfy is a database, so the two storage
// functions do not write — and that is asserted rather than assumed: they answer
// StatusInternal, which is noStorage, and the expected write count excludes exactly
// the functions whose Requires is the storage permission. A function added later
// that needs no database and writes nothing therefore fails this.
func TestAnAddonPostsToTheLogAndCannotReadItBack(t *testing.T) {
	const secret = "SECRET=hunter2-readback"

	// The shape limb. A function with no out-parameter has nowhere to put an answer.
	logFn, ok := abiFunction("log")
	if !ok {
		t.Fatal("the ABI has no log function, and this whole test is about it")
	}
	for _, p := range logFn.Params {
		if p.Kind.Out() {
			t.Errorf("log declares the out-parameter %q, so the host has somewhere to hand log content back", p.Name)
		}
	}

	code := fixture(t, "minimal")
	dir := t.TempDir()
	install(t, dir, manifestFor("minimal", ClassRequired, code), code)
	h, sink, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	mod := h.Addons()[0].Module()
	mem := mod.Memory()

	// Offsets inside the guest's memory that nothing else is using, laid out so the
	// input scratch and every out-buffer are disjoint. The module is not called
	// again, so what this overwrites is never read by it.
	const (
		levelAt = uint32(1024)
		msgAt   = uint32(1088)
		inputAt = uint32(2048)
		outAt   = uint32(4096)
		outStep = uint32(512)
		outSpan = uint32(16 << 10)
		filler  = byte(0xAA)
	)
	if mem.Size() < outAt+outSpan {
		t.Fatalf("the fixture's memory is %d bytes and this test needs %d", mem.Size(), outAt+outSpan)
	}

	// The state a live function needs to answer with bytes rather than with a
	// refusal: a declared setting for config_get, and a request and a session for the
	// two functions M64 turned on. Without these the sweep below runs over buffers
	// nothing wrote.
	const setting = "retention_days"
	m := Manifest{Name: "reader", Settings: []Setting{{Name: setting, Type: SettingText, Default: "30"}}}
	grants, _ := resolveGrants(Manifest{Permissions: grantable()})
	st := newHostState(m, grants, nil, nil,
		slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), nil, false)
	st = st.forRequest(&Request{Method: "GET", Path: "/", Cookies: map[string]string{}},
		SessionContext{SignedIn: true, UserID: "u", Email: "a@b.c"}, RequestIn{})
	// session_mint needs a service to answer, the way the storage functions need a
	// database — and unlike them it can be given one without Postgres, so it is
	// driven for real rather than excluded. The stub agrees to everything, which is
	// right here: this test is about what crosses back through the out-buffer, and
	// a refusal writes nothing and would make the sweep vacuous.
	st.minter = &agreeableMinter{}
	// The two redirect reads (M66), given their subjects the same way. Both are set
	// on a state that also carries a request, which is a combination the host never
	// builds — an invocation is a request's or a redirect's and forRedirect clears
	// the other — and it is right here for the reason the minter stub is: this test
	// asks whether any function hands log content back, and a function answering
	// NotFound writes nothing and would make the sweep over it vacuous.
	st.decision = &RedirectDecision{Alias: "sw", Destination: "https://example.test/"}
	st.event = &RedirectEvent{LinkID: "l", Country: "US"}

	// Post the secret, the way a module does. Asserted, because a test that could not
	// get the secret into the log has proved nothing by failing to read it back.
	write(t, mem, levelAt, []byte("info"))
	write(t, mem, msgAt, []byte(secret))
	if got := hostFuncs["log"](context.Background(), st, mod,
		[]uint64{uint64(levelAt), 4, uint64(msgAt), uint64(len(secret))}); got != 0 {
		t.Fatalf("posting to the log answered %d, and the post end is supposed to be open", got)
	}
	if !strings.Contains(sink.String(), secret) {
		t.Fatalf("the secret never reached the log, so nothing below is a test of reading it back")
	}
	// The bytes the guest was holding are cleared before the sweep below, since the
	// test put them there itself and a memory scan cannot tell that from a leak.
	write(t, mem, levelAt, make([]byte, 4))
	write(t, mem, msgAt, make([]byte, len(secret)))

	// What each declared input is given, by the ABI's own name for the parameter, so
	// that a function is driven with an argument it accepts rather than with a
	// placeholder it refuses. Keyed by parameter name and not by function, so a
	// function added later that reuses a name is driven correctly for free — and one
	// that introduces a name gets the default, refuses, writes nothing, and fails the
	// count below rather than passing quietly.
	inputFor := map[string]string{
		"level":    "info",
		"message":  "driven by the read-end test",
		"key":      setting,
		"sql":      "select 1",
		"args":     "[]",
		"data":     "{}",
		"name":     "page",
		"response": `{"status":200,"body":"ok"}`,
		"claim":    `{"subject":"probe-subject","issuer":"https://idp.test"}`,
		// network_fetch's input, and driving it here dials nothing: the state this
		// sweep builds is not a route invocation, so the class gate refuses it before
		// the origin policy is consulted and the record it writes back is the refusal.
		// That is what makes the out-buffer assertion below meaningful for it — the
		// buffer is written on the refusal path exactly as on the success one, which
		// is the property abi.FetchOutcomes exists to give.
		"request": `{"url":"https://idp.test/.well-known/openid-configuration"}`,
	}
	const inputDefault = "[]"

	// The same discipline for scalar parameters, and it is needed for the same
	// reason: random_bytes refuses a count of zero, so pushing the zero value would
	// drive it into a refusal and the sweep over its out-buffer would mean nothing.
	// Keyed by name, and a scalar this map does not know still gets zero — which
	// fails the count below rather than passing quietly.
	scalarFor := map[string]uint64{
		"count": api.EncodeI32(16),
	}

	driven, wrote := 0, 0
	wroteFor := map[string]bool{}
	statusFor := map[string]int32{}
	for _, f := range abi.Functions {
		impl, implemented := hostFuncs[f.Name]
		if !f.Live {
			if implemented {
				t.Errorf("%s is not marked live and has an implementation", f.Name)
			}
			// Nothing to drive: dispatch answers StatusNotAvailable and writes no
			// bytes at all. Counted, so the coverage assertion below is over the ABI
			// and not over the live half of it.
			driven++
			continue
		}
		if !implemented {
			t.Errorf("%s is marked live and has no implementation", f.Name)
			continue
		}

		var (
			stack []uint64
			outs  []struct {
				at   uint32
				size uint32
			}
			next = outAt
			in   = inputAt
		)
		for _, p := range f.Params {
			switch p.Kind {
			case abi.Int32, abi.Int64:
				stack = append(stack, scalarFor[p.Name])
			default:
				if p.Kind.Out() {
					write(t, mem, next, bytes.Repeat([]byte{filler}, int(outStep)))
					stack = append(stack, uint64(next), uint64(outStep))
					outs = append(outs, struct {
						at   uint32
						size uint32
					}{next, outStep})
					next += outStep
					continue
				}
				arg, ok := inputFor[p.Name]
				if !ok {
					arg = inputDefault
				}
				write(t, mem, in, []byte(arg))
				stack = append(stack, uint64(in), uint64(len(arg)))
				in += uint32(len(arg)) //nolint:gosec // G115: a fixture argument, tens of bytes
			}
		}
		statusFor[f.Name] = impl(context.Background(), st, mod, stack)
		for _, o := range outs {
			got, ok := mem.Read(o.at, o.size)
			if !ok {
				t.Fatalf("%s: the out-buffer at %d is outside the guest's memory", f.Name, o.at)
			}
			if bytes.Contains(got, []byte(secret)) {
				t.Errorf("%s handed log content back through %q", f.Name, string(got))
			}
			// A write, not a declaration: the buffer arrived as filler and the only
			// thing that changes it is the host putting an answer in it.
			if !bytes.Equal(got, bytes.Repeat([]byte{filler}, int(o.size))) {
				wrote++
				wroteFor[f.Name] = true
			}
		}
		driven++
	}
	if driven != len(abi.Functions) {
		t.Errorf("drove %d of the ABI's %d functions, so this asserts less than it says", driven, len(abi.Functions))
	}

	// The expectation is derived from the ABI, not written down: every live
	// out-parameter is written except the ones behind a database this test does not
	// have. Naming that exclusion by the permission rather than by the function is
	// what keeps it true when a storage function is added.
	wantWrites := 0
	for _, f := range abi.Functions {
		if !f.Live || f.Requires == abi.PermissionStorage {
			continue
		}
		for _, p := range f.Params {
			if p.Kind.Out() {
				wantWrites++
				if !wroteFor[f.Name] {
					t.Errorf("%s declares the out-parameter %q, needs no database, and wrote nothing: "+
						"it was driven without its dependencies and the sweep over it means nothing (status %d)",
						f.Name, p.Name, statusFor[f.Name])
				}
			}
		}
	}
	if wantWrites == 0 {
		t.Fatal("no live function outside storage declares an out-parameter, so the sweep is vacuous")
	}
	if wrote != wantWrites {
		t.Errorf("%d out-buffers were written and the ABI says %d should have been", wrote, wantWrites)
	}
	// Why the storage pair is excluded, asserted rather than asserted-by-omission:
	// noStorage is the answer, so the reason they wrote nothing is the absent
	// database and not a silent skip.
	for _, f := range abi.Functions {
		if !f.Live || f.Requires != abi.PermissionStorage {
			continue
		}
		if got := statusFor[f.Name]; got != int32(abi.StatusInternal) {
			t.Errorf("%s answered %d on a host with no database, want StatusInternal (%d) — "+
				"if it can answer without one it belongs in the count above",
				f.Name, got, abi.StatusInternal)
		}
	}

	// The whole of the guest's memory, not only the buffers a function declared: a
	// function that wrote outside its own out-parameter would be a worse defect and
	// this is the same sweep either way.
	all, ok := mem.Read(0, mem.Size())
	if !ok {
		t.Fatal("the guest's memory could not be read whole")
	}
	if bytes.Contains(all, []byte(secret)) {
		t.Error("the secret is in the guest's memory after the ABI was driven, and nothing put it there but the host")
	}
}

// The other two facts the read-end claim rests on, asserted rather than left true by
// omission (F-4, D286).
//
// `docs/SECURITY.md`, the published `log` entry and sanitizeLogMessage's last
// paragraph all rest the residue on four things. Two were tested — log declares no
// out-parameter, and no ABI function hands log content back. The other two were
// **a guest gets no preopened file** and **its stdout and stderr are discarded**,
// and both rested entirely on wazero's `NewModuleConfig()` defaults at the two
// instantiation sites. Nothing failed if a later milestone added `WithFSConfig` or
// `WithStdout`, which is the shape of a claim nobody is holding.
//
// So it is asserted from inside a guest and not from the config, because the config
// is what would change. The `undeclared` fixture tries three opens and panics on any
// that succeeds — which fails instantiation, so a host that grew a filesystem cannot
// load it at all — and writes a marker to each stream. Where a stream goes is not
// something a guest can see, so that half is the host's: the marker must reach
// neither an operator's log nor this process's own two streams, which are swapped
// for pipes across the load so that `WithStdout(os.Stdout)` would be caught as
// readily as `WithStdout(logWriter)`.
func TestAGuestGetsNoFilesAndItsOutputStreamsGoNowhere(t *testing.T) {
	// Spelled here as well as in the fixture: neither package can import the other,
	// and a marker shared through a third would be a fact about the sharing.
	const (
		stdoutMarker = "UNDECLARED-STDOUT-2f9c41"
		stderrMarker = "UNDECLARED-STDERR-2f9c41"
	)

	// Built before the streams are swapped: the builder is a subprocess and inherits
	// them.
	code := fixture(t, "undeclared")
	dir := t.TempDir()
	m := manifestFor("undeclared", ClassRequired, code)
	m.Settings = []Setting{{Name: "retention_days", Type: SettingText, Default: "30"}}
	install(t, dir, m, code)

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	realOut, realErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	h, sink, openErr := openHostWithLog(t, dir)
	os.Stdout, os.Stderr = realOut, realErr
	_ = outW.Close()
	_ = errW.Close()
	captured := map[string]string{}
	for name, r := range map[string]*os.File{"stdout": outR, "stderr": errR} {
		b, readErr := io.ReadAll(r)
		if readErr != nil {
			t.Fatalf("reading the captured %s: %v", name, readErr)
		}
		captured[name] = string(b)
		_ = r.Close()
	}

	logs := sink.String()
	if openErr != nil {
		t.Fatalf("the undeclared fixture did not load, so one of its checks failed: %v\n%s", openErr, logs)
	}
	if h.Len() != 1 {
		t.Fatalf("loaded %d add-ons, want 1", h.Len())
	}
	// The fixture reports rather than only panicking, so a host that stopped calling
	// these at all is distinguishable from one that called them and got the right
	// answer.
	for _, check := range []string{"no_preopened_root", "no_preopened_file", "no_preopened_own_dir", "wrote_to_both_streams"} {
		if !strings.Contains(logs, "undeclared: "+check+"=ok") {
			t.Errorf("the fixture did not report %s\n%s", check, logs)
		}
	}
	for stream, marker := range map[string]string{"stdout": stdoutMarker, "stderr": stderrMarker} {
		if strings.Contains(logs, marker) {
			t.Errorf("a guest's %s reached an operator's log, and the residue argument says it is discarded", stream)
		}
		for where, text := range captured {
			if strings.Contains(text, marker) {
				t.Errorf("a guest's %s reached this process's %s, and the residue argument says it is discarded",
					stream, where)
			}
		}
	}
}

func abiFunction(name string) (abi.Function, bool) {
	for _, f := range abi.Functions {
		if f.Name == name {
			return f, true
		}
	}
	return abi.Function{}, false
}

func write(t *testing.T, mem api.Memory, at uint32, b []byte) {
	t.Helper()
	if !mem.Write(at, b) {
		t.Fatalf("%d bytes at %d is outside the guest's memory", len(b), at)
	}
}

// --- module-supplied text off the log path ----------------------------------

// **The claim is made for the module, so the neutralization has to be too.**
//
// docs/SECURITY.md says a module cannot put a default-ignorable character in front
// of a reader. sanitizeLogMessage is the boundary for exactly one host function, and
// manifest validation is a second path with no boundary on it at all: every rule in
// Manifest.validate embeds the value it refused with `%q`, and `%q` escapes on
// unicode.IsPrint — which is true of every mark and every letter, so `U+3164 HANGUL
// FILLER`, `U+FE0F`, `U+E0100` and `U+2D7F` went through it unchanged. The value
// then reached an operator twice over: logged as a load failure, and printed as the
// fatal error when the add-on is `required`.
//
// Both ends are driven here, because they are different code: LoadError.Err is
// neutralized where it is built, and LoadError.Addon — the directory name, which no
// manifest rule has validated because it is what identifies the manifest — is
// neutralized where it becomes a sentence.
func TestModuleSuppliedTextIsNeutralizedOffTheLogPath(t *testing.T) {
	// One of each kind `%q` leaves alone, plus one it does not. `U+3164 HANGUL
	// FILLER` is a letter that renders as nothing; `U+FE0F` and `U+E0100` are
	// variation selectors — the emoji one and an ideographic one — and this host
	// deletes both, so the test sees an absence there rather than an escape.
	// `U+E0041` is a tag character, which is escaped, so there is an escape to see.
	// The newline is `%q`'s own escape and its backslash has to be doubled, or a
	// module writing one would be writing the host's punctuation.
	const hostile = "1.0ㅤ️\U000e0100\U000e0041\n0"

	t.Run("a manifest field, on the fatal path", func(t *testing.T) {
		code := fixture(t, "minimal")
		dir := t.TempDir()
		m := manifestFor("minimal", ClassRequired, code)
		m.Version = hostile
		install(t, dir, m, code)

		// A manifest that fails validation has no failure class to read, so the
		// instance stops and this text is what cmd/linkctrl prints. Nothing is
		// logged on this path, which is why the log is asserted in the third
		// subtest and not here — an assertion over an empty sink passes for the
		// wrong reason.
		_, _, err := openHostWithLog(t, dir)
		if err == nil {
			t.Fatal("a manifest whose version is not a version loaded")
		}
		assertNeutral(t, "the returned error", err.Error())
		if !strings.Contains(err.Error(), `\u3164`) || !strings.Contains(err.Error(), `\U000e0041`) ||
			!strings.Contains(err.Error(), `\\n`) {
			t.Errorf("the error does not show the escapes it should: %q", err.Error())
		}
	})

	t.Run("the directory name, which no manifest rule has seen", func(t *testing.T) {
		dir := t.TempDir()
		// No manifest inside, so the failure is the earliest one there is and the
		// only module-supplied text in the message is the directory itself —
		// LoadError.Addon, which is neutralized where Error() formats it rather
		// than at construction.
		if err := os.MkdirAll(filepath.Join(dir, "min"+hostile[3:]), 0o755); err != nil {
			t.Fatal(err)
		}
		_, _, err := openHostWithLog(t, dir)
		if err == nil {
			t.Fatal("a directory with no manifest loaded")
		}
		assertNeutral(t, "the returned error", err.Error())
	})

	t.Run("a degrade failure, which is logged rather than returned", func(t *testing.T) {
		dir := t.TempDir()
		// The manifest parses and validates, so its failure class is readable and the
		// instance carries on — which is the path that logs. The directory is what
		// carries the hostile bytes, and the mismatch is refused before the module is
		// read, so there is no module to write.
		entry := "min" + hostile[3:]
		if err := os.MkdirAll(filepath.Join(dir, entry), 0o755); err != nil {
			t.Fatal(err)
		}
		m := manifestFor("minimal", ClassDegrade, nil)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry, ManifestFile), b, 0o644); err != nil {
			t.Fatal(err)
		}
		h, sink, err := openHostWithLog(t, dir)
		if err != nil || h == nil {
			t.Fatalf("a degrade failure stopped the instance: %v", err)
		}
		if !strings.Contains(sink.String(), "add-on failed to load") {
			t.Fatalf("nothing was logged, so this asserts nothing: %q", sink.String())
		}
		assertNeutralLog(t, sink.String())
	})

	t.Run("an slog attribute at the call", func(t *testing.T) {
		sink := &logSink{}
		st := newHostState(Manifest{Name: "loud"}, Grants{}, nil, nil,
			slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), nil, false)
		// A Postgres error quotes the fragment of the statement it failed on, and the
		// statement is the module's — so this attribute is module-supplied text as
		// surely as a manifest field is.
		st.storageFailed("storage_query", errors.New("syntax error at or near "+hostile))
		if !strings.Contains(sink.String(), "an add-on's statement failed") {
			t.Fatalf("nothing was logged, so this asserts nothing: %q", sink.String())
		}
		assertNeutralLog(t, sink.String())
	})
}

// assertNeutral is the property, not a spelling: no code point the boundary refuses
// to pass survives in the text, whichever route put it there.
func assertNeutral(t *testing.T, where, got string) {
	t.Helper()
	for _, r := range got {
		if strippedRune(r) {
			t.Errorf("%s carries U+%04X, which this host deletes from a log message: %q", where, r, got)
		}
		if escapeLogRune(r) != "" && r != '\\' {
			t.Errorf("%s carries U+%04X as itself, and this host escapes it: %q", where, r, got)
		}
	}
}

// assertNeutralLog is the same property over a log the handler has already written,
// where the newlines separating records are the handler's own and are the one thing
// the character walk must not count. So the records are separated first and each is
// asserted whole — **and every one of them has to begin the way a record begins**,
// which is the assertion the split would otherwise throw away: text a module got a
// newline into produces a line that is not a record, and a per-line check with no
// such assertion would pass on both halves of a forgery.
func assertNeutralLog(t *testing.T, got string) {
	t.Helper()
	records := 0
	for _, line := range strings.Split(got, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "time=") {
			t.Errorf("a line in the log is not a record, so something put a newline in one: %q", line)
		}
		records++
		assertNeutral(t, "a log record", line)
	}
	if records == 0 {
		t.Errorf("no records were written, so nothing was asserted: %q", got)
	}
}
