// Package addon is the WASM host: it discovers add-ons in an operator-owned
// directory, verifies each module against its manifest, and instantiates it or
// refuses it.
//
// # What this is, and what it is not yet
//
// This is the lifecycle every later capability hangs off, built before any of
// them so each seam lands inside a running host rather than beside a
// hypothetical one. A module is found, its manifest read, its declared ABI
// generation checked, its bytes hashed against what the manifest claims, and the
// runtime asked to instantiate it.
//
// The imports it may resolve are the ABI, which is authored in
// internal/addon/abi and registered by hostabi.go. Three of them do something —
// abi_version, log and config_get — and the rest are declared and refused with a
// status a module can branch on, because the contract crosses a repository
// boundary one milestone before the behaviour behind it exists. Storage, routes,
// templates, the session hook and redirect observation are M63 to M66's.
//
// # The runtime, and why it is wazero
//
// The image is built CGO_ENABLED=0 — Dockerfile:86, and the Makefile's `dist`
// cross-compile loop, cited by target because the line number moved twice in one
// day (D218) — and stays so. wazero is the one production WASM runtime that needs
// no cgo, which is why it was named at planning rather than left to the build
// (D211).
//
// # What a module is handed
//
// Nothing that is not required to start. Modules are built with
// GOOS=wasip1 GOARCH=wasm -buildmode=c-shared, so they import
// wasi_snapshot_preview1 and the start function is _initialize rather than
// _start — a reactor, which stays instantiated, rather than a command that runs
// main and exits. WASI preview 1 is instantiated once per host because a Go
// module cannot start without it; every capability *behind* it is left at
// wazero's default, and wazero's defaults are not the operating system:
//
//   - no filesystem is preopened, so fd operations have nothing to reach;
//   - no environment and no arguments are passed;
//   - the clocks are wazero's **fake** clocks and the random source its **fake**
//     source (internal/sys/sys.go:151-175 in wazero v1.12.0), so a module that
//     reads the wall clock sees a frozen one and a module that reads randomness
//     gets a deterministic stream.
//
// That last one is a hazard the ABI has not answered: nothing in abi.Functions
// hands a module a real clock or a real random source, so a module needing either
// has none, and sdk/doc.go tells a publisher so rather than leaving them to
// discover it. A module's writes to stdout and stderr are still discarded —
// routing them into the operator's log would be a capability granted by accident,
// and the log function is the one that was granted on purpose.
//
// # Cost
//
// Measured in TestInstantiationCostIsMeasured, which times compiling and
// instantiating **separately** because only one of the two is a cost a request
// could pay. On the fixture the standard toolchain produces (about 1.85 MB):
// compiling is the expensive step at a few hundred milliseconds and happens once
// per module at boot; instantiating that compiled module costs about 2 ms, and the
// same again for a second instance; the guest's linear memory is about 2.4 MB per
// instance and the host heap grows about 5.4 MB with it. M66 prices a per-request
// budget against those numbers rather than against a guess. D225 records them,
// including what the race detector does to the two durations.
package addon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// StartFunction is what wazero is told to call at instantiation.
//
// Named rather than left at wazero's default of _start, and the difference is
// the whole shape of an add-on: _start runs main and the module exits, which is a
// command. An add-on is a library the host calls into later, which the Go
// toolchain produces with -buildmode=c-shared and which starts at _initialize.
const StartFunction = "_initialize"

// Outcome is the closed vocabulary of load results, and it is a metric label.
//
// Six values, so the series count is the number of installed add-ons times six
// however many times the instance restarts. No error string is ever a label, and
// the only filename that can become one is bounded by nameRe or collapsed to
// InvalidName — see labelFor, which is what keeps the sentence above true of the
// refusal path, where the label is a directory entry and not a validated name.
type Outcome string

const (
	OutcomeLoaded           Outcome = "loaded"
	OutcomeManifestInvalid  Outcome = "manifest_invalid"
	OutcomeChecksumMismatch Outcome = "checksum_mismatch"
	OutcomeModuleUnreadable Outcome = "module_unreadable"
	// OutcomeABIUnsupported is a manifest whose abi_version this host will not
	// serve: built against a newer generation, or against one whose deprecation
	// window has closed. Its own label rather than manifest_invalid, because the
	// manifest is not invalid — it is a perfectly good manifest for a different
	// host, and the operator's fix is a version rather than a syntax error.
	OutcomeABIUnsupported    Outcome = "abi_unsupported"
	OutcomeInstantiateFailed Outcome = "instantiate_failed"
)

// InvalidName is the addon label for a directory whose name could never be an
// add-on's, and it is a bound rather than a nicety.
//
// The refusal path has no manifest to take a name from, so the label is the
// directory entry, which on Linux is any byte string but `/` and NUL. Two things
// then go wrong, and the first is not a metrics problem at all:
//
//   - client_golang **panics** on a label value that is not valid UTF-8 —
//     WithLabelValues, not the scrape — so a directory named in some other
//     encoding would take the process down inside Open, at boot, before anything
//     is serving. Measured, not inferred: the panic reads `label value "\xff\xfe"
//     is not valid UTF-8`.
//   - the cardinality claim above stops being true. Series would be bounded by
//     how many directories exist rather than by how many add-ons are installed,
//     and the two differ exactly when somebody is fixing a broken install.
//
// Names nameRe accepts are used as they are; everything else lands here, and two
// badly named directories sharing one series is the point. The angle brackets are
// load-bearing: nameRe cannot produce them, so this cannot collide with a real
// add-on's name.
//
// What is *not* wrong is the exposition itself. client_golang escapes a newline,
// a quote and a backslash in a label value, so those reach a scrape as `\n`, `\"`
// and `\\` and the text format stays line-oriented — checked, because the
// plausible-sounding version of this comment claims otherwise.
const InvalidName = "<invalid>"

// labelFor is the directory entry, made safe to publish as a label value.
func labelFor(entry string) string {
	if nameRe.MatchString(entry) {
		return entry
	}
	return InvalidName
}

// LoadError is why one add-on did not load, carrying the label the metric needs.
type LoadError struct {
	// Addon is the directory the add-on was found in, which is its name: the two
	// are required to match, so this is knowable even when the manifest is the
	// thing that failed to parse.
	Addon   string
	Outcome Outcome
	Err     error

	// class is the failure class read from the manifest, or empty when the
	// manifest is what failed. Unexported: it decides whether the instance stops,
	// which is this package's answer to give, not a caller's to override.
	class FailureClass
}

func (e *LoadError) Error() string {
	return fmt.Sprintf("add-on %q: %s: %v", e.Addon, e.Outcome, e.Err)
}

func (e *LoadError) Unwrap() error { return e.Err }

// Options is what a host needs. Everything but Dir is optional.
type Options struct {
	// Dir is LINKCTRL_ADDONS_DIR. Empty means there is no host at all — Open
	// returns a nil *Host, constructs no runtime and starts no goroutine.
	Dir string

	Logger  *slog.Logger
	Metrics *observability.Metrics
}

// Loaded is one add-on that instantiated.
type Loaded struct {
	Manifest Manifest
	// Dir is the absolute directory the add-on was loaded from.
	Dir string

	module api.Module
}

// MemorySize is the guest's linear memory in bytes, which is the resident cost
// of holding this add-on instantiated.
func (l Loaded) MemorySize() uint32 {
	if l.module == nil || l.module.Memory() == nil {
		return 0
	}
	return l.module.Memory().Size()
}

// Module exposes the instance so a later milestone can call into it. It is the
// only reason this type is exported and there is nothing to call yet.
func (l Loaded) Module() api.Module { return l.module }

// Host is the runtime and the add-ons instantiated in it.
//
// Every method is nil-safe, because a nil *Host is the ordinary state of an
// instance that configured no add-ons directory and no caller should have to ask
// whether add-ons happen to be enabled.
type Host struct {
	runtime wazero.Runtime
	loaded  []Loaded
	log     *slog.Logger

	// states is what an ABI call is answered against, keyed by the calling
	// module's name — see hostabi.go. Guarded because a host function is called
	// from whatever goroutine the guest is running on: at boot that is Open's own,
	// and from M64 on it is a request's.
	mu     sync.RWMutex
	states map[string]*hostState
}

// Open discovers, verifies and instantiates every add-on in opts.Dir.
//
// Returns (nil, nil) when Dir is empty. That is the zero-cost case and it is
// exact: no runtime is constructed, no goroutine started, no metric series
// created and no route mounted, each of which is asserted by a test in this
// package rather than promised here.
//
// Returns an error when a `required` add-on fails to load, or when one fails
// before its failure class could be read — see loadOne. The error names the
// add-on and the reason, and the caller's contract is to stop the instance with
// it: that is what the class means.
func Open(ctx context.Context, opts Options) (*Host, error) {
	if opts.Dir == "" {
		return nil, nil
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("addons dir %q: %w", opts.Dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A directory that was configured and cannot be read is a configuration
		// error, not an absence. config.Validate says so before boot reaches
		// here; this is the same answer when the tree changes underneath it.
		return nil, fmt.Errorf("addons dir %q: %w", dir, err)
	}

	// Sorted, so a boot log and a scrape describe the add-ons in the same order
	// every time. os.ReadDir already sorts; stated rather than relied on, since
	// the guarantee is what makes the log diffable across restarts.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	h := &Host{log: log}
	// The runtime is constructed only once there is a directory to read, so the
	// unset case above costs nothing. WithCloseOnContextDone is set at birth
	// because it is what lets M66 interrupt a module that will not return: a
	// deadline enforced by cancelling a context does nothing unless the runtime
	// is watching for it.
	h.runtime = wazero.NewRuntimeWithConfig(ctx,
		wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, h.runtime); err != nil {
		_ = h.runtime.Close(ctx)
		return nil, fmt.Errorf("wasi preview 1: %w", err)
	}
	// The ABI, registered before any module is compiled: an import that does not
	// resolve is a link failure, so the whole declared set has to exist in the
	// runtime before the first instantiation rather than growing as milestones
	// land. hostabi.go is where "declared" and "refused" part company.
	if err := h.registerABI(ctx); err != nil {
		_ = h.runtime.Close(ctx)
		return nil, err
	}

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			// Ignored rather than refused. An operator's README or a stray
			// checksum file in the directory they were told to put add-ons in is
			// not a reason for the instance to stop, and saying so out loud is
			// enough to explain why nothing loaded.
			log.Warn("ignoring a non-directory in the add-ons directory; an add-on "+
				"is a directory holding "+ManifestFile+" and its module",
				slog.String("entry", name))
			continue
		}
		loaded, err := loadOne(ctx, h, filepath.Join(dir, name), name)
		if err != nil {
			var le *LoadError
			if !errors.As(err, &le) {
				// loadOne returns nothing else; belt, so a future edit that
				// forgets cannot lose the metric label.
				le = &LoadError{Addon: name, Outcome: OutcomeManifestInvalid, Err: err}
			}
			// labelFor, not name: this is the one label in the file taken from a
			// directory entry rather than from a validated manifest.
			opts.Metrics.ObserveAddonLoad(labelFor(name), string(le.Outcome))
			if fatal(le) {
				_ = h.Close(ctx)
				return nil, err
			}
			log.Error("add-on failed to load; the instance continues without it",
				slog.String("addon", name),
				slog.String("outcome", string(le.Outcome)),
				slog.String("failure_class", string(ClassDegrade)),
				slog.Any("error", le.Err))
			continue
		}
		opts.Metrics.ObserveAddonLoad(loaded.Manifest.Name, string(OutcomeLoaded))
		opts.Metrics.SetAddonInfo(loaded.Manifest.Name, loaded.Manifest.Version,
			loaded.Manifest.ABIVersion, string(loaded.Manifest.FailureClass))
		h.loaded = append(h.loaded, loaded)
		log.Info("add-on loaded",
			slog.String("addon", loaded.Manifest.Name),
			slog.String("version", loaded.Manifest.Version),
			slog.Int("abi_version", loaded.Manifest.ABIVersion),
			slog.String("failure_class", string(loaded.Manifest.FailureClass)),
			slog.Int("declared_permissions", len(loaded.Manifest.Permissions)),
			slog.Int("declared_settings", len(loaded.Manifest.Settings)),
			slog.Uint64("guest_memory_bytes", uint64(loaded.MemorySize())))
	}

	log.Info("add-on host started",
		slog.String("dir", dir),
		slog.Int("loaded", len(h.loaded)))
	return h, nil
}

// fatal reports whether a failed load stops the instance.
//
// A `required` add-on does, which is what the class means. So does a failure that
// happened before any class could be **read** — an empty class here means the
// manifest itself is what failed. That is the deliberately harsh limb: the
// alternative is to assume `degrade` for a manifest nobody could parse, which on
// an authentication add-on means an instance that boots with sign-in silently
// missing. The operator who just changed this directory gets an error naming the
// add-on instead.
//
// Keyed on the class rather than on the outcome, deliberately. A manifest that
// parses and then disagrees with its own directory name is also
// OutcomeManifestInvalid, and its class *is* readable — so it is honoured, because
// the whole rule is that the add-on decides.
func fatal(e *LoadError) bool {
	return e.class == "" || e.class == ClassRequired
}

// loadOne is the whole lifecycle for one add-on, in the order that makes each
// step's failure the cheapest one available: the manifest before the module, the
// digest before the runtime.
func loadOne(ctx context.Context, h *Host, dir, entry string) (Loaded, error) {
	fail := func(o Outcome, class FailureClass, err error) (Loaded, error) {
		return Loaded{}, &LoadError{Addon: entry, Outcome: o, Err: err, class: class}
	}

	m, err := ReadManifest(dir)
	if err != nil {
		return fail(OutcomeManifestInvalid, "", err)
	}
	// The directory *is* the add-on's identity. Requiring the two to agree buys
	// two things: a metric label that exists before the manifest parses, and the
	// impossibility of two directories claiming one name — which would be two
	// add-ons contending for one Postgres schema at M63.
	if m.Name != entry {
		return fail(OutcomeManifestInvalid, m.FailureClass,
			fmt.Errorf("manifest names %q but the directory is %q; they must match", m.Name, entry))
	}
	// Before the module is read, let alone hashed or compiled. An add-on built
	// against an ABI this host does not serve is refused for a reason that is
	// knowable from 200 bytes of JSON, and doing the cheap check first is the same
	// ordering rule the rest of this function follows.
	if err := abi.CheckGeneration(m.ABIVersion); err != nil {
		return fail(OutcomeABIUnsupported, m.FailureClass, err)
	}

	// filepath.Join with a validated bare filename, so this cannot leave dir.
	// Validate refuses a separator, a dot entry, and anything not ending .wasm.
	wasmPath := filepath.Join(dir, m.Module)
	code, err := os.ReadFile(wasmPath) //nolint:gosec // G304: an operator-owned directory is the feature; the filename is validated to be bare
	if err != nil {
		return fail(OutcomeModuleUnreadable, m.FailureClass, err)
	}

	// Verified before the runtime is asked for anything. A module whose bytes are
	// not the bytes the manifest describes is not compiled, not validated and not
	// instantiated — the check is worth nothing if it happens after the wasm has
	// been parsed.
	sum := sha256.Sum256(code)
	if got := hex.EncodeToString(sum[:]); got != m.SHA256 {
		return fail(OutcomeChecksumMismatch, m.FailureClass,
			fmt.Errorf("%s hashes to %s, manifest says %s", m.Module, got, m.SHA256))
	}

	compiled, err := h.runtime.CompileModule(ctx, code)
	if err != nil {
		return fail(OutcomeInstantiateFailed, m.FailureClass, fmt.Errorf("compile: %w", err))
	}
	// Registered before instantiation, and this is not tidiness. Package
	// initialization runs *during* InstantiateModule — that is what makes a
	// load-time failure expressible at all — so a module whose init calls a host
	// function does so before this call returns. State registered afterwards would
	// mean every add-on's first ABI call answered StatusInternal.
	deregister := h.registerState(m)
	// The compiled form is owned by the runtime and closed with it. Closing it
	// here would invalidate the instance below.
	mod, err := h.runtime.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().
			WithName(m.Name).
			WithStartFunctions(StartFunction))
	if err != nil {
		// wazero returns a non-nil module alongside the error when the start
		// function traps, and leaving it open would leak the guest's memory for
		// the life of the process.
		if mod != nil {
			_ = mod.Close(ctx)
		}
		// The add-on is not loaded, so nothing may answer a call in its name. A
		// `degrade` failure leaves the instance serving, and a stale entry here
		// would be state for a module that is gone.
		deregister()
		return fail(OutcomeInstantiateFailed, m.FailureClass, err)
	}

	return Loaded{Manifest: m, Dir: dir, module: mod}, nil
}

// Addons is what loaded, in discovery order. The slice is a copy; the instances
// inside it are not.
func (h *Host) Addons() []Loaded {
	if h == nil {
		return nil
	}
	out := make([]Loaded, len(h.loaded))
	copy(out, h.loaded)
	return out
}

// Len is how many add-ons are instantiated.
func (h *Host) Len() int {
	if h == nil {
		return 0
	}
	return len(h.loaded)
}

// Close shuts the runtime down, which closes every module in it.
func (h *Host) Close(ctx context.Context) error {
	if h == nil || h.runtime == nil {
		return nil
	}
	err := h.runtime.Close(ctx)
	h.runtime = nil
	h.loaded = nil
	h.mu.Lock()
	h.states = nil
	h.mu.Unlock()
	return err
}
