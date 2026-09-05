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
// internal/addon/abi and registered by hostabi.go. Count them from abi.Functions
// rather than from this comment, which has been wrong once: the live set grows as
// milestones land, and the rest are declared and refused with a status a module can
// branch on, because the contract crosses a repository boundary before the
// behaviour behind it exists. Template rendering and redirect observation are the
// ones still refused.
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
//   - the clock and the random source are **not** wazero's defaults, and that is
//     the one place this host deliberately departs from them. See
//     [guestModuleConfig], which is the only place in this package a module
//     config is built.
//
// The departure is D292, and what it repairs is worth stating where the defaults
// are described. wazero's default random source is `rand.New(rand.NewSource(42))`
// — a compile-time constant, so every module on every deployment drew the same
// bytes — and its default clock starts at 2022-01-01 and advances a millisecond
// per reading. With a fresh instance per request (D260) that made *every
// visitor's* nonce identical rather than merely predictable, which is F292. A
// module's writes to stdout and stderr are still discarded — routing them into the
// operator's log would be a capability granted by accident, and the log function is
// the one that was granted on purpose.
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
//
// Those are measurements of one fixture, and a measurement is not a bound. What
// bounds an instance is WithMemoryLimitPages below, added when M64 was reopened:
// before it, a module that asked for more simply got more, and the concurrency
// bound priced sixteen instances of whatever the module chose (F290). It binds
// however the module's memory section is written — an over-large *minimum* is
// refused at load, an over-large *maximum* is replaced by this limit while the
// section is decoded, and TestWhatAMemorySectionMayDeclare measures both.
package addon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
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

// guestModuleConfig is the configuration **every** guest instance in this product
// is made with, and it is the only place in this package a module config is built.
//
// One function rather than a literal per call site, because there are two sites —
// the load-time instance in loadOne and the per-request instance in http.go — and
// a difference between them is invisible: a module drawing entropy would work at
// load and be predictable per request, or the reverse, with nothing failing.
// TestOnlyOneModuleConfigIsBuilt asserts that this stays the only site, so a third
// instantiation added by a later milestone cannot quietly get wazero's defaults.
//
// # The clock and the random source (D292)
//
// wazero's defaults for both are fakes, and they are fakes with a shape worth
// naming rather than merely replacing:
//
//   - the random source is `rand.New(rand.NewSource(42))`. Not "seeded at
//     startup" — a **compile-time constant**, so the stream is byte-identical
//     across requests, across add-ons, across host processes, across machines and
//     across deployments. With a fresh instance per request (D260) every visitor
//     therefore received the *same* nonce, which is the composition F292 filed;
//   - the wall clock starts at 2022-01-01 and advances one millisecond per
//     reading, so an `exp` claim checked against it is checked against 2022.
//
// Both are replaced here with the operating system's own, which is what makes
// `crypto/rand` and `time.Now` **inside a guest** mean what a publisher assumes
// they mean. That is deliberately the load-bearing half of D292: an add-on built
// against the older SDK, which cannot call `random_bytes` because it did not exist
// when the add-on was compiled, still gets real entropy from the standard library
// call it already made. The two ABI functions are the *documented* spelling of the
// same two sources, not a second pair of them.
//
// WithSysNanotime is set as well as WithSysWalltime. It is not what an expiry is
// compared against — that is the wall clock — but it is what Go's runtime uses for
// monotonic time inside `time.Since` and for scheduling, and leaving one real and
// the other fake is the shape that produces a duration measured between two clocks.
func guestModuleConfig(name string) wazero.ModuleConfig {
	return wazero.NewModuleConfig().
		WithName(name).
		WithStartFunctions(StartFunction).
		// crypto/rand.Reader, which is the operating system's own getrandom(2) on
		// Linux. Handed to WASI's random_get, which is what a guest's crypto/rand
		// and the runtime's own seeding both read.
		WithRandSource(rand.Reader).
		WithSysWalltime().
		WithSysNanotime()
}

// Outcome is the closed vocabulary of load results, and it is a metric label.
//
// Nine values, so the series count is the number of installed add-ons times nine
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
	// OutcomeLoadTimeout is an add-on that did not finish loading inside
	// [DefaultLoadTimeout] — see the deadline there for what that bounds and why.
	//
	// Its own label rather than instantiate_failed for the reason abi_unsupported
	// and name_collision are theirs: nothing is malformed, and the operator's
	// question is a different one. A module that traps at instantiation is broken
	// and the log carries the trap; a module that never returns is *running*, and
	// the only fact anyone has is that the budget ran out. Folding the two together
	// would have made the one alert an operator needs — an add-on is spending boot
	// — indistinguishable from the ordinary case of a bad build.
	OutcomeLoadTimeout Outcome = "load_timeout"
)

// DefaultLoadTimeout bounds how long an add-on's **own code** may run in one step
// of its load.
//
// **It bounds guest execution, and deliberately not the whole load.** Two steps of
// loadOne run code the add-on supplied — compiling the module, and instantiating
// it, which is where package initialization runs and where F287's hang was — and
// each of the two is given this budget. Nothing else in the load is inside it.
// The compile half is bounded only because [compileWorkers] is set: wazero's
// default compilation path does not stop for a context that is done, so the
// wrapper alone would have been a deadline nobody enforced. That is measured
// there rather than asserted here.
//
// That distinction is the whole of the choice, and it is a choice about what the
// number *means* rather than about what the host does when it expires. A budget
// laid over the whole of loadOne is simpler to write and is wrong, because the
// load's expensive step is not the add-on's at all: applying an add-on's
// migrations waits up to **five minutes** for the migration lock — store's
// MigrateAddon, which says "the same five minutes the host's own migrations wait.
// A replica arriving mid-migration should wait rather than fail into a crash
// loop", and store's own Migrate is the twin of it. Thirty seconds over the whole
// load caps that wait at thirty seconds, so a second replica arriving
// mid-migration fails as load_timeout and a `required` add-on then stops the
// instance — the crash loop M63 chose five minutes to prevent, produced by the fix
// for F287. A first `CREATE INDEX` on a real table at upgrade meets the same bound
// with nothing wrong anywhere. Bounding the guest instead leaves that wait
// reachable and still catches the module that never returns, which is the only
// thing F287 ever asked for.
//
// What it therefore does **not** bound is named rather than left to be discovered:
// an add-on's migrations are code it supplied too, and nothing here stops one
// statement in one of them running forever. The bound for that is Postgres's, on
// the connection the migration runs on, and it belongs beside where that
// connection is opened rather than here — F274.
//
// **Per add-on, and not for the directory**, which is the other half of F287's fix
// and the part worth arguing. A single budget shared across the directory has
// three faults, and the second is disqualifying:
//
//   - Attribution. One expired context tells an operator that *something* took too
//     long. A deadline per add-on names the add-on in the log line and in the
//     `addon` label of the metric, which is exactly what F287 says was missing —
//     "nothing says which add-on boot is stuck on".
//   - **It converts a `degrade` failure into a `required` one.** A shared context,
//     once expired, is expired for every add-on after it in the directory. One
//     `degrade` add-on spinning in `init()` would then fail every add-on behind
//     it, and a `required` one among them stops the instance. That is the precise
//     bullet this reopening exists to repair, re-broken from the other side.
//   - Scaling. Ten installed add-ons would each get a tenth of the budget, so
//     installing an eleventh could refuse the other ten.
//
// The cost is stated rather than hidden, and docs/operations.md states it where an
// operator reads: N add-ons that all hang cost N times this before the listener
// opens, and an add-on that contrived to hang in both of its steps costs twice it.
// In practice one — compiling is a finite pass over a file of finite size, while
// instantiation runs a loop the add-on wrote. Each is still bounded, and each logs
// as its budget expires, so what an operator sees is progress with names on it
// rather than the silence F287 measured at twenty seconds and counting.
//
// **The number is the one this milestone's own test already called the boundary.**
// TestInstantiationCostIsMeasured has asserted since M60 shipped that loading one
// add-on inside `Open` past 30 s "is not a boot-time cost any more"; this makes
// that assertion the host's behaviour instead of only the test's opinion, and the
// test now measures against this constant so the two cannot drift apart. Measured
// on this machine, 2026-08-20, at the two workers this host sets: a 1.87 MB
// fixture compiles in 211 ms and instantiates in 1.6 ms, so the whole of one
// add-on's guest execution is 213 ms — about a hundred-and-fortieth of the
// budget. The 380 ms figure eleven lines below is the same fixture at one
// worker, which is what the host used to do and no longer does. What this catches is a module that never
// returns, never a slow machine and never a slow database.
//
// It is a constant and not a config field. An operator has no information with
// which to choose it: the number is about what the host will wait for, not about
// this deployment, and a knob here would be one more thing to get wrong in the
// direction of "unbounded". [Options.LoadTimeout] overrides it for tests, which
// need a budget they can afford to spend.
const DefaultLoadTimeout = 30 * time.Second

// compileWorkers is how many goroutines wazero compiles a module with, and it is
// set for a reason that has nothing to do with speed.
//
// **It is what makes [DefaultLoadTimeout] bound the compile step at all.** wazero
// checks the context while compiling only on its *multi-worker* path — wazevo's
// compileModule in v1.12.0, where the worker loop opens with `ctx.Err()` and the
// caller then reports `context.Cause`. The single-worker branch, which is the one
// taken by default, walks the code section with no check in it anywhere, so a
// compile handed a context that is already done runs to completion and returns a
// nil error. Measured on the `minimal` fixture, 2026-08-20: with an expired
// context and the default, CompileModule succeeded after 377 ms; with two workers
// it returned `context deadline exceeded` after 23 ms. Wrapping the call in
// [Host.runGuest] without this was a deadline nobody enforced — expiry is read
// only when the step returns an error, so the load simply finished late.
//
// **Two**, because two is the whole of the requirement: experimental's
// GetCompilationWorkers returns max(workers, 1), so anything below two lands back
// on the branch with no check — which a NumCPU-shaped number would do on a
// one-core container, the machine most likely to need the bound. Compiling is
// also faster with more (383 ms, 208 ms and 126 ms at one, two and four here) and
// that is a side effect rather than the reason; the price of more is a machine, an
// SSA builder and a backend compiler per worker for the length of one boot.
//
// What this does **not** buy is a bound finer than one function. The check sits
// between functions of the code section, so a single function whose compilation is
// pathologically slow overshoots by however long that function takes.
// TestTheCompileStepIsBounded is written against what is enforced rather than
// against the rounder claim.
//
// It is `experimental`, and that is the hazard worth naming: nothing stops wazero
// moving the check, and the failure would be silent — a budget that quietly stops
// being enforced while every test that does not measure it stays green. That test
// is what fails at such an upgrade.
const compileWorkers = 2

// runGuest runs one step of an add-on's own code under [Host.loadTimeout].
//
// It reports expiry separately from the step's error, because a runtime handed a
// context that is done reports what it was doing rather than why it stopped: a
// module closed mid-instantiation returns "module closed with context deadline
// exceeded" today and something else on the next wazero, and neither is a thing to
// match on.
//
// Expiry is counted only when the **parent** is still live. A shutdown signal
// during boot cancels every load in flight, and that is an instance being stopped
// rather than an add-on being slow — the difference between an operator's own
// SIGTERM and an add-on to go and fix.
//
// The context is cancelled the moment the step returns, and that is load-bearing
// in the other direction: WithCloseOnContextDone means a module is closed when the
// context it ran under is done, so a per-step context left alive would close a
// healthy add-on when its budget expired, thirty seconds into an otherwise good
// boot. wazero's watcher is `defer done()` inside the call, so a step that
// finished is not closed by the cancel that tidies up after it —
// TestALoadedModuleOutlivesItsOwnBudget is what makes that a property of this
// repository rather than of a comment.
func (h *Host) runGuest(ctx context.Context, step func(context.Context) error) (expired bool, err error) {
	stepCtx, cancel := context.WithTimeout(ctx, h.loadTimeout)
	defer cancel()
	err = step(stepCtx)
	return errors.Is(stepCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil, err
}

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

// Error neutralizes the directory name and nothing else: Err is neutralized where
// it is constructed, and this escaping doubles a backslash, so applying it twice
// would double it twice. The composition with %q is deliberate and is the one place
// a reader sees it — a directory name carrying a backslash reads as four, which is
// odd and is the safe direction to be odd in.
//
// **Unbounded, and that is the fix F-5 named** (D286). This is what cmd/linkctrl
// prints when a `required` add-on refuses to load, and Manifest.Validate aggregates
// every problem with the manifest into one error precisely so that the person
// publishing an add-on for the first time sees the whole list. A 4 KiB cap belongs
// to a log line and was imported onto this path with the escaping; the two are
// separate concerns now, and only a log record gets the cap.
func (e *LoadError) Error() string {
	return fmt.Sprintf("add-on %q: %s: %v", moduleText(e.Addon), e.Outcome, e.Err)
}

func (e *LoadError) Unwrap() error { return e.Err }

// neutralized marks the sentence above as already escaped, so that logging a
// LoadError folds and bounds it rather than escaping it again. See logsafe.go.
func (*LoadError) neutralized() {}

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

	// Overrides is where an operator's per-add-on answers come from — the two
	// names in [config.AddonOverrideNames], which are not settings and which no
	// manifest may declare. Nil means the environment, through
	// [config.AddonOverrides]; a test substitutes them without writing to the
	// process environment, which is what lets the tests that exercise them run in
	// parallel.
	Overrides func(addon string) map[string]string

	// Sessions is what answers `session_mint` (M65). Nil is a host that cannot
	// mint — every unit test in this package, and no instance — and such a host
	// answers StatusInternal rather than pretending the function is not
	// implemented, because *this host has no database* and *this ABI does not have
	// that function* are different facts and a module branches on them differently.
	Sessions SessionMinter

	// Audit is what records the two lifecycle acts (M67). Nil records nothing,
	// which is a host built by a test; an instance passes the audit service, and
	// installing code into a running server without a record of who did it is the
	// one thing this surface must not be able to do quietly.
	Audit audit.Recorder

	// LoadTimeout is how long one add-on may take to load. Zero means
	// [DefaultLoadTimeout], which is what an instance uses; a test sets a budget it
	// can afford to spend watching a module that will not return.
	LoadTimeout time.Duration

	// InlineDeadline is how long an add-on's own code may hold a redirect open
	// (M66). Zero means [DefaultInlineDeadline], and an instance sets it from
	// LINKCTRL_ADDON_INLINE_DEADLINE. Unlike LoadTimeout it is an operator's knob
	// and not only a test's, because what it bounds is somebody else's code on the
	// path this product makes a latency promise about — see redirect.go.
	InlineDeadline time.Duration

	// InstantiateDeadline is how long this host will spend starting a module for a
	// redirect-class invocation (M66, reopened). Zero means
	// [DefaultInstantiateDeadline], and an instance sets it from
	// LINKCTRL_ADDON_INSTANTIATE_DEADLINE.
	//
	// **A test sets it for a reason no other bound here has.** It is the one number
	// in this package whose right value depends on the machine, so a suite that
	// leaves it at the default is a suite asserting that this machine is fast:
	// F326 shipped because five integration tests were green here and could not
	// pass on a hosted runner. Behaviour tests therefore buy room with it, and the
	// tests that are *about* the bound set a hostile one and make a slow machine
	// reachable on any machine at all.
	InstantiateDeadline time.Duration

	// PoolSize is how many idle add-on instances this host keeps across every
	// add-on (M66.5). Zero means [DefaultPoolSize], and an instance sets it from
	// LINKCTRL_ADDON_POOL_SIZE.
	//
	// It is not a concurrency bound. What bounds invocations in flight is
	// [addonSlots] and the pool takes nothing from it; this is what may be
	// held at rest, which is the term the guest-memory ceiling gained when an
	// instance stopped being destroyed after every redirect.
	PoolSize int

	// PoolTTL is how long an idle instance is kept before it is closed for lack of
	// traffic (M66.5). Zero means [DefaultPoolTTL], and an instance sets it from
	// LINKCTRL_ADDON_POOL_TTL.
	PoolTTL time.Duration

	// RouteDeadline is how long one request to an add-on's own route may take,
	// start to finish (M68.5). Zero means [DefaultRouteDeadline], and an instance
	// sets it from LINKCTRL_ADDON_ROUTE_DEADLINE.
	//
	// **The bound m68.5.md required before anything was allowed to fetch**, and it
	// applies to every route invocation rather than only to the ones that do — a
	// deadline conditional on a permission would leave the hole open for every
	// add-on that did not declare it while being a second rule to reason about.
	//
	// It is a bound **inside** the caller's, not the first one a route handler ever
	// had. An application request already carries LINKCTRL_HTTP_REQUEST_TIMEOUT's
	// context deadline and that cancels this same context, so the value only means
	// anything while it is shorter; internal/config refuses one that is not. What
	// the margin buys is a host that is still alive when the guest is killed, and
	// what the bound buys outright is an instance slot back from a module that will
	// not return — including on an instance that has turned the request timeout
	// off. See [DefaultRouteDeadline].
	RouteDeadline time.Duration

	// FetchTimeout bounds one outbound request an add-on makes (M68.5). Zero means
	// [DefaultFetchTimeout], and an instance sets it from
	// LINKCTRL_ADDON_FETCH_TIMEOUT.
	FetchTimeout time.Duration

	// FetchMaxBytes is the largest response body an add-on's fetch may bring back
	// (M68.5). Zero means [DefaultFetchMaxBytes], and an instance sets it from
	// LINKCTRL_ADDON_FETCH_MAX_BYTES.
	FetchMaxBytes int64
}

// FailureClassError is an operator override this host cannot interpret.
//
// Its own type because it is the one add-on failure that is neither the
// publisher's fault nor recoverable by degrading: the variable that says whether
// this add-on may be skipped is the variable that could not be read, so there is
// no answer to fall back to. Open returns it and the instance stops.
type FailureClassError struct {
	Addon string
	Var   string
	Value string
}

func (e *FailureClassError) Error() string {
	return fmt.Sprintf("add-on %q: %s=%q: must be %q or %q",
		moduleText(e.Addon), e.Var, moduleText(e.Value), ClassRequired, ClassDegrade)
}

// neutralized marks the sentence above as already escaped — moduleText is what
// does it, on both values that came from outside this build.
func (*FailureClassError) neutralized() {}

// requiredByDefault reports whether this manifest's declared permissions put the
// add-on on the authentication path.
//
// One permission does, and it is the highest-value grant in the vocabulary: an
// add-on holding `session.mint` decides who is signed in, so an instance that
// booted without it is an instance whose external sign-in silently does not exist.
// m65.md's answer, which is the owner's load-failure answer applied to the one
// milestone it was written for: **anything on the authentication path defaults to
// required**, whatever the manifest says.
//
// Read off the manifest's declarations rather than off resolved grants,
// deliberately. A permission that is declared and not held is still a statement
// about what the add-on is *for*, and an add-on refused the grant is exactly the
// case where an operator most needs the instance to say so.
func requiredByDefault(m Manifest) bool {
	return slices.Contains(m.Permissions, PermissionSessionMint)
}

// effectiveFailureClass is the class this host actually applies, which is not
// always the one the manifest declared.
//
// Three answers in a fixed order, and the order is the argument:
//
//  1. **The operator's override wins**, when there is one. It is the escape hatch
//     m65.md requires and the only way to run an authentication add-on as
//     `degrade`; a value that is neither class is an error rather than a fallback,
//     because falling back would be this host choosing the answer an operator was
//     trying to give.
//  2. **Otherwise `session.mint` forces `required`.** The manifest still declares
//     the class and this still overrides it, which is a deliberate exception to
//     *the add-on decides* (M60): the publisher of an authentication add-on cannot
//     know whether this instance has another way in, and the failure mode of
//     guessing wrong is an instance that boots with sign-in missing.
//  3. **Otherwise the manifest's own class**, unchanged.
func effectiveFailureClass(m Manifest, overrides map[string]string) (FailureClass, error) {
	if v, set := overrides["failure_class"]; set {
		switch FailureClass(v) {
		case ClassRequired, ClassDegrade:
			return FailureClass(v), nil
		default:
			return "", &FailureClassError{
				Addon: m.Name, Var: config.AddonSettingVar(m.Name, "failure_class"), Value: v,
			}
		}
	}
	if requiredByDefault(m) {
		return ClassRequired, nil
	}
	return m.FailureClass, nil
}

// mfaSatisfiedByProvider is the operator's answer about *this* add-on's provider.
//
// Anything other than the exact string "true" is false, and that is the point
// rather than lax parsing: this is the one flag in this product that can stop a
// second factor being asked for, so the safe reading is the default and saying
// otherwise has to be unambiguous. An unparseable value is not an error for the
// same reason it is not a yes — the instance keeps asking for the factor, which
// is what an operator who typed `yes` instead of `true` would want.
func mfaSatisfiedByProvider(overrides map[string]string) bool {
	return overrides["mfa_satisfied"] == "true"
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
	// FailureClass is what this host **applies**, which is the manifest's answer
	// only when neither of the two things that outrank it applied. Read this rather
	// than Manifest.FailureClass: the boot log, the info gauge and the decision to
	// stop the instance all use this one, and the manifest's field is the
	// publisher's declaration rather than the outcome.
	FailureClass FailureClass

	module  api.Module
	grants  Grants
	storage *store.AddonDB
	// settings is what an operator configured for this add-on, by declared
	// setting name — the stored answers from `addon_settings` with the
	// environment's on top (D347). Held as Secret whatever the manifest called
	// the setting, so no value can print itself, and behind one holder shared with
	// this add-on's hostState so a save from the Add-on manager reaches the
	// instances already built.
	settings *settingValues
	// live is how many invocations of this add-on are inside a guest call, and
	// whether it is being removed. A pointer, because a Loaded is copied by value
	// into every set and every caller of Addons and the count has to be one count.
	// Nil on a Loaded a test built by hand, which addonLive's methods treat as an
	// add-on nothing is removing — see set.go.
	live *addonLive
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

// overrides is the operator's answers about one add-on, nil-safe.
//
// Nil-safe because a *Host built by a test — which several in this package are,
// directly rather than through Open — has no function here, and registerState is
// reached from both. An absent lookup is an add-on with no overrides, which is
// what an operator who set no variables has.
func (h *Host) overrides(addon string) map[string]string {
	if h == nil || h.overrideFor == nil {
		return nil
	}
	return h.overrideFor(addon)
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
	// set is what this host is running, as one immutable value swapped under an
	// atomic pointer. It replaced three fields — loaded, inline and pools — that
	// Open wrote once and every reader took as constants, because M67 made the
	// installed set change while requests are in flight. set.go is the mechanism
	// and the argument, including why it is not a lock.
	set setPointer
	// installMu serializes the writers: Install, Remove and Close. No reader takes
	// it. It exists so that two installs cannot each derive a new set from the same
	// old one and lose each other's add-on.
	installMu sync.Mutex
	log       *slog.Logger
	// metrics is held rather than passed, because the refusal counter is written
	// from an ABI call and not from Open. Nil-safe on every method, which is what
	// lets a test open a host without a registry.
	metrics *observability.Metrics

	// dir is the absolute add-ons directory, held because runtime install and
	// removal write into it (M67) and because the directory Open read has to be
	// the one an install lands in — otherwise the operator's boot route and the
	// API's would be two lifecycles that disagree about what is installed.
	dir string
	// db and dsn are Options.DB and Options.DSN, held because a schema is created
	// per add-on inside the discovery loop and measured again long after it, from
	// the maintenance job.
	db  *pgxpool.Pool
	dsn string
	// settings is Options.Settings with its default applied, and overrides is
	// Options.Overrides with its default applied. Two functions rather than one,
	// because they read two disjoint sets of variable names and a manifest may
	// declare from only one of them — see config.AddonOverrideNames.
	settings    func(addon string, declared []string) map[string]config.Secret
	overrideFor func(addon string) map[string]string
	// sessions is Options.Sessions: what answers session_mint. Nil is a host that
	// cannot mint.
	sessions SessionMinter
	// auditor is Options.Audit: what records an install and a removal (M67). Nil
	// records nothing, which is every unit test in this package and no instance.
	auditor audit.Recorder

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
	// addonSlots.
	slots chan struct{}
	// instances numbers per-request module names. Monotonic rather than random:
	// a name that appears in a log is one an operator can order against another.
	instances atomic.Uint64

	// loadTimeout is Options.LoadTimeout with its default applied. Held rather
	// than passed because the two steps it bounds are inside loadOne, several
	// calls below Open, and threading a duration through them would say less
	// about where it applies than the two call sites do.
	loadTimeout time.Duration

	// The redirect limb (M66); redirect.go is where all of it is used. Which
	// add-ons are on it moved into `set` above at M67; what is left here is the
	// two bounds, which no install changes.
	//
	// inlineDeadline is Options.InlineDeadline with its default applied: how long
	// a module's own code may hold a redirect. Instantiation is not in it — that is
	// instantiateDeadline below, and F326 is what the two being one number cost.
	inlineDeadline time.Duration
	// instantiateDeadline is Options.InstantiateDeadline with its default applied:
	// how long this host will spend starting a module for a redirect-class
	// invocation before serving the redirect without it.
	instantiateDeadline time.Duration
	// The instance pool (M66.5); pool.go is where all of it is used. The map of
	// pools moved into `set` above at M67, for the reason the inline slice did:
	// an add-on installed at runtime brings a pool with it and one removed takes
	// its pool away, and the redirect path must never see a set where the two
	// disagree. Each addonPool still guards its own contents.
	//
	// idleInstances is how many entries are held at rest across every pool, which
	// is the term this milestone added to the guest-memory ceiling. Instance-wide
	// rather than per add-on because what an operator sizes a host by is the memory
	// this process holds, not how it is divided between modules.
	idleInstances atomic.Int64
	// poolSize is Options.PoolSize with its default applied, and poolTTL is
	// Options.PoolTTL with its default applied. Neither is [addonSlots]
	// and neither is derived from it — see pool.go.
	poolSize int
	poolTTL  time.Duration
	// The egress limb (M68.5); fetch.go is where all of it is used.
	//
	// fetcher is the outbound client, one per host: keep-alives are off, so sharing
	// it hands no add-on a socket another one opened, and building one per
	// invocation would build a TLS configuration per request for nothing.
	fetcher *fetcher
	// installFetcher is the same mechanism under M68.6's bounds: [MaxUploadBytes]
	// instead of the response cap an add-on gets, and [InstallFetchTimeout]
	// instead of the three seconds a discovery document is allowed. A second
	// *fetcher* and not a second fetch *path* — both drive [fetcher.get], so the
	// address policy and the redirect rule are one implementation, and only the
	// two numbers and the origin policy differ. Their files say why each number
	// is not the other.
	installFetcher *fetcher
	// routeDeadline is Options.RouteDeadline with its default applied: how long one
	// request to an add-on's own route may take. Not any of the redirect bounds and
	// not derived from one — a page is allowed to be slow in a way a redirect is
	// not, which is the whole reason it is a separate number.
	routeDeadline time.Duration

	// poolStop cancels the idle sweep's context; poolWG waits for it.
	poolStop context.CancelFunc
	poolWG   sync.WaitGroup

	// observe is the out-of-band queue, and it is nil unless something declared
	// the grant — which is what makes the observe class cost an ordinary instance
	// nothing at all: no channel, no goroutine.
	//
	// **Behind an atomic pointer since M67**, for the same reason the installed set
	// is: an add-on installed at runtime may be the first observer this host has,
	// so the channel is now written while [Host.Observe] is reading it from the
	// click pipeline. A lock there would be paid on every recorded click; one
	// atomic load is not, and the nil case — an instance where nothing observes —
	// costs exactly what it did.
	observe atomic.Pointer[observeQueue]
	// observeStop cancels the workers' context, which is what ends an invocation in
	// flight rather than waiting one out — see stopObserving.
	observeStop context.CancelFunc
	observeWG   sync.WaitGroup
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
	// Wrapped before anything else holds it, and this is the whole of how the
	// neutralization reaches a call site nobody has written yet: every logger this
	// subsystem uses is derived from this one, including the one openStorage hands
	// to internal/store, so a log line added there is neutralized without that
	// package importing anything. See logsafe.go, and D286.
	log := neutralizingLogger(opts.Logger)

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

	settings := opts.Settings
	if settings == nil {
		settings = config.AddonSettings
	}
	overrides := opts.Overrides
	if overrides == nil {
		overrides = config.AddonOverrides
	}

	// Every name that is claimed, read before anything is loaded, because whether
	// one add-on may load is a question about the *other* names installed beside
	// it — the one check in this file that cannot be made from one directory.
	// See nameCollisions.
	claimed := claimants(dir, entries, overrides)
	collisions := nameCollisions(claimed)
	classes := make(map[string]FailureClass, len(claimed))
	for _, c := range claimed {
		classes[c.name] = c.class
	}

	timeout := opts.LoadTimeout
	if timeout <= 0 {
		timeout = DefaultLoadTimeout
	}
	deadline := opts.InlineDeadline
	if deadline <= 0 {
		deadline = DefaultInlineDeadline
	}
	instantiate := opts.InstantiateDeadline
	if instantiate <= 0 {
		instantiate = DefaultInstantiateDeadline
	}
	h := &Host{
		log: log, metrics: opts.Metrics, dir: dir, db: opts.DB, dsn: opts.DSN,
		settings:    settings,
		overrideFor: overrides,
		sessions:    opts.Sessions,
		auditor:     opts.Audit,
		slots:       make(chan struct{}, addonSlots),

		loadTimeout:         timeout,
		inlineDeadline:      deadline,
		instantiateDeadline: instantiate,
		poolSize:            poolSizeFrom(opts.PoolSize),
		poolTTL:             poolTTLFrom(opts.PoolTTL),
		routeDeadline:       routeDeadlineFrom(opts.RouteDeadline),
	}
	h.fetcher = newFetcher(fetchTimeoutFrom(opts.FetchTimeout),
		fetchMaxBytesFrom(opts.FetchMaxBytes), log)
	// Not configurable, and deliberately: the two knobs an operator turns are the
	// ones a legitimate identity provider can plausibly need turned, and a bundle
	// fetch is bounded by the request it runs inside as well. See
	// [InstallFetchTimeout].
	h.installFetcher = newFetcher(InstallFetchTimeout, MaxUploadBytes, log)
	// The runtime is constructed only once there is a directory to read, so the
	// unset case above costs nothing. WithCloseOnContextDone is set at birth
	// because it is what lets M66 interrupt a module that will not return: a
	// deadline enforced by cancelling a context does nothing unless the runtime
	// is watching for it.
	//
	// WithMemoryLimitPages is set at birth for the same kind of reason and it is
	// F290's fix: wazero's default is 65536 pages, 4 GiB per instance, so without
	// this line the concurrency bound priced sixteen instances of an amount the
	// module chose. It is a property of the *runtime* rather than of a module
	// config, so it holds for every instance this host will ever make — the ones
	// load makes below, the per-request ones internal/addon/http.go makes, and any
	// a later milestone adds without knowing this line is here.
	h.runtime = wazero.NewRuntimeWithConfig(ctx,
		wazero.NewRuntimeConfig().
			WithCloseOnContextDone(true).
			WithMemoryLimitPages(maxGuestMemoryPages))
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

	// Accumulated locally and published once below, rather than appended to a
	// field. Open is a writer of the set like Install is, and the set is a value:
	// there is no half-built one to publish.
	var started []Loaded
	// discovered is every directory this loop treated as an add-on, appended
	// before the outcome is known — which is the whole point, and is D428's answer
	// to F281. `started` answers *what is running*; this answers *what is
	// installed*, and an orphan is a schema with no answer to the second question.
	// A degrade-class add-on that fails to instantiate appears here and not in
	// `started`, so its schema stops being offered for purge.
	var discovered []string
	for _, e := range entries {
		name := e.Name()
		if isStaging(name) {
			// The staging area runtime install writes through, and whatever a crash
			// mid-install left in it. Not an add-on, not a warning: it is this host's
			// own working directory inside the operator's, and lifecycle.go sweeps it
			// below. Skipped before the non-directory branch so a leftover file there
			// does not produce a line about an operator's README either.
			continue
		}
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
		// Recorded here, above every branch that can refuse this add-on: a name
		// collision, an invalid manifest and a module that will not instantiate all
		// leave the directory exactly where the operator put it. Anything appended
		// below this line would be recording an outcome rather than an installation.
		discovered = append(discovered, name)
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
				Err: neutralize(fmt.Errorf("%q cannot load beside %s: one name plus an underscore "+
					"is a prefix of the other, so the cookie prefixes and the %s "+
					"variables derived from the two names overlap. Rename one directory "+
					"and the add-on's manifest with it; neither loads until then",
					name, quoteAll(others), config.AddonEnvPrefix)),
			}
		} else {
			loaded, err = loadOne(ctx, h, filepath.Join(dir, name), name)
		}
		if err != nil {
			var le *LoadError
			if !errors.As(err, &le) {
				// loadOne returns nothing else; belt, so a future edit that
				// forgets cannot lose the metric label.
				le = &LoadError{Addon: name, Outcome: OutcomeManifestInvalid, Err: neutralize(err)}
			}
			// labelFor, not name: this is the one label in the file taken from a
			// directory entry rather than from a validated manifest.
			h.metrics.ObserveAddonLoad(labelFor(name), string(le.Outcome))
			if fatal(le) {
				_ = h.Close(ctx)
				return nil, err
			}
			log.Error("add-on failed to load; the instance continues without it",
				// The raw entry, like every other value logged here: the neutralization
				// is the handler's and a site that did it too would double it. This is
				// the one addon label in the file taken from a directory entry rather
				// than from a validated manifest, and a directory name is as much the
				// publisher's as a manifest field is.
				slog.String("addon", name),
				slog.String("outcome", string(le.Outcome)),
				slog.String("failure_class", string(ClassDegrade)),
				slog.Any("error", le.Err))
			continue
		}
		h.metrics.ObserveAddonLoad(loaded.Manifest.Name, string(OutcomeLoaded))
		h.metrics.SetAddonInfo(loaded.Manifest.Name, loaded.Manifest.Version,
			loaded.Manifest.ABIVersion, string(loaded.FailureClass),
			loaded.grants.String())
		started = append(started, loaded)
		if loaded.grants.Has(abi.PermissionNetworkFetch) && !loaded.Manifest.hasOriginSetting() {
			// Coherent and inert: the manifest asked an operator to authorize outbound
			// requests and gave them no field to say where. Validation does not refuse
			// it — an add-on that reaches nothing is the state this design produces on
			// purpose — but an operator who granted something and sees nothing happen
			// is owed the reason, and the add-on's author is the one who can fix it.
			log.Warn("an add-on declared the outbound-request permission and no setting "+
				"marked `origin`, so it can reach nothing; this is its author's to fix",
				slog.String("addon", loaded.Manifest.Name),
				slog.String("permission", abi.PermissionNetworkFetch))
		}
		// The grants are named rather than counted. A count answers "did anything
		// change" and nothing else, and what an operator reading a boot log needs to
		// know about a module they installed is which capabilities it asked for —
		// m62.md's *grants are visible*, of which this and the info gauge are the
		// minimum and M68's manager is the proper surface.
		log.Info("add-on loaded",
			slog.String("addon", loaded.Manifest.Name),
			slog.String("version", loaded.Manifest.Version),
			slog.Int("abi_version", loaded.Manifest.ABIVersion),
			// The class this host applies, not the one the manifest declared — see
			// Loaded.FailureClass. An operator reading a boot log to find out whether
			// this add-on can be skipped is asking about the applied one.
			slog.String("failure_class", string(loaded.FailureClass)),
			slog.Any("permissions", loaded.grants.Names()),
			slog.Int("declared_settings", len(loaded.Manifest.Settings)),
			// How many of them an operator has actually set, which is the question a
			// boot log can answer and `config_get` returning a default cannot. The
			// count and never a name-value pair: a setting may be a secret, and the
			// values are held as config.Secret exactly so that a line like this one
			// cannot become the leak.
			slog.Int("configured_settings", loaded.settings.len()),
			slog.String("schema", loaded.Schema),
			slog.Int("migrations", len(loaded.Manifest.Migrations)),
			slog.Uint64("guest_memory_bytes", uint64(loaded.MemorySize())))
	}

	// The set, published once now that everything that is going to load has. The
	// redirect classes and the pools are derived from it rather than accumulated
	// beside it (M66, M66.5, and newAddonSet since M67): what goes on the redirect
	// path is a property of the whole set, and a slice appended to while modules
	// were still being refused would carry an add-on that did not finish loading.
	set := h.store(newAddonSet(started, discovered, nil))
	if len(set.inline) > 0 {
		// Warn, because this is the line that tells an operator their published
		// redirect latency is no longer this product's alone to answer for. It is
		// the boundary the owner set, said out loud on the instance it applies to
		// rather than only in docs/slo.md.
		log.Warn("an add-on runs inside the redirect path; this instance's redirect "+
			"latency is no longer core's alone, and the measured figure in docs/slo.md "+
			"is core with no inline add-on on the path",
			slog.Any("addons", h.InlineAddons()),
			slog.String("deadline", h.inlineDeadline.String()),
			// Both bounds, because an operator reading one of them and not the other
			// has the wrong picture of what a redirect can cost here: the worst case
			// is a module that hangs at startup, and that is the second number.
			slog.String("instantiate_deadline", h.instantiateDeadline.String()))
	}
	// The staging area, swept before anything can be installed into it. A crash
	// between staging a module and renaming it into place leaves a directory
	// nothing will ever claim, and an operator's add-ons directory is not a place
	// to accumulate them — see lifecycle.go.
	h.sweepStaging(dir)
	h.startPoolSweep(ctx)
	h.startObserving(ctx)
	if observers := h.ObservingAddons(); len(observers) > 0 {
		log.Info("add-ons are observing redirects out of band; nothing they do can delay "+
			"or fail one, and an observation the host cannot deliver in time is dropped",
			slog.Any("addons", observers))
	}

	// Said at boot rather than only when somebody opens the manager, because an
	// orphan is data an operator is paying for and does not know about — a module
	// whose file was deleted takes its page row with it, and the schema stays. M68
	// is the surface that offers to purge one; this is the line that stops the
	// question from waiting for it.
	if orphans, err := h.OrphanSchemas(ctx); err != nil {
		log.Debug("could not look for orphaned add-on schemas", slog.Any("error", err))
	} else if len(orphans) > 0 {
		log.Warn("add-on schemas belonging to no installed add-on; their data is still "+
			"on disk and nothing here deletes it",
			slog.Any("schemas", orphans))
	}

	log.Info("add-on host started",
		slog.String("dir", dir),
		slog.Int("loaded", len(set.loaded)))
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
func claimants(dir string, entries []os.DirEntry, overrides func(string) map[string]string) []claimant {
	out := make([]claimant, 0, len(entries))
	for _, e := range entries {
		// isStaging for the same reason the load loop below skips it: this host's
		// own working directory is not an operator's add-on. Stated rather than left
		// to ReadManifest failing on it, because [Host.collidingNames] reads this
		// function as the answer to *what claims a name* and that answer has to be
		// the same one discovery gives.
		if !e.IsDir() || isStaging(e.Name()) {
			continue
		}
		m, err := ReadManifest(filepath.Join(dir, e.Name()))
		if err != nil || m.Name != e.Name() {
			continue
		}
		// The **effective** class, for the same reason loadOne uses it: a collision
		// refusal has to stop the instance when the add-on it refuses is on the
		// authentication path, and reading the manifest's own field here would have
		// let an `oidc`/`oidc_x` pair degrade past it. An override this host cannot
		// interpret leaves the class empty, which fatal() already treats as the
		// harsh case — and loadOne reports the variable by name a moment later.
		class, cerr := effectiveFailureClass(m, overrides(m.Name))
		if cerr != nil {
			class = ""
		}
		out = append(out, claimant{name: m.Name, class: class})
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
	// Every LoadError this function makes is built here, which is what makes one
	// neutralization enough. A validation error embeds the value it refused with
	// %q, and %q escapes on unicode.IsPrint — so a manifest field carrying
	// U+3164, U+FE0F or U+E0100 reached an operator's log through it untouched
	// until this line existed.
	fail := func(o Outcome, class FailureClass, err error) (Loaded, error) {
		return Loaded{}, &LoadError{Addon: entry, Outcome: o, Err: neutralize(err), class: class}
	}

	m, err := ReadManifest(dir)
	if err != nil {
		return fail(OutcomeManifestInvalid, "", err)
	}
	// Resolved once, immediately, and used for every refusal below rather than
	// class. The two differ whenever an operator overrode the class or the
	// add-on declared `session.mint`, and a `fail` call that reached for the
	// manifest's field would let an authentication add-on degrade past exactly the
	// failures this milestone made fatal.
	class, err := effectiveFailureClass(m, h.overrides(m.Name))
	if err != nil {
		// No class to fall back to — the variable that decides whether this add-on
		// may be skipped is the one that could not be read — so this is the
		// deliberately harsh limb, the same one an unparseable manifest gets.
		return fail(OutcomeManifestInvalid, "", err)
	}
	// The directory *is* the add-on's identity. Requiring the two to agree buys
	// two things: a metric label that exists before the manifest parses, and the
	// impossibility of two directories claiming one name — which would be two
	// add-ons contending for one Postgres schema at M63.
	if m.Name != entry {
		return fail(OutcomeManifestInvalid, class,
			fmt.Errorf("manifest names %q but the directory is %q; they must match", m.Name, entry))
	}
	// Before the module is read, let alone hashed or compiled. An add-on built
	// against an ABI this host does not serve is refused for a reason that is
	// knowable from 200 bytes of JSON, and doing the cheap check first is the same
	// ordering rule the rest of this function follows.
	if err := abi.CheckGeneration(m.ABIVersion); err != nil {
		return fail(OutcomeABIUnsupported, class, err)
	}

	// filepath.Join with a validated bare filename, so this cannot leave dir.
	// Validate refuses a separator, a dot entry, and anything not ending .wasm.
	wasmPath := filepath.Join(dir, m.Module)
	code, err := os.ReadFile(wasmPath) //nolint:gosec // G304: an operator-owned directory is the feature; the filename is validated to be bare
	if err != nil {
		return fail(OutcomeModuleUnreadable, class, err)
	}

	// Verified before the runtime is asked for anything. A module whose bytes are
	// not the bytes the manifest describes is not compiled, not validated and not
	// instantiated — the check is worth nothing if it happens after the wasm has
	// been parsed.
	sum := sha256.Sum256(code)
	if got := hex.EncodeToString(sum[:]); got != m.SHA256 {
		return fail(OutcomeChecksumMismatch, class,
			fmt.Errorf("%s hashes to %s, manifest says %s", m.Module, got, m.SHA256))
	}

	// The first of the two steps that run the add-on's own code, and so the first
	// under its own budget. See DefaultLoadTimeout for what the budget covers and,
	// more to the point, what it does not.
	var compiled wazero.CompiledModule
	expired, err := h.runGuest(ctx, func(ctx context.Context) error {
		var e error
		// compileWorkers is on the context because that is where wazero reads it,
		// and it is not a tuning knob: without it the budget above is not enforced
		// during compilation at all. Its comment is the measurement.
		compiled, e = h.runtime.CompileModule(
			experimental.WithCompilationWorkers(ctx, compileWorkers), code)
		return e
	})
	if err != nil {
		if expired {
			return fail(OutcomeLoadTimeout, class,
				fmt.Errorf("did not finish compiling within %v: %w", h.loadTimeout, err))
		}
		return fail(OutcomeInstantiateFailed, class, fmt.Errorf("compile: %w", err))
	}

	// From here the compiled form exists and every failure return below has to
	// close it, or it survives for the life of the process (F353).
	//
	// Measured by summing anonymous `r-xp` mappings out of /proc/self/maps, which
	// counts wazevo's compiled code and is immune to page reclaim: two failed
	// installs of one module cost `+34220 KiB` each, against `+0` on the success
	// path. Dropping every Go reference frees nothing, because wazevo holds
	// `e.compiledModules[m.ID]` strongly and only `engine.Close` nils that map —
	// so the comment further down claiming the runtime owns it is true only of
	// process shutdown. Identical bytes replayed cost `+0`, since `AssignModuleID`
	// hashes the binary; an operator rebuilding a module that will not start
	// produces distinct bytes every time, which is the loop where this is felt.
	//
	// A deferred flag rather than a Close on each of the three returns, because
	// the failure this row records is a *return that forgot*, and a fourth one
	// added later would forget the same way.
	loaded := false
	defer func() {
		if !loaded {
			_ = compiled.Close(ctx)
		}
	}()

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
		return fail(OutcomeStorageFailed, class, err)
	}

	// What an operator configured, resolved once at load for the reason the grants
	// are: config_get is a map lookup on a request's path and must not read the
	// environment per call. Declared settings only — an environment variable for a
	// setting no manifest declares is not read, which is the same scoping
	// config_get itself applies (D263).
	values := newSettingValues(h.resolveSettings(ctx, m))

	// Registered before instantiation, and this is not tidiness. Package
	// initialization runs *during* InstantiateModule — that is what makes a
	// load-time failure expressible at all — so a module whose init calls a host
	// function does so before this call returns. State registered afterwards would
	// mean every add-on's first ABI call answered StatusInternal.
	deregister := h.registerState(m, grants, storage, values)
	// The second step under a budget, and the one F287 was filed for: package
	// initialization is the add-on's own code, and a module that loops there returns
	// nothing, traps nothing, and — before this — was waited on for as long as the
	// instance was allowed to live.
	//
	// The compiled form is closed by the deferred guard above on every failure
	// return, and kept on the success path because closing it here would
	// invalidate the instance this makes. It is *not* owned by the runtime in any
	// sense that matters before shutdown — F353 measured that, and this comment
	// used to say otherwise.
	var mod api.Module
	expired, err = h.runGuest(ctx, func(ctx context.Context) error {
		var e error
		mod, e = h.runtime.InstantiateModule(ctx, compiled, guestModuleConfig(m.Name))
		return e
	})
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
		if expired {
			return fail(OutcomeLoadTimeout, class,
				fmt.Errorf("did not finish starting within %v: %w", h.loadTimeout, err))
		}
		return fail(OutcomeInstantiateFailed, class, err)
	}

	schema := ""
	if storage != nil {
		schema = storage.Schema()
	}
	// The one path that keeps the compiled form: it is about to be handed to a
	// Loaded, and Host.unload closes it from there.
	loaded = true
	return Loaded{
		Manifest: m, Dir: dir, Schema: schema, settings: values, FailureClass: class,
		module: mod, grants: grants, storage: storage, compiled: compiled,
		live: newAddonLive(),
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
	loaded := h.current().loaded
	out := make([]Loaded, len(loaded))
	copy(out, loaded)
	return out
}

// Len is how many add-ons are instantiated.
func (h *Host) Len() int {
	if h == nil {
		return 0
	}
	return len(h.current().loaded)
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
	for _, l := range h.current().loaded {
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
	// Installed, not loaded. `Schemas()` reads the add-ons that started, and an
	// add-on can be on disk without having started — a module that fails to
	// instantiate, or a manifest that stops validating while the schema it created
	// on an earlier good boot survives. Subtracting the loaded set called those
	// orphans, and M68's manager offered a still-installed add-on's rows for purge
	// on the strength of it (F281, driven end to end; D428 is the answer).
	//
	// Both sets are subtracted rather than only `discovered`. They are equal for
	// every add-on that started, so the second loop is belt: if a path ever
	// publishes a set whose `discovered` has fallen behind its `loaded`, the
	// failure is an orphan that goes unreported rather than data that gets
	// deleted, and that is the direction to fail in.
	live := make(map[string]bool, len(h.current().loaded))
	for _, name := range h.current().discovered {
		live[store.AddonSchema(name)] = true
	}
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
	for _, l := range h.current().loaded {
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
	if h == nil {
		return nil
	}
	// Serialized with Install and Remove, which is new at M67: closing a host
	// while an install is halfway through swapping the set would leave the
	// instance it just started running in a runtime that is about to be closed.
	h.installMu.Lock()
	defer h.installMu.Unlock()
	if h.runtime == nil {
		return nil
	}
	// Before the runtime, so nothing can be executing a statement on a pool that is
	// closing underneath it. The schemas themselves are untouched: closing a host is
	// not an uninstall, and an add-on's data outliving the process is the whole
	// point of it being in Postgres.
	// Before the storage pools and before the runtime, because an observing
	// invocation in flight is a guest that may still be holding a statement open
	// on one of them.
	h.stopObserving()
	// After the observers and before the runtime: an entry is a module, and closing
	// one underneath an invocation still running would be the failure stopObserving
	// exists to avoid.
	h.stopPoolSweep(ctx)
	for _, l := range h.current().loaded {
		l.storage.Close()
	}
	// Neutralized on the way out, like Route's: wazero's close error names the
	// modules it was closing, and the caller logs this through its own logger rather
	// than through the one Open wrapped.
	err := neutralize(h.runtime.Close(ctx))
	h.runtime = nil
	// One store rather than three field writes. The set is a value and the empty
	// one is a value: a reader that loaded the pointer a moment ago goes on working
	// from the set it has, and one that loads it now finds nothing installed. That
	// is what [F325](../../docs/build-notes/deferred-findings.md#open) is a row
	// about from the other side — a hot-path read racing a teardown write on
	// `h.inline` and `h.pools` — and it is closed here as a consequence of M67
	// needing the set to be swappable at all, rather than as this milestone's work.
	// **The row is not closed by this**: what it names is `Host.Close` as a whole,
	// including `h.runtime` and `h.loaded` above and the storage pools closed a few
	// lines up, and a teardown discipline for those is still the fix it asks for.
	h.store(emptyAddonSet)
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
func (l Loaded) ConfiguredSettings() int { return l.settings.len() }
