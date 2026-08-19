package addon

import (
	"context"
	"log/slog"
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
		{"a variation selector is a mark and stays", "\u2764\ufe0f", "\u2764\ufe0f"},
		{"invalid UTF-8 becomes the replacement character", "a\xffb", "a\ufffdb"},
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
	// Graphic and still escaped: Unicode's seven graphic default-ignorables, plus the
	// Braille blank cell, which is neither non-graphic nor default-ignorable and renders
	// as nothing anyway.
	blank := map[rune]bool{
		'\u034f': true, '\u115f': true, '\u1160': true, '\u17b4': true, '\u17b5': true,
		'\u3164': true, '\uffa0': true, '\u2800': true,
	}
	// Graphic, escaped, and for neither of those reasons: backslash introduces every
	// escape this function writes, so leaving it would make `\` `n` and a newline the
	// same two bytes in the line (D244).
	introducer := map[rune]bool{'\\': true}

	var allowedSeen, blankSeen, introducerSeen int
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // a surrogate is not a rune a string can carry
		}
		esc := escapeLogRune(r) != ""
		switch {
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
	// Thirteen is what the property carried on Unicode 15.0, the tables this tree's Go
	// was built with. Growth is the mechanism working — a newer toolchain widens the
	// allowlist and this test follows it — so only a shrink is asserted against.
	if allowedSeen < 13 {
		t.Errorf("Prepended_Concatenation_Mark walked %d members and carried 13 when this was written", allowedSeen)
	}
	if blankSeen != len(blank) || introducerSeen != len(introducer) {
		t.Fatalf("walked %d of %d blank and %d of %d introducer code points",
			blankSeen, len(blank), introducerSeen, len(introducer))
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
		{"emoji with a variation selector", "ok \U0001F600 ❤️"},
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
// `storage_query` call from probe — whose manifest declares the grant — answers
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
		"session.mint", "redirect.observe",
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
	// storage_query is the one function both fixtures call and neither host
	// implements, so the two answers differ only by what the manifest declared.
	if !strings.Contains(logs, "probe: declared_but_refused=ok") {
		t.Errorf("the declaring module did not get ErrNotAvailable from storage_query\n%s", logs)
	}
	if !strings.Contains(logs, "undeclared: storage_query_denied=ok") {
		t.Errorf("the non-declaring module did not get ErrDenied from storage_query\n%s", logs)
	}
}
