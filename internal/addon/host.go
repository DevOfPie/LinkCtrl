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
// internal/addon/abi and registered by hostabi.go. Five of them do something —
// abi_version, log, config_get, storage_query and storage_exec — and the rest are
// declared and refused with a status a module can branch on, because the contract
// crosses a repository boundary before the behaviour behind it exists. Routes,
// templates, the session hook and redirect observation are M64 to M66's.
//
// # An add-on's own tables
//
// A module that declares `storage.own_schema` gets a Postgres schema of its own,
// `addon_<name>`, and a login role of the same name that reaches nothing else. Its
// migrations arrive with it — a `migrations/` directory, each file named in the
// manifest with its digest — and the *host* applies them, at load, before the
// listener opens, as the add-on's own role. That last clause is the confinement:
// DDL naming another schema is refused by Postgres rather than by a parser here,
// and a SECURITY DEFINER function the DDL creates is owned by a role that can
// reach nothing. internal/store/addons.go is the whole of it, and its header says
// why `SET ROLE` on the application's own session is not a boundary.
//
// What a module may *call* is narrower still: every function names the permission
// it costs, the manifest is where an add-on declares the ones it needs, and an
// undeclared call is refused and counted (M62). Grants are resolved once here at
// load — grants.go — and checked on every call in hostabi.go, because from M66 the
// check sits on the redirect path.
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/store"
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
// Eight values, so the series count is the number of installed add-ons times eight
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
	// OutcomeStorageFailed is the add-on's schema, role or migrations (M63). Its
	// own label rather than instantiate_failed, because the module is fine and the
	// operator's fix is a database one: a privilege the application's user does not
	// hold, a migration the add-on's author wrote wrongly, or DDL that reached
	// outside the schema the add-on owns. Which of those it was is in the log; the
	// label is what tells an operator where to look.
	OutcomeStorageFailed Outcome = "storage_failed"
	// OutcomeNameCollision is two installed add-ons whose names stand in a
	// `name + "_"` prefix relation — see nameCollisions, which is where the whole
	// rule is. Its own label rather than manifest_invalid for the reason
	// abi_unsupported is: neither manifest is invalid, each is a perfectly good
	// manifest on its own, and the operator's fix is a directory name rather than
	// anything inside a file.
	OutcomeNameCollision Outcome = "name_collision"
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

	// DB is the application's own pool, used for the two things an add-on's
	// storage needs the *product's* privileges for: creating the schema and the
	// role an add-on is confined to, and reading the catalogue to enumerate and
	// measure them. No add-on's statement ever runs on it — that is what the
	// per-add-on pool in store.AddonDB is for, and the separation is the boundary
	// rather than a tidiness.
	DB *pgxpool.Pool
	// DSN is the same database, as a connection string, because an add-on's own
	// pool authenticates as a different role and a pool cannot be re-pointed at
	// one. Everything else about the connection — host, port, database, TLS — is
	// inherited from it, so there is no second connection string to configure.
	//
	// Empty means no storage: the schema is not created, the migrations are not
	// applied, and a call to a storage function answers StatusInternal after
	// saying so in the log. That is a host constructed without a database, which
	// in this product is a test and never an instance — cmd/linkctrl opens the
	// pools before it opens the host, and both of these come from the same place
	// the migrations did.
	DSN string

	// Settings is where an add-on's configured values come from, asked for the
	// settings its manifest declares. Nil means the environment, through
	// [config.AddonSettings], which is what an instance uses; a test substitutes
	// values without writing to the process environment.
	Settings func(addon string, declared []string) map[string]config.Secret
}

// Loaded is one add-on that instantiated.
type Loaded struct {
	Manifest Manifest
	// Dir is the absolute directory the add-on was loaded from.
	Dir string
	// Schema is the Postgres schema this add-on owns, or "" for one that did not
	// declare storage.own_schema. It is derived from the name rather than recorded
	// anywhere — store.AddonSchema is the derivation — which is what makes two
	// add-ons contending for one schema impossible rather than merely unlikely: the
	// directory *is* the name and loadOne refuses a manifest that disagrees.
	Schema string

	module  api.Module
	grants  Grants
	storage *store.AddonDB
	// settings is what an operator configured for this add-on, by declared
	// setting name — see config.AddonSettings. Held as Secret whatever the
	// manifest called the setting, so no value can print itself.
	settings map[string]config.Secret
	// compiled is the module's compiled form, kept because a route gets an
	// instance of its own per request (M64, D260) and compiling per request was
	// ruled out by M60's measurement. It is owned by the runtime and closed with
	// it, which is why nothing here closes it.
	compiled wazero.CompiledModule
}

// Grants is what this add-on holds, which is not the same as what its manifest
// declared: a permission the vocabulary carries and no host grants yet is
// declarable and not held. This is the readable form m62.md asks for — the boot
// log and linkctrl_addon_info carry the same set, and the Add-on manager reads it
// from here.
func (l Loaded) Grants() Grants { return l.grants }

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
	// metrics is held rather than passed, because the refusal counter is written
	// from an ABI call and not from Open. Nil-safe on every method, which is what
	// lets a test open a host without a registry.
	metrics *observability.Metrics

	// db and dsn are Options.DB and Options.DSN, held because a schema is created
	// per add-on inside the discovery loop and measured again long after it, from
	// the maintenance job.
	db  *pgxpool.Pool
	dsn string
	// settings is Options.Settings with its default applied.
	settings func(addon string, declared []string) map[string]config.Secret

	// states is what an ABI call is answered against, keyed by the calling
	// module's name — see hostabi.go. Guarded because a host function is called
	// from whatever goroutine the guest is running on: at boot that is Open's own,
	// and since M64 it is a request's.
	//
	// A per-request instance is registered here too, under a name no manifest
	// could carry, and removed when the request ends. That is what scopes a
	// request to the instance answering it: dispatch resolves state by the
	// *calling* module's name, so an add-on's own request record cannot be read
	// by the add-on's load-time instance or by another request's.
	mu     sync.RWMutex
	states map[string]*hostState

	// slots bounds how many add-on requests hold an instance at once. See
	// maxConcurrentRoutes.
	slots chan struct{}
	// instances numbers per-request module names. Monotonic rather than random:
	// a name that appears in a log is one an operator can order against another.
	instances atomic.Uint64
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

	// Every name that is claimed, read before anything is loaded, because whether
	// one add-on may load is a question about the *other* names installed beside
	// it — the one check in this file that cannot be made from one directory.
	// See nameCollisions.
	claimed := claimants(dir, entries)
	collisions := nameCollisions(claimed)
	classes := make(map[string]FailureClass, len(claimed))
	for _, c := range claimed {
		classes[c.name] = c.class
	}

	settings := opts.Settings
	if settings == nil {
		settings = config.AddonSettings
	}
	h := &Host{
		log: log, metrics: opts.Metrics, db: opts.DB, dsn: opts.DSN,
		settings: settings,
		slots:    make(chan struct{}, maxConcurrentRoutes),
	}
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
		var (
			loaded Loaded
			err    error
		)
		if others := collisions[name]; len(others) > 0 {
			// Refused before it is loaded, rather than unwound after: the cheapest
			// place to stop is the one before a schema exists and a module's package
			// initialization has run.
			err = &LoadError{
				Addon: name, Outcome: OutcomeNameCollision, class: classes[name],
				Err: fmt.Errorf("%q cannot load beside %s: one name plus an underscore "+
					"is a prefix of the other, so the cookie prefixes and the %s "+
					"variables derived from the two names overlap. Rename one directory "+
					"and the add-on's manifest with it; neither loads until then",
					name, quoteAll(others), config.AddonEnvPrefix),
			}
		} else {
			loaded, err = loadOne(ctx, h, filepath.Join(dir, name), name)
		}
		if err != nil {
			var le *LoadError
			if !errors.As(err, &le) {
				// loadOne returns nothing else; belt, so a future edit that
				// forgets cannot lose the metric label.
				le = &LoadError{Addon: name, Outcome: OutcomeManifestInvalid, Err: err}
			}
			// labelFor, not name: this is the one label in the file taken from a
			// directory entry rather than from a validated manifest.
			h.metrics.ObserveAddonLoad(labelFor(name), string(le.Outcome))
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
		h.metrics.ObserveAddonLoad(loaded.Manifest.Name, string(OutcomeLoaded))
		h.metrics.SetAddonInfo(loaded.Manifest.Name, loaded.Manifest.Version,
			loaded.Manifest.ABIVersion, string(loaded.Manifest.FailureClass),
			loaded.grants.String())
		h.loaded = append(h.loaded, loaded)
		// The grants are named rather than counted. A count answers "did anything
		// change" and nothing else, and what an operator reading a boot log needs to
		// know about a module they installed is which capabilities it asked for —
		// m62.md's *grants are visible*, of which this and the info gauge are the
		// minimum and M68's manager is the proper surface.
		log.Info("add-on loaded",
			slog.String("addon", loaded.Manifest.Name),
			slog.String("version", loaded.Manifest.Version),
			slog.Int("abi_version", loaded.Manifest.ABIVersion),
			slog.String("failure_class", string(loaded.Manifest.FailureClass)),
			slog.Any("permissions", loaded.grants.Names()),
			slog.Int("declared_settings", len(loaded.Manifest.Settings)),
			// How many of them an operator has actually set, which is the question a
			// boot log can answer and `config_get` returning a default cannot. The
			// count and never a name-value pair: a setting may be a secret, and the
			// values are held as config.Secret exactly so that a line like this one
			// cannot become the leak.
			slog.Int("configured_settings", len(loaded.settings)),
			slog.String("schema", loaded.Schema),
			slog.Int("migrations", len(loaded.Manifest.Migrations)),
			slog.Uint64("guest_memory_bytes", uint64(loaded.MemorySize())))
	}

	// Said at boot rather than only when somebody opens the manager, because an
	// orphan is data an operator is paying for and does not know about — a module
	// whose file was deleted takes its page row with it, and the schema stays. M68
	// is the surface that offers to purge one; this is the line that stops the
	// question from waiting for it.
	if orphans, err := h.OrphanSchemas(ctx); err != nil {
		log.Debug("could not look for orphaned add-on schemas", slog.Any("error", err))
	} else if len(orphans) > 0 {
		log.Warn("add-on schemas with no loaded module; their data is still on disk and "+
			"nothing here deletes it",
			slog.Any("schemas", orphans))
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

// claimant is a directory that has established an identity: its manifest parses
// and names the directory it sits in.
type claimant struct {
	name  string
	class FailureClass
}

// claimants reads the manifests in dir for the one thing the collision check
// needs before anything loads — which names are claimed, and under whose failure
// class.
//
// Best-effort by design. An entry whose manifest will not parse, or which names
// some other directory, has claimed nothing: it fails on its own terms in the
// load loop, and it must not take a neighbour whose install is intact down with
// it. That is the difference between reading manifests here and reading the
// directory entries — a mis-installed `oidc_x` would otherwise refuse `oidc` as
// well, and the operator would be told about a name collision when what they have
// is one typo.
//
// Everything past the name is deliberately *not* checked here. A module whose
// bytes are wrong or whose ABI generation this host will not serve has still
// declared the name and the prefixes under it, and its operator's fix is the same
// rename either way — so it collides, and the cost is that a broken install
// refuses its neighbour rather than only itself.
//
// The manifest is read twice per add-on, here and in loadOne. Boot only, a few
// hundred bytes of JSON each, and the alternative is threading a parsed manifest
// through the load path to save it.
func claimants(dir string, entries []os.DirEntry) []claimant {
	out := make([]claimant, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := ReadManifest(filepath.Join(dir, e.Name()))
		if err != nil || m.Name != e.Name() {
			continue
		}
		out = append(out, claimant{name: m.Name, class: m.FailureClass})
	}
	return out
}

// nameCollisions reports every claimed name standing in a `name + "_"` prefix
// relation with another, mapped to the names it collides with.
//
// That relation is the whole of the ambiguity, in both places a name becomes a
// namespace by concatenation, and the measurement is in host_test.go rather than
// in this comment:
//
//   - **Cookies.** A declared prefix must begin with the add-on's own name and an
//     underscore (D234), so add-on `oidc` may declare `oidc_x` — which is a
//     prefix of every prefix add-on `oidc_x` is allowed to declare. Inbound
//     filtering and outbound authorisation are both a prefix test, so `oidc`
//     reads and overwrites `oidc_x`'s cookies, session state included.
//   - **Settings.** LINKCTRL_ADDON_OIDC_X_KEY is `x_key` of `oidc` and `key` of
//     `oidc_x` at once (D263), and no lookup can tell which operator meant which.
//
// It is exactly the relation and not merely one case of it. Both namespaces are
// `name + "_" + anything`, so two distinct names produce one string only when the
// shorter plus an underscore is a prefix of the longer: the concatenations agree
// character for character, and the character after the shorter name is the
// underscore. Names are lowercase (nameRe), so upper-casing for the environment
// introduces no collisions of its own.
//
// The route prefix and the Postgres schema are unaffected. Each uses the whole
// name as one path or identifier segment, with nothing concatenated after it, so
// `/addons/oidc/` and `/addons/oidc_x/` are as distinct as their names are.
//
// **Both members of a pair are refused**, never the shorter or the longer. There
// is no principled winner, and awarding the namespace to whichever name sorts
// first would be the first-come rule D234 rejected, one milestone later with the
// order decided by spelling instead of by install order — `oidc` sorts before
// `oidc_x` precisely because it is the prefix. The cost is that either add-on can
// deny the other by being installed; that cost exists under any refusal rule,
// while handing one of them the other's cookies does not.
func nameCollisions(cs []claimant) map[string][]string {
	out := make(map[string][]string)
	for i, a := range cs {
		for _, b := range cs[i+1:] {
			if strings.HasPrefix(b.name, a.name+"_") || strings.HasPrefix(a.name, b.name+"_") {
				out[a.name] = append(out[a.name], b.name)
				out[b.name] = append(out[b.name], a.name)
			}
		}
	}
	return out
}

// quoteAll is the collision message's list of other names, quoted so a name with
// an underscore in it reads as one name.
func quoteAll(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return strings.Join(quoted, ", ")
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

	// Resolved once, here, and never again: from M66 a grant check sits on the
	// redirect path, so what the check reads has to already exist. See grants.go.
	grants, withheld := resolveGrants(m)
	if len(withheld) > 0 {
		// Said before the module runs, because package initialization runs during
		// instantiation and a call refused for this reason happens next. A warning
		// rather than a refusal: the class is declarable on purpose, and the
		// milestone that admits an add-on onto the redirect path is what turns it on.
		h.log.Warn("an add-on declared a permission no host grants yet; it does not "+
			"hold it and every call behind it is refused",
			slog.String("addon", m.Name),
			slog.Any("permissions", withheld))
	}
	// The schema, the role, the migrations and the confined pool — all of it before
	// instantiation, and for the same reason the state registration below is: package
	// initialization runs *during* InstantiateModule, so a module whose init writes a
	// row has to find its tables already there. It is also the ordering the failure
	// classes need: a `required` add-on whose migration will not apply must stop the
	// instance before anything is listening, which is what M60's class rule means and
	// what m63.md's third risk names as the accepted cost.
	storage, err := h.openStorage(ctx, m, dir, grants)
	if err != nil {
		return fail(OutcomeStorageFailed, m.FailureClass, err)
	}

	// What an operator configured, resolved once at load for the reason the grants
	// are: config_get is a map lookup on a request's path and must not read the
	// environment per call. Declared settings only — an environment variable for a
	// setting no manifest declares is not read, which is the same scoping
	// config_get itself applies (D263).
	values := h.settings(m.Name, settingNames(m))

	// Registered before instantiation, and this is not tidiness. Package
	// initialization runs *during* InstantiateModule — that is what makes a
	// load-time failure expressible at all — so a module whose init calls a host
	// function does so before this call returns. State registered afterwards would
	// mean every add-on's first ABI call answered StatusInternal.
	deregister := h.registerState(m, grants, storage, values)
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
		// The schema and its data stay — that is the point of an orphan being
		// detectable rather than cleaned up behind an operator's back — but the
		// connections do not. A `degrade` failure leaves the instance serving for
		// however long it runs, and four idle connections per module that would not
		// start is a pool leak with a schedule.
		storage.Close()
		return fail(OutcomeInstantiateFailed, m.FailureClass, err)
	}

	schema := ""
	if storage != nil {
		schema = storage.Schema()
	}
	return Loaded{
		Manifest: m, Dir: dir, Schema: schema, settings: values,
		module: mod, grants: grants, storage: storage, compiled: compiled,
	}, nil
}

// openStorage gives one add-on the schema it owns, applies the DDL it shipped, and
// opens the confined connection its queries run on.
//
// Nil is the ordinary answer for an add-on that did not declare
// storage.own_schema: no schema is created, no role exists, and every storage call
// is already refused one layer up by the permission check. Creating a schema for a
// module that did not ask for one would be the host granting a capability nobody
// requested, which is the thing the permission model exists to prevent.
func (h *Host) openStorage(ctx context.Context, m Manifest, dir string, grants Grants) (*store.AddonDB, error) {
	if !grants.Has(abi.PermissionStorage) {
		// Migrations without the grant are refused by Manifest.Validate, so there is
		// no case here where DDL exists and is quietly being skipped.
		return nil, nil
	}
	if h.db == nil || h.dsn == "" {
		// A host constructed without a database, which in this product is a test and
		// never an instance: cmd/linkctrl opens the pools before it opens the host.
		// Loud rather than fatal, because the add-on itself is fine and the storage
		// functions answer StatusInternal — the status that means the host failed at
		// something that is not the guest's fault.
		h.log.Error("an add-on declared storage and this host has no database; every "+
			"storage call it makes will fail",
			slog.String("addon", m.Name),
			slog.String("permission", abi.PermissionStorage))
		return nil, nil
	}

	// The bytes are verified before a schema exists, so a manifest that disagrees
	// with its own migrations directory costs nothing in the database.
	source, err := readMigrations(dir, m)
	if err != nil {
		return nil, err
	}
	password, err := store.EnsureAddonSchema(ctx, h.db, m.Name, h.log)
	if err != nil {
		return nil, err
	}
	if source != nil {
		if err := store.MigrateAddon(ctx, h.dsn, m.Name, password, source, h.log); err != nil {
			return nil, err
		}
	}
	// The post-condition on somebody else's DDL. Privileges are what confine it,
	// and this asks the catalogue whether they did — once per load, in one query —
	// because *refuses DDL that names any other schema* is a claim about the
	// outcome, and the mechanism is the part a later change could weaken without
	// anybody noticing.
	//
	// **Outside the migrations branch, because DDL is not the only way out.** A
	// large object and a temp relation are both owned by the role that created them
	// and live outside its schema, and an add-on makes either from a *query* rather
	// than from a migration — so an add-on that shipped no `.sql` file at all can
	// still own something out there. It also asks the other direction, which no
	// migration causes and a restore does: a table inside the add-on's schema owned
	// by the application, because `pg_dump` carries no roles. An operator whose
	// add-on is refused has docs/operations.md's purge for the first and
	// docs/deployment.md's role restore for the second.
	violations, err := store.AddonConfinementViolations(ctx, h.db, m.Name)
	if err != nil {
		return nil, err
	}
	if len(violations) > 0 {
		return nil, fmt.Errorf("its confinement to %s does not hold: %v",
			store.AddonSchema(m.Name), violations)
	}
	return store.OpenAddonDB(ctx, h.db, h.dsn, m.Name, password, h.log)
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

// Schemas is the Postgres schema every loaded add-on owns, in discovery order.
//
// Only the add-ons that declared storage have one, so this is shorter than
// [Host.Addons] whenever a module asked for none. It is what
// [Host.OrphanSchemas] subtracts and what the maintenance job measures.
func (h *Host) Schemas() []string {
	if h == nil {
		return nil
	}
	var out []string
	for _, l := range h.loaded {
		if l.Schema != "" {
			out = append(out, l.Schema)
		}
	}
	return out
}

// OrphanSchemas is every `addon_*` schema in the database that no loaded add-on
// owns.
//
// m63.md's *an orphan is detectable*, and it is detectable because the schema
// name is derived from the add-on's name rather than recorded: removing a
// module's directory removes the only thing that would have claimed the schema,
// and what is left over is exactly the set this returns. Nothing here deletes
// anything — a purge is an operator's explicit act, and [M68]'s flow.
//
// Nil host, or a host with no database, answers nil and no error: an instance
// that configured no add-ons has no orphans to have, and saying so with an error
// would make every caller ask first.
func (h *Host) OrphanSchemas(ctx context.Context) ([]string, error) {
	if h == nil || h.db == nil {
		return nil, nil
	}
	all, err := store.AddonSchemas(ctx, h.db)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(h.loaded))
	for _, schema := range h.Schemas() {
		live[schema] = true
	}
	var out []string
	for _, schema := range all {
		if !live[schema] {
			out = append(out, schema)
		}
	}
	return out, nil
}

// ObserveSchemaSizes publishes how much disk each loaded add-on's schema holds.
//
// The metric is the whole of m63.md's answer to quotas: there is no cap on how
// large an add-on's schema may grow, for the reason there is no cap on the audit
// log, and the default is only defensible if the growth it permits is visible.
// Measured on a schedule from the maintenance job — see cmd/linkctrl/jobs.go, and
// the audit log's own gauge beside it for why every replica measures rather than
// only the leader.
//
// Catalogue arithmetic per add-on, so the cost is two cheap queries times the
// number of installed modules. A failure is logged at debug and skipped: a
// measurement that could not be taken is not an operational event.
//
// **Two, because a schema is not everything an add-on can fill.** A large object
// belongs to the role that created it and to no schema, so it is absent from the
// first measurement by construction; the second counts them, which is what keeps
// *stored growth is visible by metric* true for the kind of stored growth the
// schema's size cannot show. It is *the* kind only because the first measurement now
// sums every relation kind in the schema that has storage rather than a list of
// them: a sequence was a second kind, inside the schema and invisible to the list,
// which is D254. See store.AddonLargeObjects for why this one is a count and not a
// size — and for the qualifier: transient disk an add-on's session holds is neither
// gauge's subject, which is F279.
func (h *Host) ObserveSchemaSizes(ctx context.Context) {
	if h == nil || h.db == nil {
		return
	}
	for _, l := range h.loaded {
		if l.Schema == "" {
			continue
		}
		bytes, err := store.AddonSchemaBytes(ctx, h.db, l.Manifest.Name)
		if err != nil {
			h.log.Debug("could not measure an add-on's schema",
				slog.String("addon", l.Manifest.Name),
				slog.Any("error", err))
		} else {
			h.metrics.SetAddonSchemaBytes(l.Manifest.Name, bytes)
		}
		los, err := store.AddonLargeObjects(ctx, h.db, l.Manifest.Name)
		if err != nil {
			h.log.Debug("could not count an add-on's large objects",
				slog.String("addon", l.Manifest.Name),
				slog.Any("error", err))
			continue
		}
		h.metrics.SetAddonLargeObjects(l.Manifest.Name, los)
	}
}

// Close shuts the runtime down, which closes every module in it.
func (h *Host) Close(ctx context.Context) error {
	if h == nil || h.runtime == nil {
		return nil
	}
	// Before the runtime, so nothing can be executing a statement on a pool that is
	// closing underneath it. The schemas themselves are untouched: closing a host is
	// not an uninstall, and an add-on's data outliving the process is the whole
	// point of it being in Postgres.
	for _, l := range h.loaded {
		l.storage.Close()
	}
	err := h.runtime.Close(ctx)
	h.runtime = nil
	h.loaded = nil
	h.mu.Lock()
	h.states = nil
	h.mu.Unlock()
	return err
}

// settingNames is the settings a manifest declares, which is what
// config.AddonSettings is asked for.
func settingNames(m Manifest) []string {
	out := make([]string, 0, len(m.Settings))
	for _, s := range m.Settings {
		out = append(out, s.Name)
	}
	return out
}

// ConfiguredSettings is how many of this add-on's declared settings an operator
// has given a value. It is what M68's manager reads to draw "set" beside a
// secret it must never echo.
func (l Loaded) ConfiguredSettings() int { return len(l.settings) }
