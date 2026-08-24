package addon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// This file is M67: an add-on arrives and leaves without a reboot.
//
// # The store is the directory, and there is only one of them
//
// M60 reads `LINKCTRL_ADDONS_DIR` at boot and that is the whole lifecycle. This
// milestone does not add a second place an add-on can live — no table of
// installed modules, no blob column — because two stores is two answers to *what
// is installed*, and the first time they disagree the disagreement is a module
// running that nothing lists or a row for a module that is not there. Install
// writes into the same directory an operator writes into by hand, so the boot
// route keeps working unchanged and there is one set to enumerate.
//
// What that costs is stated rather than hidden, in docs/configuration.md and
// docs/SECURITY.md: the directory is per container, so an install reaches the
// replica that served the request and no other, and a container filesystem that
// is not a volume loses it on the next deploy. Both are properties of *where an
// operator mounted the directory*, which is a thing they can already see and
// control, and neither is made better by a second store that then disagrees with
// the mount.
//
// # Atomicity is one rename
//
// A crash mid-install must leave either the old world or the new one. So the
// files are written into a staging directory **inside** the add-ons directory —
// inside, because `rename(2)` does not cross filesystems and the add-ons
// directory is the one place guaranteed to be on the same one as itself — and
// then renamed into place as a unit. Before the rename, nothing in the discovery
// set has changed. After it, the whole file set is there: the module and the
// manifest that describes it, together, because a directory is what moved.
//
// Removal is the same act backwards: the directory is renamed *out* of the
// discovery set first and deleted afterwards, so a crash between the two leaves
// something in the staging area that the next boot sweeps, and never a module the
// operator removed still loading. That is the bullet about a `required` add-on
// not being able to brick the boot it was required for: the file set is gone from
// discovery the instant the rename returns, whatever happens next.
//
// Install refuses a name that already exists rather than replacing it, which is
// what keeps this to one rename. `rename(2)` onto a non-empty directory fails,
// so upgrade-in-place would need a remove and an install with a window between
// them — and m67.md puts upgrade out of scope for exactly that reason.

// stagingName is the directory install stages through, inside the add-ons
// directory.
//
// A dot name, and discovery skips it by name rather than by pattern: an operator
// with a dot-directory of their own still gets it read as an add-on and refused
// as one, which is the answer M60 gives every other unreadable entry, and only
// this host's own working directory is invisible.
const stagingName = ".staging"

// isStaging reports whether a directory entry is this host's staging area.
func isStaging(name string) bool { return name == stagingName }

// MaxUploadBytes bounds the whole install request body.
//
// 32 MiB, and it is a bound on the *transfer* rather than a statement about what
// a reasonable module weighs. The fixtures this repository builds are 1.8 MB to
// 3.6 MB because a `GOOS=wasip1` binary from big-Go carries the runtime, and a
// module written in a language with a smaller one is a few hundred kilobytes; 32
// MiB is past anything that shape produces and short of a body worth reading into
// memory by accident. It is read into memory rather than streamed for the reason
// the manifest is: the module is hashed before it is written, so the bytes are
// held either way, and streaming to disk first would mean writing an unverified
// module into the directory this instance executes from.
//
// Documented in docs/configuration.md beside the manifest bound, because meeting
// an undocumented limit as a failed install is the same experience as a bug.
const MaxUploadBytes = 32 << 20

// removeGrace bounds how long removal waits for the invocations already inside a
// guest call.
//
// **The choice m67.md asks to be made and recorded: they complete, and the bound
// is what interrupts.** Waiting is right because every one of them is already
// bounded by something else — an inline invocation by
// `LINKCTRL_ADDON_INLINE_DEADLINE` and `LINKCTRL_ADDON_INSTANTIATE_DEADLINE`, an
// observation by the same pair, a page request by the server's write timeout — so
// quiet is a state that arrives rather than one this code hopes for. Five seconds
// is comfortably past the sum of those bounds on any machine, and an operator
// who waits five seconds for a removal has not noticed.
//
// Past it the modules are closed anyway, and that is safe rather than reckless:
// wazero documents `CompiledModule.Close` as safe to call with outstanding calls
// from instances made from it. The in-flight guest call then fails, and every
// caller on these paths already handles a failed invocation — a redirect is
// served without the add-on, a page answers the 502 a trapping module produces.
// The alternative, waiting without a bound, makes one hung module able to hold a
// removal open forever, which is the state removal exists to escape.
const removeGrace = 5 * time.Second

// recordTimeout bounds the audit write a lifecycle act is detached onto.
//
// The same five seconds, and the same reasoning read the other way round: the
// write is one insert, so a bound this loose only ever fires on a database that
// is not answering, and on that database failing fast buys nothing the warning
// log does not already say. Detaching without a bound is what this is not — see
// [Host.record] — because a detached context with no deadline is a goroutine
// nothing can end.
const recordTimeout = 5 * time.Second

// InstallRequest is the pair that arrives in the request body.
//
// **Bytes, never a URL, and the absence is the design.** A field naming somewhere
// to fetch the module from would make this the cleanest server-side request
// forgery in the product: an authenticated caller naming an address the server
// connects to, on a path whose whole job is then to execute what comes back. The
// product refuses that shape everywhere else — a link's destination is validated
// against a policy, a webhook's target is validated, a root redirect's is — and
// here there is no validation that would help, because the danger is the request
// rather than the response. So the bytes cross in the body the caller already has
// to send, and this struct has no third field.
type InstallRequest struct {
	// Manifest is `addon.json` verbatim, parsed by the same reader the boot path
	// uses so that an add-on installed through the API and one placed by hand are
	// refused for the same reasons.
	Manifest []byte
	// Module is the `.wasm`, hashed against the manifest's `sha256` before it is
	// written anywhere.
	Module []byte
}

// Installed is what a lifecycle act says about the add-on it acted on.
//
// The same shape for both directions, because the useful record of a removal is
// the same set of facts as the record of an install: which module, at which
// version, with which digest. It is also what the audit metadata is built from,
// so the API's answer and the audit record cannot describe different things.
type Installed struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	ABIVersion   int          `json:"abi_version"`
	SHA256       string       `json:"sha256"`
	FailureClass FailureClass `json:"failure_class"`
	Permissions  []string     `json:"permissions"`
	// Schema is the Postgres schema this add-on owns, or empty. On a removal it is
	// the orphan that has just been created: removal deletes no data, and naming
	// the schema at the moment of the act is what stops the leftover being
	// something an operator discovers later (M63, and M68's purge offer).
	Schema string `json:"schema,omitempty"`
	// Draining is set on a removal whose in-flight invocations had not finished
	// when [removeGrace] expired, so the modules were closed under them. It is a
	// fact about what this instance did, not a warning to act on.
	Draining bool `json:"draining,omitempty"`
}

// ErrNoAddonsDir is what every lifecycle operation answers on an instance that
// configured no add-ons directory.
//
// Its own sentinel wrapping domain.ErrUnavailable rather than a 404, because the
// caller did nothing wrong and the operation would work on an instance that set
// the variable. A 404 would say the endpoint does not exist, which is a different
// and less actionable thing to tell somebody whose install just failed.
var ErrNoAddonsDir = fmt.Errorf("%w: this instance has no add-ons directory; "+
	"set LINKCTRL_ADDONS_DIR and restart before installing an add-on", domain.ErrUnavailable)

// Install verifies an uploaded module, writes it into the add-ons directory, and
// starts it — without restarting the instance.
//
// The order is the whole security argument and it is the same order [loadOne]
// uses, moved one step earlier: the manifest is parsed and the module is hashed
// **before anything is written to disk**, so bytes that are not the bytes the
// manifest describes never reach the directory the instance executes from. The
// digest is then checked again by loadOne against the file it reads, which is not
// redundant — the first check is about what arrived, the second about what landed.
func (h *Host) Install(
	ctx context.Context, actor *auth.Identity, req InstallRequest,
) (Installed, error) {
	if !actor.Can(auth.PermAddonsManage) {
		return Installed{}, fmt.Errorf("%w: installing an add-on requires %s",
			domain.ErrForbidden, auth.PermAddonsManage)
	}
	return h.install(ctx, actor, req)
}

// install is Install with the permission already established.
//
// The split is for the tests. An *auth.Identity carries its permissions in an
// unexported map with no constructor, deliberately — nothing outside internal/auth
// can mint an authority — so a unit test in this package cannot produce an actor
// that holds `addons.manage`, and a lifecycle only reachable through the check
// would be a lifecycle only testable against a database. The gate itself is
// asserted where an identity is real: test/integration, both directions.
func (h *Host) install(
	ctx context.Context, actor *auth.Identity, req InstallRequest,
) (Installed, error) {
	if h == nil || h.dir == "" {
		return Installed{}, ErrNoAddonsDir
	}

	m, err := checkUpload(req)
	if err != nil {
		return Installed{}, err
	}

	h.installMu.Lock()
	defer h.installMu.Unlock()

	old := h.current()
	if slices.ContainsFunc(old.loaded, func(l Loaded) bool { return l.Manifest.Name == m.Name }) {
		return Installed{}, fmt.Errorf("%w: %q is already installed; remove it before "+
			"installing another copy — this version has no upgrade-in-place",
			domain.ErrConflict, moduleText(m.Name))
	}
	// **The name-collision rule, applied to the runtime path.** M60 refuses two
	// add-ons whose names stand in a `name + "_"` prefix relation — the cookie
	// prefixes and the `LINKCTRL_ADDON_` variables derived from them overlap, so
	// `oidc` reads and overwrites `oidc_x`'s session cookies — and it refuses
	// *both* members of the pair at boot. Installing into a running host has to
	// apply it too, or the API would be a way to reach the state that check
	// exists to prevent, and M60's claim that neither loads would be false on a
	// host nobody restarted.
	//
	// Applying it means reading the set boot reads, which is every name *claimed*
	// in the directory and not only the ones running — see [Host.collidingNames]
	// for why the narrower set left this API able to arrange a boot that stops.
	//
	// **It refuses the arrival rather than unloading the pair.** That is the one
	// place this differs from boot, and it differs because the situations do: at
	// boot nothing is running yet and there is no principled winner, while here
	// what is already claimed is either serving or an operator's own directory,
	// and the arrival is a request somebody just made. Taking down a running
	// authentication provider — or deleting a directory somebody was debugging —
	// because somebody uploaded a badly-named module would make the API a denial
	// of service against what is already installed. The operator is told which
	// name it collides with and renames the one they are holding.
	others, err := h.collidingNames(m.Name, old.loaded)
	if err != nil {
		return Installed{}, err
	}
	if len(others) > 0 {
		return Installed{}, fmt.Errorf("%w: %q cannot be installed beside %s, which the "+
			"add-ons directory already claims: one name plus an underscore is a prefix of "+
			"the other, so the cookie prefixes and the %s variables derived from the two "+
			"names overlap, and neither would load at the next start. Rename this add-on "+
			"and its manifest, or remove the other one first",
			domain.ErrConflict, moduleText(m.Name), quoteAll(others), config.AddonEnvPrefix)
	}
	target := filepath.Join(h.dir, m.Name)
	if _, err := os.Stat(target); err == nil {
		// A directory that exists and did not load. Refused rather than replaced,
		// because the thing on disk is an operator's — placed by hand, or left by a
		// module that failed to start — and overwriting it would destroy whatever
		// they were about to debug.
		return Installed{}, fmt.Errorf("%w: the add-ons directory already holds %q, and "+
			"this instance did not load it; remove the directory or fix what stopped it "+
			"loading before installing over the name", domain.ErrConflict, moduleText(m.Name))
	} else if !errors.Is(err, os.ErrNotExist) {
		return Installed{}, fmt.Errorf("look at the add-ons directory: %w", err)
	}

	staged, err := h.stage(m, req.Manifest, req.Module)
	if err != nil {
		return Installed{}, err
	}
	// The atomic step. Everything before it is invisible to discovery; everything
	// after it is the new world, whole.
	if err := os.Rename(staged, target); err != nil {
		_ = os.RemoveAll(staged)
		return Installed{}, writableErr(err, "install into")
	}

	loaded, err := loadOne(ctx, h, target, m.Name)
	if err != nil {
		// The module is on disk and will not run, so it comes back off: leaving it
		// would mean the next boot meets a refusal the operator was already told
		// about — and, for a `required` add-on, an instance that will not start
		// because of an install that reported failure.
		_ = os.RemoveAll(target)
		var le *LoadError
		if errors.As(err, &le) {
			h.metrics.ObserveAddonLoad(labelFor(m.Name), string(le.Outcome))
		}
		return Installed{}, domain.ValidationErrors{{
			Field: "module", Code: "would_not_load",
			Message: err.Error(),
		}}
	}

	h.store(newAddonSet(append(slices.Clone(old.loaded), loaded), old.pools))
	// Both are idempotent and both are needed: an instance that booted with no
	// pooled add-on has no sweep, and one that booted with no observer has no queue
	// or worker. See their own comments for why they are not started per add-on.
	h.startPoolSweep(ctx)
	h.startObserving(ctx)

	h.metrics.ObserveAddonLoad(loaded.Manifest.Name, string(OutcomeLoaded))
	h.metrics.SetAddonInfo(loaded.Manifest.Name, loaded.Manifest.Version,
		loaded.Manifest.ABIVersion, string(loaded.FailureClass), loaded.grants.String())

	out := summarize(loaded)
	// Warn rather than info, and this is the line that matters most in this file:
	// somebody just put executable code into a running server, and the boot log's
	// equivalent is only ever read after a restart that an operator chose. The
	// grants are named for m62.md's reason — what a module asked for is what an
	// operator needs to see — and the digest so that the log and the audit record
	// can be compared against the artifact.
	h.log.Warn("an add-on was installed and started without a restart; this instance is "+
		"now running code it was not started with",
		slog.String("addon", out.Name),
		slog.String("version", out.Version),
		slog.String("sha256", out.SHA256),
		slog.String("failure_class", string(out.FailureClass)),
		slog.Any("permissions", out.Permissions),
		slog.String("schema", out.Schema))
	h.record(ctx, actor, audit.ActionAddonInstalled, out)
	return out, nil
}

// Remove unloads an add-on and takes its files out of the add-ons directory.
//
// The order is the reverse of Install's and is load-bearing in the same way:
// nothing is closed until the add-on is out of the set *and* out of the
// directory, so a crash at any point leaves either a running add-on that is
// installed or no add-on at all — never a directory the next boot loads for a
// module this one deliberately unloaded.
//
// The schema is left. That is M63's answer and not an oversight: removal creates
// an orphan, an orphan is enumerable, and offering to purge one is the surface's
// job at the point of decision (M68). [Installed.Schema] names it here so the
// caller can offer that choice.
func (h *Host) Remove(
	ctx context.Context, actor *auth.Identity, name string,
) (Installed, error) {
	if !actor.Can(auth.PermAddonsManage) {
		return Installed{}, fmt.Errorf("%w: removing an add-on requires %s",
			domain.ErrForbidden, auth.PermAddonsManage)
	}
	return h.remove(ctx, actor, name)
}

// remove is Remove with the permission already established. See install for why
// the two are split.
func (h *Host) remove(
	ctx context.Context, actor *auth.Identity, name string,
) (Installed, error) {
	if h == nil || h.dir == "" {
		return Installed{}, ErrNoAddonsDir
	}

	h.installMu.Lock()
	defer h.installMu.Unlock()

	old := h.current()
	i := slices.IndexFunc(old.loaded, func(l Loaded) bool { return l.Manifest.Name == name })
	if i < 0 {
		return Installed{}, domain.ErrNotFound
	}
	gone := old.loaded[i]
	out := summarize(gone)
	// From the manifest the host validated, never from the caller's string, even
	// though the two are equal by the lookup above. What is about to be joined onto
	// a path is a bare name Manifest.Validate refused a separator in; `name` is
	// whatever arrived in a URL, and one day the lookup will be by something else.
	name = gone.Manifest.Name

	// Out of the set first. From here nothing new resolves the add-on: a redirect
	// does not find it inline, the worker does not show it a click, and a request
	// under its prefix is the 404 an add-on that is not installed gets.
	kept := slices.Delete(slices.Clone(old.loaded), i, i+1)
	h.store(newAddonSet(kept, old.pools))

	// Out of the directory second, and this is the bullet about a `required`
	// add-on: the rename is what makes the removal survive a crash, because after
	// it the next boot cannot find a module to stop for. Renamed rather than
	// deleted so the act is atomic — a delete of a directory is a walk, and a walk
	// interrupted halfway leaves a manifest without its module, which is a
	// refusal at boot rather than an absence.
	stage, err := h.stagingRoot()
	if err != nil {
		h.store(old)
		return Installed{}, err
	}
	// Named so that a leftover in the staging area says what it was and when,
	// which is the whole of what somebody looking at one needs.
	parked := filepath.Join(stage, "removed-"+name+"-"+
		strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Rename(filepath.Join(h.dir, name), parked); err != nil {
		// Nothing has been closed yet, so putting the set back is a complete undo.
		h.store(old)
		return Installed{}, writableErr(err, "remove from")
	}

	// Only now is anything destroyed. Every step below is about this process's
	// memory and connections; the operator's view of what is installed changed two
	// statements ago.
	out.Draining = !h.quiet(gone)
	h.unload(ctx, gone, old.pools)
	if err := os.RemoveAll(parked); err != nil {
		// The add-on is gone from discovery and from this process either way, so
		// this is not a failed removal — it is disk this host could not tidy, and
		// the next boot's sweep will.
		h.log.Warn("an add-on was removed and its parked files could not be deleted; "+
			"the next start clears the staging area",
			slog.String("addon", name), slog.Any("error", err))
	}

	h.metrics.ForgetAddon(out.Name, out.Version, out.ABIVersion,
		string(out.FailureClass), gone.grants.String())
	h.log.Warn("an add-on was removed and unloaded without a restart",
		slog.String("addon", out.Name),
		slog.String("version", out.Version),
		slog.String("sha256", out.SHA256),
		slog.Bool("draining", out.Draining),
		// Named because it is what is left behind. Nothing here deletes it, and an
		// operator reading this line is the one who can decide to.
		slog.String("orphaned_schema", out.Schema))
	h.record(ctx, actor, audit.ActionAddonRemoved, out)
	return out, nil
}

// collidingNames is every name already claimed on this host whose namespace
// overlaps this one's.
//
// The predicate is [nameCollisions]'s, over one candidate rather than over every
// pair — which is what the runtime case is: everything already claimed has been
// checked against everything else.
//
// **Claimed, not loaded**, and that distinction is the whole of this function.
// Boot decides collisions over [claimants]: every directory whose manifest parses
// and names the directory it sits in, *whether or not the add-on then loaded*. A
// directory with a valid manifest and a wrong digest is refused at boot and is
// absent from the running set, and it still claims its name. Reading only the
// running set here would allow `oidc_x` to be installed beside a refused `oidc`,
// and the next boot would then refuse **both** — an instance that does not start,
// if either is `required`, reached through a shipped API by an operator doing
// nothing wrong. That is exactly the state M60's boot check exists to prevent, so
// the two checks read the same set or the API is a way around one of them.
//
// Agreeing means agreeing on the exclusions too, and [claimants] owns all of
// them: a non-directory, a manifest that will not parse, a manifest naming some
// other directory, and this host's own staging area. So nothing refuses an
// install that discovery would have ignored.
//
// The running set is unioned on top for the one claim a directory read cannot
// see: an add-on this process loaded whose directory was deleted underneath it.
// It is still serving and its cookie prefixes are still live, so it still claims
// its name — and refusing against it is what this check already did.
func (h *Host) collidingNames(name string, loaded []Loaded) ([]string, error) {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return nil, fmt.Errorf("look at the add-ons directory: %w", err)
	}
	claimed := make(map[string]struct{}, len(entries)+len(loaded))
	for _, c := range claimants(h.dir, entries, h.overrides) {
		claimed[c.name] = struct{}{}
	}
	for _, l := range loaded {
		claimed[l.Manifest.Name] = struct{}{}
	}
	var out []string
	for other := range claimed {
		if strings.HasPrefix(other, name+"_") || strings.HasPrefix(name, other+"_") {
			out = append(out, other)
		}
	}
	// Sorted because a map is not, and the refusal names these to an operator.
	slices.Sort(out)
	return out, nil
}

// checkUpload parses and verifies the pair, before anything is written.
func checkUpload(req InstallRequest) (Manifest, error) {
	if len(req.Manifest) == 0 {
		return Manifest{}, domain.ValidationErrors{{
			Field: "manifest", Code: "required",
			Message: "the body carries no " + ManifestFile + " part",
		}}
	}
	if len(req.Module) == 0 {
		return Manifest{}, domain.ValidationErrors{{
			Field: "module", Code: "required",
			Message: "the body carries no module part",
		}}
	}
	m, err := parseManifest(bytes.NewReader(req.Manifest))
	if err != nil {
		return Manifest{}, domain.ValidationErrors{{
			Field: "manifest", Code: "invalid", Message: neutralize(err).Error(),
		}}
	}
	// **Before the bytes are written, which is the point of doing it here rather
	// than leaving it to loadOne.** A module whose digest does not match is never
	// on this instance's disk at all, so there is no window in which the add-ons
	// directory holds something the manifest does not describe.
	sum := sha256.Sum256(req.Module)
	if got := hex.EncodeToString(sum[:]); got != m.SHA256 {
		return Manifest{}, domain.ValidationErrors{{
			Field: "module", Code: "checksum_mismatch",
			Message: fmt.Sprintf("the module hashes to %s and the manifest says %s", got, m.SHA256),
		}}
	}
	// **The one thing this surface cannot install**, named as a refusal rather
	// than left to fail as a missing file. A manifest may declare `.sql` files the
	// host applies into the add-on's schema (M63), and those files are neither of
	// the two parts this request carries — m67.md's install is a module and its
	// manifest. An add-on that owns a schema and creates its tables from its own
	// code installs here; one that ships DDL files does not, and is told so in a
	// sentence rather than by a load failure naming a path.
	if len(m.Migrations) > 0 {
		return Manifest{}, domain.ValidationErrors{{
			Field: "manifest", Code: "migrations_unsupported",
			Message: "this add-on declares migration files, and an upload carries only " +
				"the module and its manifest; install it by placing its directory in " +
				"LINKCTRL_ADDONS_DIR and restarting",
		}}
	}
	return m, nil
}

// stage writes the pair into a fresh directory inside the staging area and
// returns its path.
//
// Nothing here is in the discovery set, so a failure at any point needs no undo
// beyond deleting what was written.
func (h *Host) stage(m Manifest, manifest, module []byte) (string, error) {
	root, err := h.stagingRoot()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, m.Name+"-")
	if err != nil {
		return "", writableErr(err, "install into")
	}
	// The manifest is written **verbatim**, not re-encoded from the parsed struct.
	// What a reviewer inspected before installing is the file's own text, and
	// nothing in this product covers those bytes with a digest (see
	// checkManifestKeys), so re-serializing would silently substitute this build's
	// idea of the document for the one somebody read.
	//
	// 0o600 and 0o700: the process that executes a module has no reason to let
	// anybody else on the box rewrite it, and the trust boundary
	// docs/configuration.md describes is *who may write to this directory*.
	for _, f := range []struct {
		name string
		body []byte
	}{{ManifestFile, manifest}, {m.Module, module}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.body, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", writableErr(err, "install into")
		}
	}
	return dir, nil
}

// stagingRoot is the staging directory, created if it is not there.
func (h *Host) stagingRoot() (string, error) {
	root := filepath.Join(h.dir, stagingName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", writableErr(err, "install into")
	}
	return root, nil
}

// sweepStaging deletes whatever a crash left in the staging area.
//
// At boot, and only at boot: everything in there belongs to an operation that is
// no longer running, because the only writer is this process and it has just
// started. A leftover is a half-written install or a removal whose delete did not
// finish, and neither is anything but disk.
func (h *Host) sweepStaging(dir string) {
	root := filepath.Join(dir, stagingName)
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) == 0 {
		return
	}
	if err := os.RemoveAll(root); err != nil {
		h.log.Warn("could not clear the add-on staging area; it holds files a previous "+
			"install or removal did not finish with, and nothing loads from it",
			slog.String("dir", root), slog.Any("error", err))
		return
	}
	h.log.Info("cleared the add-on staging area",
		slog.String("dir", root), slog.Int("entries", len(entries)))
}

// quiet seals the add-on and waits for the invocations already inside a guest
// call, reporting whether they all finished inside [removeGrace].
func (h *Host) quiet(l Loaded) bool {
	select {
	case <-l.live.seal():
		return true
	case <-time.After(removeGrace):
		h.log.Warn("an add-on still had invocations running when it was removed; they "+
			"are being interrupted",
			slog.String("addon", l.Manifest.Name),
			slog.Int("in_flight", l.live.inFlight()),
			slog.String("waited", removeGrace.String()))
		return false
	}
}

// unload releases everything one add-on holds in this process.
//
// The order is the reverse of the order loadOne acquired them, which is the only
// order in which no step can be undone by a later one: the pooled instances are
// made from the compiled module, the compiled module is the runtime's, and the
// storage pool is what a guest statement runs on.
func (h *Host) unload(ctx context.Context, l Loaded, pools map[string]*addonPool) {
	// **Detached from the caller before anything is closed, and this is the one
	// place in the lifecycle where that is not a nicety.** Remove is reached from a
	// request handler with the client's context and then waits up to [removeGrace]
	// for the invocations already inside a guest call, so a caller hanging up
	// mid-removal is ordinary rather than exotic. By the time execution is here the
	// add-on is out of the set and its files are out of the directory, so there is
	// nothing left to retry the close: whatever a cancelled context declined to
	// release stays resident for the life of the process, which is exactly the leak
	// m67.md bounds. Every close in the pool detaches for the same reason
	// (closeInstance), and cmd/linkctrl/main.go states the rule at shutdown — a
	// runtime told to close on a cancelled context would refuse the close itself.
	ctx = context.WithoutCancel(ctx)
	name := l.Manifest.Name
	// M66.5's seam, and the bullet m67.md wrote into this file when that milestone
	// was added: the pool keeps instances rather than destroying them, so unloading
	// has something to release that M66 did not have. Drained from the *old* pool
	// map, because the set stored above no longer has this add-on's pools in it.
	h.drainPool(ctx, name, pools)
	if l.module != nil {
		_ = l.module.Close(ctx)
	}
	if l.compiled != nil {
		// **Promptness rather than a leak fix, and that was measured.** wazero
		// releases a compiled module's mapping from a finalizer once nothing
		// references it, so forty install/remove cycles without this line moved the
		// resident set by −208 KiB. It is still right to close it: waiting for a
		// collection to release megabytes of mapped code is a thing that happens
		// eventually rather than a thing that happens. Safe with outstanding calls
		// by wazero's own contract — see removeGrace.
		_ = l.compiled.Close(ctx)
	}
	// After the runtime objects, so nothing can be executing a statement on a pool
	// that is closing underneath it — Close's ordering, applied to one add-on.
	l.storage.Close()
	// The add-on is gone, so nothing may answer an ABI call in its name. Registered
	// under the manifest name by registerState; the per-instance entries the pool
	// made were cleared by drainPool.
	h.clearState(name)
}

// record writes the audit event for one lifecycle act.
//
// Logged rather than returned on failure, like every other post-write record in
// this product: the add-on has already been installed or removed, and failing the
// request now would tell the caller nothing happened when something did.
//
// No TargetID. An add-on is not a row and has no uuid; what identifies it is its
// name, which is in the metadata beside the version and the digest that say
// *which* module it was.
//
// `InstanceWide` because an add-on belongs to no organization; the actions
// themselves are [audit.ActionAddonInstalled] and [audit.ActionAddonRemoved],
// declared in internal/audit with every other action for the reason D343 gives.
//
// **Detached from the caller's context, which no other audit call site in this
// product does, and the difference is [removeGrace].** Everywhere else the write
// follows the act by microseconds, so a caller who disconnects between the two
// has hit a window nothing can be designed against. Removal holds the request
// open for up to five seconds waiting for guest calls to finish, and a client
// timing out inside a window the server chose is ordinary. The act has already
// happened by then — module unloaded, files gone — so a lost record is
// *"every lifecycle act is audited"* being false about the removal an operator
// most wants to find later, the one that did not answer. Bounded rather than
// merely detached: [recordTimeout], the house shape for a detached write.
func (h *Host) record(ctx context.Context, actor *auth.Identity, action string, a Installed) {
	if h.auditor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	meta := map[string]any{
		"module":        a.Name,
		"version":       a.Version,
		"sha256":        a.SHA256,
		"abi_version":   a.ABIVersion,
		"failure_class": string(a.FailureClass),
		"permissions":   a.Permissions,
	}
	if a.Schema != "" {
		meta["schema"] = a.Schema
	}
	if a.Draining {
		meta["draining"] = true
	}
	if err := h.auditor.Record(ctx, actor, audit.Event{
		Action: action, TargetType: "addon", Metadata: meta, InstanceWide: true,
	}); err != nil {
		h.log.Warn("an add-on's lifecycle changed and the audit record was not written",
			slog.String("action", action), slog.String("addon", a.Name),
			slog.Any("error", err))
	}
}

// summarize is one add-on as both the API answer and the audit record see it.
func summarize(l Loaded) Installed {
	return Installed{
		Name:         l.Manifest.Name,
		Version:      l.Manifest.Version,
		ABIVersion:   l.Manifest.ABIVersion,
		SHA256:       l.Manifest.SHA256,
		FailureClass: l.FailureClass,
		Permissions:  l.grants.Names(),
		Schema:       l.Schema,
	}
}

// writableErr turns a filesystem refusal into the answer an operator can act on.
//
// A read-only mount is the case worth naming, and it is the *documented* one:
// docs/configuration.md tells an operator to mount the add-ons directory `:ro`,
// which is still the right advice for an instance that installs by hand and is
// exactly what stops this API from working. Saying so is better than either
// changing the advice or letting the operation fail with `permission denied`.
func writableErr(err error, verb string) error {
	// Both, because they are two different mounts. `:ro` on a bind mount is EROFS
	// and a directory the container's user does not own is EACCES, and an operator
	// who set up either one meant the same thing.
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("%w: this instance cannot %s its add-ons directory, which is what "+
			"a read-only mount means; install by hand and restart, or mount it writable and "+
			"accept that the process can rewrite the code it runs", domain.ErrUnavailable, verb)
	}
	return fmt.Errorf("%s the add-ons directory: %w", verb, err)
}
