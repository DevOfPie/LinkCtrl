package addon

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/tetratelabs/wazero/api"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
)

// This file is the host half of the ABI. The guest half is the generated SDK in
// sdk/, and neither of them enumerates the functions: both derive from
// abi.Functions, which is the whole point of that slice existing.
//
// # What is live, and what is declared
//
// Three functions work here — abi_version, log and config_get — and the rest are
// registered and answer abi.StatusNotAvailable. That is m61.md's requirement, not
// a shortcut: the contract has to be complete on paper before it is complete in
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
		st.log.Log(context.Background(), slogLevels[level], message)
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

// hostState is what one add-on's calls are answered against.
type hostState struct {
	manifest Manifest
	// log carries the add-on's name and marks the line as an add-on's own, so an
	// operator reading a log can tell this product's words from a module's.
	log *slog.Logger
	// settings is manifest.Settings by name, which is what config_get scopes to.
	settings map[string]Setting
}

func newHostState(m Manifest, log *slog.Logger) *hostState {
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
		settings: settings,
	}
}

// registerState makes an add-on's state reachable from a host function before the
// module that will call one exists. Returns the deregistration.
func (h *Host) registerState(m Manifest) func() {
	st := newHostState(m, h.log)
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
// calling add-on's state and answers on the wasm stack.
func (h *Host) dispatch(f abi.Function, impl hostFunc) api.GoModuleFunc {
	if impl == nil {
		return func(_ context.Context, _ api.Module, stack []uint64) {
			stack[0] = api.EncodeI32(int32(abi.StatusNotAvailable))
		}
	}
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
