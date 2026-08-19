package addon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tetratelabs/wazero/api"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// This file is the host half of the ABI. The guest half is the generated SDK in
// sdk/, and neither of them enumerates the functions: both derive from
// abi.Functions, which is the whole point of that slice existing.
//
// # What is live, and what is declared
//
// Five functions work here — abi_version, log, config_get and the two storage
// functions M63 turned on — and the rest are
// registered and answer abi.StatusNotAvailable to an add-on that holds the
// permission they cost, and abi.StatusDenied to one that does not. Which of the two
// comes first is the next section's, and it is a decision rather than an accident.
// That the remainder is refused rather than absent is m61.md's requirement, not a
// shortcut: the contract has to be complete on paper before it is complete in
// behaviour, because the add-on repository compiles against it from its first
// commit and cannot wait for six milestones. The refusal is a status a module
// branches on rather than a link failure it cannot, and implementing a refused
// function is explicitly not a breaking change — docs/addon-abi.md says so.
//
// The pairing is checked rather than trusted: registerABI panics if a function
// abi.Functions marks Live has no implementation here, or if one it does not mark
// Live has one. A milestone that implements a limb and forgets to flip the flag
// therefore fails at construction, in every test that opens a host, instead of
// shipping a function the documentation calls refused.
//
// # What a call costs
//
// Every function names the permission it costs in abi.Function.Requires, and
// dispatch refuses a call whose grant the calling add-on did not declare —
// **before** it refuses one this host has not implemented, so a module that
// declared nothing cannot probe for the limbs a host has. Grants are resolved once
// at load (grants.go) because from M66 this check sits on the redirect path. Two
// functions cost nothing: abi_version reports a constant, and log is the one
// capability granted on purpose, since a module's stdout and stderr are discarded
// and it has no other way out.
//
// # How one host module serves many add-ons
//
// wazero resolves imports by module name from the runtime's registry, so there is
// exactly one "linkctrl" module per runtime and every add-on imports the same
// one. Scoping is per *call*: api.GoModuleFunction hands the implementation the
// **calling** module, whose name is the add-on's — Open sets it from the manifest
// — and hostState turns that into the manifest and logger for the add-on that
// called. The state is registered before instantiation because package
// initialization runs *during* it, which is the one window where a module can
// call a host function before Open has finished with it.

// maxStringIn bounds a single value crossing from a guest into the host.
//
// 64 KiB, and it is a liveness bound rather than a validation rule. A module can
// address up to wazero's memory limit, so a log call naming a length of a
// gigabyte is a gigabyte copied into host memory on the host's own heap, once per
// call, from code an operator did not write. Nothing in this ABI has a legitimate
// argument that large: the largest is a SQL statement.
const maxStringIn = 64 << 10

// hostFunc is one live ABI function. It reads its arguments off the wasm stack —
// the layout is abi.Function.Params, expanded by hostSignature — and returns what
// the caller gets: a length, or one of abi.Statuses.
type hostFunc func(ctx context.Context, st *hostState, mod api.Module, stack []uint64) int32

// hostFuncs is every function this host implements. A name here that
// abi.Functions does not mark Live, or a Live one missing from here, is a
// programming error registerABI refuses to build past.
var hostFuncs = map[string]hostFunc{
	"abi_version": func(_ context.Context, _ *hostState, mod api.Module, stack []uint64) int32 {
		return writeOut(mod, stack[0], stack[1], []byte(abi.Version))
	},

	"log": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		level, ok := readString(mod, stack[0], stack[1])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		message, ok := readString(mod, stack[2], stack[3])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		if !slices.Contains(abi.LogLevels, level) {
			// Not defaulted to info. A level nobody spelled correctly becomes a
			// line nobody greps for, and the add-on's author is the one person who
			// can fix it.
			return int32(abi.StatusInvalid)
		}
		// Neutralized here, on the way in, and never by whatever writes the line —
		// see sanitizeLogMessage. The level needs none of it: it is compared against
		// a closed vocabulary one line above rather than passed through.
		st.log.Log(context.Background(), slogLevels[level], sanitizeLogMessage(message))
		return 0
	},

	"config_get": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		key, ok := readString(mod, stack[0], stack[1])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		s, declared := st.settings[key]
		if !declared {
			// Denied rather than not-found, and the difference is the whole of the
			// scoping: an undeclared key is not a value that happens to be absent,
			// it is a value this add-on has no standing to ask for. A module cannot
			// probe for another add-on's settings or for this product's own
			// configuration, because neither is in its manifest.
			//
			// The second of two questions, and dispatch has already answered the
			// first: `config.read` is whether this add-on may read settings at all,
			// and this is whether the key is one of its own. Both answer Denied and
			// only the first is a permission, which is why only the first is
			// counted in the refusals metric.
			return int32(abi.StatusDenied)
		}
		if s.Default == "" {
			// A declared setting with nothing behind it yet. Secrets are here by
			// construction — a manifest may not give one a default — and so is any
			// setting whose value the Add-on manager will supply.
			return int32(abi.StatusNotFound)
		}
		return writeOut(mod, stack[2], stack[3], []byte(s.Default))
	},

	// The two storage functions (M63). They share every line of their argument
	// handling and differ in one thing that matters: a query runs in a READ ONLY
	// transaction and an exec does not, so which of the pair a module called is a
	// fact Postgres enforces rather than a description of intent.
	"storage_query": func(ctx context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		statement, args, status := readStatement(mod, stack)
		if status != 0 {
			return status
		}
		if st.storage == nil {
			return st.noStorage("storage_query")
		}
		rows, err := st.storage.Query(ctx, statement, args)
		if err != nil {
			return st.storageFailed("storage_query", err)
		}
		return writeOut(mod, stack[4], stack[5], rows)
	},

	"storage_exec": func(ctx context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		statement, args, status := readStatement(mod, stack)
		if status != 0 {
			return status
		}
		if st.storage == nil {
			return st.noStorage("storage_exec")
		}
		if err := st.storage.Exec(ctx, statement, args); err != nil {
			return st.storageFailed("storage_exec", err)
		}
		// Zero, not a row count. The ABI's convention is that a non-negative answer
		// is a length, and this function has no out parameter for one to be the
		// length of; inventing a meaning for the number here would be a second
		// convention for one i32.
		return 0
	},
}

// readStatement reads the (sql, args) pair both storage functions begin with.
//
// The layout is abi.Function.Params expanded by hostSignature: the statement is
// stack[0..1] and the JSON argument array is stack[2..3], which is the same for
// both because both declare the same first two parameters. A test holds that
// sentence to the ABI rather than leaving it as a comment.
func readStatement(mod api.Module, stack []uint64) (string, []any, int32) {
	statement, ok := readString(mod, stack[0], stack[1])
	if !ok {
		return "", nil, int32(abi.StatusInvalid)
	}

	if statement == "" {
		return "", nil, int32(abi.StatusInvalid)
	}
	raw, ok := readBytes(mod, stack[2], stack[3])
	if !ok {
		return "", nil, int32(abi.StatusInvalid)
	}
	args, err := store.DecodeAddonArgs(raw)
	if err != nil {
		// The guest's fault and the guest's to fix, so nothing is logged: an add-on
		// looping on a malformed argument list would otherwise decide how much an
		// instance logs.
		return "", nil, int32(abi.StatusInvalid)
	}
	return statement, args, 0
}

// noStorage is the answer when this add-on holds the grant and the host has no
// database to honour it with — a host constructed without one, which in this
// product is a test and never an instance. See Host.openStorage, which has already
// said so once at load; this says it again per call at debug, because a module
// branching on the status deserves the reason to be findable.
func (s *hostState) noStorage(function string) int32 {
	s.hostLog.Debug("an add-on called a storage function and this host has no database",
		slog.String("addon", s.manifest.Name),
		slog.String("function", function))
	return int32(abi.StatusInternal)
}

// storageFailed turns a database error into the status the guest gets, and puts the
// detail where an operator can read it.
//
// **The message never crosses**, which is the same rule StatusInternal is
// documented with: a Postgres error names tables, columns and constraints, and an
// add-on that can read one can print this product's schema into somebody's page.
// What the guest gets is a number it can branch on.
//
// Denied is separated from Invalid on purpose. A privilege refusal is confinement
// working, and it is the one failure the add-on's author cannot fix by editing
// their statement — telling them apart is what lets a module report "that is not
// mine to read" rather than "your SQL is wrong". It costs the module nothing it did
// not already know: it knows which schema it owns.
func (s *hostState) storageFailed(function string, err error) int32 {
	status := abi.StatusInvalid
	level := slog.LevelDebug
	if errors.Is(err, store.ErrAddonDenied) {
		status = abi.StatusDenied
		// Warned rather than debugged, and this is the one storage failure worth
		// waking somebody for: a module reaching outside its schema is either a bug
		// its author has not noticed or an attempt. Either way an operator wants to
		// know, and it is bounded — a module that keeps trying is a module whose log
		// volume is already the smaller problem.
		level = slog.LevelWarn
	}
	s.hostLog.Log(context.Background(), level,
		"an add-on's statement failed",
		slog.String("addon", s.manifest.Name),
		slog.String("function", function),
		slog.Any("error", err))
	return int32(status)
}

// slogLevels maps the ABI's level vocabulary onto slog's. Keyed off
// abi.LogLevels by a test, so a level added there without a mapping here fails
// rather than logging at debug.
var slogLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// maxLogMessage bounds the line one log call can put in front of a reader.
//
// Not the same bound as maxStringIn, which protects this process's heap: 64 KiB of
// escaped control characters is a record nobody reads, and a module writing one per
// call is a denial of service against whoever has to read the log rather than
// against the host. 4 KiB is longer than any message with something to say and
// short enough that a truncated one is still greppable. The bound is on what gets
// written, after escaping, so it holds whatever the message was made of.
const maxLogMessage = 4 << 10

// logTruncated is what a reader sees in place of the rest. Present or absent, it
// answers the question a bounded line otherwise leaves open — whether the message
// ended or the host stopped copying it.
//
// It carries a backslash, and that is the whole of what makes it the host's own
// (D244). Every backslash a module writes is doubled by escapeLogRune, so a lone one
// in a written line can only have come from this file: a module ending its message
// with `…(truncated)` produces exactly those characters and reads as a message that
// was cut, while one ending it with `…\(truncated)` reads as `…\\(truncated)`. The
// mark is a claim the host makes about its own copying, so a module has to be unable
// to make it.
const logTruncated = `…\(truncated)`

// sanitizeLogMessage neutralizes a message a module handed over, before the logger
// sees it.
//
// D240's requirement, and the reason it lives here rather than in the logger: log
// is ungated, so every loaded module can reach it including one that declared no
// permission at all, which makes this the widest untrusted input this host has —
// and there is no second boundary between this function and an operator's screen.
// Two harms follow, and a permission check would have stopped neither. A message
// carrying a newline can close the host's own record and open one that reads as the
// host's, and log records are what an operator reasons from when something has gone
// wrong. A message carrying an ANSI escape, a zero-width character or a bidi
// override can put bytes in front of a reader arranged to be overlooked.
//
// Escaped rather than dropped, because what a module tried to write is evidence: a
// reader who sees \n knows more than one handed a message with a hole in it. Not
// delegated to slog either, though both handlers NewLogger builds do quote what
// they write: which handler an operator configured is not something this boundary
// may depend on, and neither of them bounds a length.
func sanitizeLogMessage(s string) string {
	var b strings.Builder
	// Sized to what will be written, not to what arrived: the input may be maxStringIn
	// and the output cannot exceed maxLogMessage, and Builder.String does not copy — so
	// growing to the input would leave a 4 KiB line holding a 64 KiB array for as long
	// as the log record lives.
	b.Grow(min(len(s), maxLogMessage))
	// The mark is reserved rather than appended over the bound, so maxLogMessage is
	// the size of what gets written and not the size of most of it.
	limit := maxLogMessage - len(logTruncated)
	for _, r := range s {
		esc := escapeLogRune(r)
		n := len(esc)
		if esc == "" {
			n = utf8.RuneLen(r)
		}
		if b.Len()+n > limit {
			b.WriteString(logTruncated)
			break
		}
		if esc == "" {
			// WriteRune, so a byte sequence that was not valid UTF-8 becomes the
			// replacement character rather than reaching the log as itself. readString
			// has already refused one, and this function is also called on its own.
			b.WriteRune(r)
			continue
		}
		b.WriteString(esc)
	}
	return b.String()
}

// escapeLogRune is the escape for a rune that may not reach a log line as itself,
// or "" for one that may. The ones a reader actually meets keep their familiar
// spellings; everything else becomes its code point, which is what makes an
// invisible character visible rather than merely absent.
//
// **Backslash is escaped although it is graphic, and that is what makes the escaping
// injective** (D244). Left alone, a module writing the two characters `\` and `n`
// produced a line byte-identical to the one a real newline produced, so a reader
// could not tell which had happened — and this log is meant to be read as evidence.
// Doubling it is what strconv.Quote does, for this reason. It is the one place where
// a graphic character does not reach the line as itself, and it is the reason
// logTruncated can be a mark a module cannot forge.
func escapeLogRune(r rune) string {
	switch r {
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	case '\\':
		return `\\`
	}
	if !escapedRune(r) {
		return ""
	}
	if r > 0xFFFF {
		return `\U` + hexRune(r, 8)
	}
	return `\u` + hexRune(r, 4)
}

const hexDigits = "0123456789abcdef"

func hexRune(r rune, digits int) string {
	out := make([]byte, digits)
	for i := digits - 1; i >= 0; i-- {
		out[i] = hexDigits[r&0xf]
		r >>= 4
	}
	return string(out)
}

// escapedRune reports whether a rune may not reach a log line as itself.
//
// **It is a default-deny, and that shape is the decision** (D242). The first form of
// this function enumerated the invisible code points, and an enumeration is
// permanently behind Unicode: it missed U+061C ARABIC LETTER MARK, which arrived in
// the same revision as the bidi isolates it did cover, along with U+180E, the
// interlinear annotation controls and the musical and hieroglyphic format
// characters. Three documents describe this function's set as closed, and a list a
// Unicode revision can outdate cannot keep that description true.
//
// So the test is inverted: **everything that is not a graphic character is escaped**
// — Cc, Cf, Cn, Co, Zl and Zp, which is the C0 and C1 controls, every format
// character, every unassigned code point and every private use one. Category Cf
// needs no limb of its own; no Cf code point is graphic, checked over the whole
// range rather than assumed.
//
// Three corrections sit on top, each a named case rather than a category, and each
// one a place default-deny alone answers wrongly.
//
// **What the inversion costs**, stated because it is the mirror of what the
// enumeration cost: Cn means unassigned *in the Unicode tables this Go was built
// with*, so a code point assigned after them is escaped until Go catches up. The
// enumeration's staleness was a hole a new code point walked through; this one is a
// message that reads worse for a release. Failing closed on the way Unicode moves is
// the trade.
func escapedRune(r rune) bool {
	if meaningfulFormatRune(r) {
		return false
	}
	// A Braille blank cell is a genuine graphic character and is not
	// default-ignorable, so neither limb below reaches it. What earns it a named case is
	// that it is the one blank which is not whitespace: the seventeen Zs code points
	// that survive here render as nothing too — U+2000-U+200A, U+202F, U+205F and
	// U+3000 among them, several wider than a space — but a reader knows whitespace when
	// they meet it and so does everything that trims, collapses or splits on it, while a
	// run of U+2800 is content that looks like blank. Padding is not the reason and
	// cannot be: what a line can hold is bounded by maxLogMessage whatever it is made
	// of. Braille text an add-on logs gets its spaces escaped: loud rather than silent,
	// which is the direction this whole function leans.
	if r == '\u2800' {
		return true
	}
	// Seven code points are letters or marks by category — the Hangul fillers, the
	// Khmer inherent vowels, the combining grapheme joiner — and render as nothing, so
	// IsGraphic says yes where a reader sees no. Unicode has a property for exactly
	// this class and Go carries its non-Cf residue under this name.
	if unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) {
		return true
	}
	return !unicode.IsGraphic(r)
}

// meaningfulFormatRune is the allowlist D241 argued for: format characters that
// carry meaning in a message rather than hiding one.
//
// Each is a prefixed sign that scopes the digits after it — the Arabic number, sign,
// footnote marker, end-of-ayah, pound and piastre marks, the Syriac abbreviation
// mark, the Kaithi number signs. They are Cf, so default-deny would escape them, and
// a sanitizer that mangles Arabic is a bug with a worse blast radius than the one it
// prevents. Nothing here is invisible in the sense that matters: each changes how the
// run that follows it is read, in the place a reader is already looking.
//
// **It is Unicode's own property, not a transcription of one** (D243). Those
// characters are exactly `Prepended_Concatenation_Mark`, and the first form of this
// allowlist copied that property's members out by hand — eleven of the thirteen, so
// U+0890 ARABIC POUND MARK and U+0891 ARABIC PIASTRE MARK were escaped from the day
// it was written. That is the staleness U+061C was, and it is what D242 replaced an
// enumeration to be rid of one function up: a list a Unicode revision can outdate
// cannot carry a claim that names its members. Reading the table Go already ships
// means a toolchain update moves the allowlist with it.
//
// This is the whole of what default-deny gives back, which is why it is one
// predicate. Everything else in Cf is escaped, U+061C included — D241's own
// reasoning put it there and only the code disagreed.
func meaningfulFormatRune(r rune) bool {
	return unicode.Is(unicode.Prepended_Concatenation_Mark, r)
}

// hostState is what one add-on's calls are answered against.
type hostState struct {
	manifest Manifest
	// log carries the add-on's name and marks the line as an add-on's own, so an
	// operator reading a log can tell this product's words from a module's.
	log *slog.Logger
	// hostLog is the host's own voice about this add-on: it names the add-on and
	// does not mark the line as the add-on's words. The distinction is the one
	// dispatch already makes when it refuses a call — a refusal is the host
	// speaking about a module, not the module speaking.
	hostLog *slog.Logger
	// settings is manifest.Settings by name, which is what config_get scopes to.
	settings map[string]Setting
	// grants is what this add-on holds, resolved once at load. Read on every call
	// through the ABI and never rebuilt — see Grants, where the reason it has to be
	// a lookup rather than a walk of the manifest is the redirect path's budget.
	grants Grants
	// storage is this add-on's confined connection to the schema it owns, or nil
	// for one that declared no storage — and also for one that declared it on a
	// host with no database, which noStorage answers.
	storage *store.AddonDB
}

func newHostState(m Manifest, grants Grants, storage *store.AddonDB, log *slog.Logger) *hostState {
	settings := make(map[string]Setting, len(m.Settings))
	for _, s := range m.Settings {
		settings[s.Name] = s
	}
	return &hostState{
		manifest: m,
		log: log.With(
			slog.String("addon", m.Name),
			slog.String("source", "addon"),
		),
		hostLog:  log,
		settings: settings,
		grants:   grants,
		storage:  storage,
	}
}

// registerState makes an add-on's state reachable from a host function before the
// module that will call one exists. Returns the deregistration.
func (h *Host) registerState(m Manifest, grants Grants, storage *store.AddonDB) func() {
	st := newHostState(m, grants, storage, h.log)
	h.mu.Lock()
	if h.states == nil {
		h.states = make(map[string]*hostState)
	}
	h.states[m.Name] = st
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.states, m.Name)
		h.mu.Unlock()
	}
}

func (h *Host) hostState(name string) *hostState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.states[name]
}

// registerABI instantiates the one host module every add-on imports.
//
// Called once per runtime, from Open, and only when there is an add-ons directory
// to read — an instance with none constructs no runtime, so it registers no
// module either and the ABI costs it nothing.
func (h *Host) registerABI(ctx context.Context) error {
	b := h.runtime.NewHostModuleBuilder(abi.HostModule)
	for _, f := range abi.Functions {
		impl, ok := hostFuncs[f.Name]
		switch {
		case f.Live && !ok:
			// A construction-time panic rather than an error, because it cannot
			// depend on anything an operator did: abi.Functions and hostFuncs are
			// both in this binary, so this is a build that should not have linked.
			panic(fmt.Sprintf("addon: ABI function %q is marked live and has no host implementation", f.Name))
		case !f.Live && ok:
			panic(fmt.Sprintf("addon: ABI function %q has a host implementation and is not marked live", f.Name))
		}
		params, results := hostSignature(f)
		b.NewFunctionBuilder().
			WithGoModuleFunction(h.dispatch(f, impl), params, results).
			WithName(f.Name).
			Export(f.Name)
	}
	if _, err := b.Instantiate(ctx); err != nil {
		return fmt.Errorf("host module %q: %w", abi.HostModule, err)
	}
	return nil
}

// dispatch is the wrapper every ABI function is registered through: it finds the
// calling add-on's state, checks the grant the function costs, and answers on the
// wasm stack.
//
// **The permission check comes before everything else, including before a refused
// function's StatusNotAvailable.** Two reasons, and the second is the one worth
// stating: an add-on that declared nothing must not be able to use the ABI's own
// capability probe to enumerate which limbs this host implements, and a refusal
// counted per module is only complete if it counts the calls to functions that do
// not work yet — which is every capability worth abusing until M63 to M66 land.
//
// The check is a map lookup on a set resolved at load. It sits on the redirect
// path from M66, where the inherited rule is a cached p99 under 20 ms, so it
// touches no manifest, no vocabulary and nothing on disk. See Grants.
func (h *Host) dispatch(f abi.Function, impl hostFunc) api.GoModuleFunc {
	return func(ctx context.Context, mod api.Module, stack []uint64) {
		st := h.hostState(mod.Name())
		if st == nil {
			// The calling module is not one this host registered state for, which
			// is either a module instantiated outside Open or a name collision the
			// directory-equals-name rule is supposed to make impossible. Refuse,
			// and say so where an operator can see it: silently answering with
			// another add-on's configuration is the failure worth being loud about.
			h.log.Error("an add-on called a host function and the host has no state for it",
				slog.String("module", mod.Name()),
				slog.String("function", f.Name))
			stack[0] = api.EncodeI32(int32(abi.StatusInternal))
			return
		}
		if f.Requires != "" && !st.grants.Has(f.Requires) {
			// Counted always, logged at debug. The counter is what an operator
			// alerts on and it is bounded — one series per add-on per permission —
			// while a warning per call would be a module's own loop deciding how
			// much an instance logs, on a path that from M66 is the redirect path.
			// The add-on's name comes from a validated manifest and the permission
			// from a closed vocabulary, so neither label is guest input.
			h.metrics.ObserveAddonRefusal(st.manifest.Name, f.Requires)
			// h.log rather than st.log: st.log marks a line as the add-on's own
			// words, and this is the host's refusal of them.
			h.log.Debug("refused an add-on's call: it did not declare the permission "+
				"the function needs",
				slog.String("addon", st.manifest.Name),
				slog.String("function", f.Name),
				slog.String("permission", f.Requires))
			stack[0] = api.EncodeI32(int32(abi.StatusDenied))
			return
		}
		if impl == nil {
			stack[0] = api.EncodeI32(int32(abi.StatusNotAvailable))
			return
		}
		stack[0] = api.EncodeI32(impl(ctx, st, mod, stack))
	}
}

// hostSignature expands an ABI function's parameters into the wasm value types
// wazero needs. It is the same expansion the generator applies to produce the
// //go:wasmimport declarations, from the same slice, which is what makes a
// signature mismatch between host and guest unrepresentable rather than merely
// unlikely.
func hostSignature(f abi.Function) (params, results []api.ValueType) {
	for _, p := range f.Params {
		switch p.Kind {
		case abi.Int32:
			params = append(params, api.ValueTypeI32)
		case abi.Int64:
			params = append(params, api.ValueTypeI64)
		default:
			// A pointer and a length, both i32: wasm32 addresses are 32 bits and so
			// is every length this convention carries.
			params = append(params, api.ValueTypeI32, api.ValueTypeI32)
		}
	}
	return params, []api.ValueType{api.ValueTypeI32}
}

// readString copies a (pointer, length) pair out of guest memory.
//
// Copied, not aliased: mod.Memory().Read hands back a window onto the guest's own
// linear memory, and holding one past the call means holding a view the guest can
// rewrite. It also validates UTF-8, because every String parameter in this ABI is
// a name, a level or a statement — things a host would otherwise store or log with
// replacement characters in them.
func readString(mod api.Module, ptr, length uint64) (string, bool) {
	b, ok := readBytes(mod, ptr, length)
	if !ok {
		return "", false
	}
	if !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

func readBytes(mod api.Module, ptr, length uint64) ([]byte, bool) {
	n := api.DecodeU32(length)
	if n == 0 {
		return nil, true
	}
	if n > maxStringIn {
		return nil, false
	}
	view, ok := mod.Memory().Read(api.DecodeU32(ptr), n)
	if !ok {
		return nil, false
	}
	out := make([]byte, len(view))
	copy(out, view)
	return out, true
}

// writeOut implements the out-parameter half of the calling convention: the value
// goes into the guest's buffer when it fits, and either way the answer is the
// size it occupies.
//
// The "does not fit" case writes nothing at all rather than truncating. A
// truncated JSON record is a parse error a publisher debugs as a host bug; a size
// they can allocate against is the retry the SDK already does for them.
func writeOut(mod api.Module, ptr, capacity uint64, value []byte) int32 {
	if len(value) > math.MaxInt32 {
		// Not reachable from anything this host produces, and checked because the
		// convention's return value is an i32: a size that did not fit one would come
		// back negative and the guest would read it as a status. StatusInternal is the
		// honest answer — the host has a value it cannot describe.
		return int32(abi.StatusInternal)
	}
	size := int32(len(value)) //nolint:gosec // G115: bounded against MaxInt32 immediately above
	// Compared as int, because the guest's capacity is a u32 and the size is an i32:
	// converting either into the other's type is the overflow this function just ruled
	// out for one of them and cannot rule out for the other.
	if int(size) > int(api.DecodeU32(capacity)) {
		return size
	}
	if size > 0 && !mod.Memory().Write(api.DecodeU32(ptr), value) {
		return int32(abi.StatusInvalid)
	}
	return size
}
