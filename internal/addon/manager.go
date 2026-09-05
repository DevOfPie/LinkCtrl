package addon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/store"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// This file is what the Add-on manager reads (M68): what is installed, what each
// module costs the redirect path, and what data is left over from modules that are
// not installed any more.
//
// # Read here, rendered in two places, decided in neither
//
// The page and `GET /api/v1/addons` are the same call — the inherited *every UI
// feature has API support* rule, and the mechanism behind it is that there is
// nothing in either surface to diverge. Everything below is a read except
// [Host.PurgeData], which is the one destructive act the manager offers and is the
// reason the milestone puts the offer and the act at the same point of decision.
//
// # Nothing here is cached
//
// A schema's size is read from the catalogue at the moment the page is rendered,
// not from the gauge the maintenance job publishes on a schedule. m68.md's second
// risk is exactly that: the number beside a purge button is what makes "this
// deletes data" concrete, and a figure an hour stale beside a confirmation is
// worse than no figure. Two catalogue queries and two indexed counts per orphan,
// on a page an operator opens rarely.

// DeclarationClass is how an add-on relates to the redirect path, as the manager's
// list column names it.
//
// Three values, and they are the three the milestone's list column asks for. It is
// derived from what the add-on **holds** rather than from what its manifest
// declared, for the reason `linkctrl_addon_info`'s permission label is: a
// permission the vocabulary carries and no host grants yet is declarable and not
// held, and a column reading the manifest would promise behaviour the module
// cannot perform.
type DeclarationClass string

const (
	// ClassNone is an add-on that is not on the redirect path at all — pages,
	// authentication, storage. The commonest case.
	ClassNone DeclarationClass = "none"
	// ClassRedirectObserve is an add-on that sees redirects after the visitor has
	// been answered.
	ClassRedirectObserve DeclarationClass = "redirect-observe"
	// ClassRedirectInline is an add-on that runs inside the redirect, on the
	// visitor's own latency. Listed last because it is the one that can cost
	// somebody something.
	ClassRedirectInline DeclarationClass = "redirect-inline"
)

// Declaration is this add-on's class. Inline outranks observe: a module holding
// both runs on the path, which is the fact the column exists to surface.
func (l Loaded) Declaration() DeclarationClass {
	switch {
	case l.RunsInline():
		return ClassRedirectInline
	case l.ObservesRedirects():
		return ClassRedirectObserve
	default:
		return ClassNone
	}
}

// Managed is one installed add-on as the manager's list and detail pages see it.
//
// It carries [Installed] rather than repeating its fields, so the lifecycle's
// answer and the manager's row cannot come to describe the same module
// differently — the same reason M67 made one summary serve both directions.
type Managed struct {
	Installed
	// Declaration is the list's CLASS column.
	Declaration DeclarationClass `json:"declaration"`
	// Declared is what the manifest asked for, which is m68.md's column and is not
	// always [Installed.Permissions] — that one is what the add-on **holds**, and a
	// permission this build publishes and grants to nobody is declarable and not
	// held.
	//
	// Both, rather than one, because they answer different questions and the
	// milestone asks for the first: *what does this add-on say it needs* is what an
	// operator agreed to when they installed it, and *what does it hold* is what
	// this build will let it do. The two are identical today — nothing declarable
	// is ungranted since M66 turned `redirect.inline` on — and the page marks the
	// difference rather than assuming it away, because the last time they diverged
	// it was for a whole phase.
	Declared []string `json:"declared_permissions"`
	// Performance is what this module cost, cumulative since this process started:
	// its invocations on the redirect path, and — since M68.5 — its outbound
	// requests, which are a different path with a different bound and sit beside
	// the redirect figures rather than inside them.
	//
	// Absent — `IsZero()`, which is `Observed()` false on **both** halves — for a
	// module with no record of either kind. The page draws a dash for whichever
	// half a module has no record of, and the API omits the whole object rather
	// than answering with one full of zeros. The two predicates ask different
	// questions and M68.5 is where they stopped coinciding: a module that has only
	// ever fetched has never run on the redirect path, so its row still draws a
	// dash there while its JSON carries the object.
	//
	// **It is in the JSON as well as on the page**, and that is the inherited *every
	// UI feature has API support* rule read as it is written: the figures are the
	// reason the manager exists, so a client that could list add-ons and not read
	// them would be a second surface with less in it. `/metrics` is not an answer
	// here — it is on a listener this product does not publish, for the reason the
	// add-on inventory is operational detail.
	Performance observability.AddonPerformance `json:"performance,omitzero"`
	// SchemaBytes is the on-disk size of the add-on's own schema, or 0 for a module
	// that declared no storage. Measured at render time; see this file's header.
	SchemaBytes int64 `json:"schema_bytes,omitempty"`
	// DeclaredSettings is how many settings the manifest declares and Configured is
	// how many have a value. The pair is what the list needs to say *3 of 5 set*
	// without reading a value it may not echo.
	//
	// **The manifest's own list, which since M69.5 is not all of [Settings].** An
	// add-on that asked for a sign-in link gets one more row on the detail page —
	// the host's consent toggle — and it is deliberately not counted here: this
	// figure answers *what did this add-on ask to be configured with*, and the
	// consent is this instance's question rather than the add-on's. Making the two
	// agree would mean calling the host's question the add-on's.
	DeclaredSettings int `json:"declared_settings"`
	ConfiguredCount  int `json:"configured_settings"`
	// Settings is the detail page's render model, and is nil on the list. Two
	// shapes rather than two types, because a list row and a detail page describe
	// one add-on and a second type would be a second thing to keep true.
	Settings []SettingView `json:"settings,omitempty"`
	// MemoryBytes is the resident guest memory of the load-time instance. What an
	// operator sizes a host by, and the number M64's bound is expressed in.
	MemoryBytes uint32 `json:"memory_bytes,omitempty"`
}

// declaredPermissions is the manifest's list, never nil.
//
// Never nil because the field is required in the API document and an add-on that
// declares none is ordinary: `[]` and `null` are different answers, and a client
// should not have to handle both for the commonest case. Same reasoning as sqlc's
// `emit_empty_slices`, applied by hand where no generator does it.
func declaredPermissions(l Loaded) []string {
	out := slices.Clone(l.Manifest.Permissions)
	if out == nil {
		return []string{}
	}
	return out
}

// Orphan is an `addon_*` schema no installed module owns.
//
// The name is the add-on it belonged to rather than the schema, because that is
// the string an operator recognises; [Schema] is what will actually be dropped and
// is shown beside it, because a confirmation that names something other than what
// it deletes is not a confirmation.
type Orphan struct {
	Name   string `json:"name"`
	Schema string `json:"schema"`
	// Bytes is every relation in the schema that has storage, with its indexes and
	// its TOAST — store.AddonSchemaBytes, measured now.
	Bytes int64 `json:"bytes"`
	// LargeObjects is how many large objects the schema's role owns. **They are not
	// deleted by a purge** — they live outside every schema — so the number is here
	// to be honest about what is left rather than to describe what goes.
	LargeObjects int64 `json:"large_objects"`
	// IdentityLinks is how many `addon_identity_links` rows were written under this
	// name, and it is here for the same reason: **a purge deletes none of them.**
	//
	// One of the four things `PurgeAddonSchema` leaves standing, and one of the two
	// an operator is least likely to predict — the mappings are keyed on the
	// add-on's *name*, so a different module installed under a name that has been
	// used before inherits every account mapping the previous one wrote and can
	// mint a session against them on its first assertion (docs/SECURITY.md; F330
	// carries the removal-side answer). The confirmation is the point of decision
	// where that can still be acted on, so the number is measured for it rather
	// than described in prose the page does not carry.
	IdentityLinks int64 `json:"identity_links"`
	// StoredSettings is how many `addon_settings` rows were saved under this name,
	// and it is the fourth. Same key, same inheritance, same silence: a value an
	// operator typed into the manager's detail page — possibly a `secret` — is
	// keyed on the add-on's name (04800), survives both the removal and the purge,
	// and is handed to whatever is installed under that name next. **Nothing in
	// this product deletes one**: `SaveSettings` refuses a name that is not loaded,
	// so a removed add-on's rows are unreachable from every surface here. F332 in
	// docs/build-notes/deferred-findings.md carries that half. What this number
	// buys is that the point of decision says so with a figure instead of leaving
	// it to the migration's comment.
	StoredSettings int64 `json:"stored_settings"`
	// Measured says the size in [Orphan.Bytes] came from the catalogue rather than
	// from the read having failed.
	//
	// Not in the JSON: a client reading a *list* can ask again, and the list is the
	// only place this type is answered before the schema is gone. The audit record
	// is where it matters, because that row is the durable answer to *how much did
	// that delete* and `0` is what an empty schema honestly measures — so a failed
	// read written as a figure is indistinguishable from a true one, in the one
	// record that outlives the thing it describes.
	Measured bool `json:"-"`
}

// List is every installed add-on, in discovery order, with what the manager's list
// page draws beside each.
//
// Requires `addons.manage`. An add-on's name, version and declared permissions are
// an inventory of what this box runs — `/metrics` is not published for exactly
// that reason (docs/SECURITY.md) — so the page that prints them is behind the same
// scope as the API that installs them, and there is no lighter read.
func (h *Host) List(ctx context.Context, actor *auth.Identity) ([]Managed, error) {
	if !actor.Can(auth.PermAddonsManage) {
		return nil, fmt.Errorf("%w: listing add-ons requires %s",
			domain.ErrForbidden, auth.PermAddonsManage)
	}
	loaded := h.Addons()
	perf := h.metrics.AddonPerformance()
	out := make([]Managed, 0, len(loaded))
	for _, l := range loaded {
		out = append(out, h.managed(ctx, l, perf[l.Manifest.Name], false))
	}
	return out, nil
}

// Detail is one installed add-on with its settings resolved.
func (h *Host) Detail(ctx context.Context, actor *auth.Identity, name string) (Managed, error) {
	if !actor.Can(auth.PermAddonsManage) {
		return Managed{}, fmt.Errorf("%w: reading an add-on requires %s",
			domain.ErrForbidden, auth.PermAddonsManage)
	}
	l := h.find(name)
	if l == nil {
		return Managed{}, domain.ErrNotFound
	}
	return h.managed(ctx, *l, h.metrics.AddonPerformance()[name], true), nil
}

// managed builds one row. `full` adds the detail page's settings.
func (h *Host) managed(
	ctx context.Context, l Loaded, perf observability.AddonPerformance, full bool,
) Managed {
	m := Managed{
		Installed:        summarize(l),
		Declaration:      l.Declaration(),
		Declared:         declaredPermissions(l),
		Performance:      perf,
		DeclaredSettings: len(l.Manifest.Settings),
		ConfiguredCount:  l.ConfiguredSettings(),
		MemoryBytes:      l.MemorySize(),
	}
	if l.Schema != "" && h.db != nil {
		if n, err := store.AddonSchemaBytes(ctx, h.db, l.Manifest.Name); err == nil {
			m.SchemaBytes = n
		} else {
			// A size that could not be measured is drawn as unknown rather than as
			// zero, and the page has nothing else that depends on it — the same trade
			// the shell makes for the notification badge.
			h.log.Debug("could not measure an add-on's schema for the manager",
				slog.String("addon", l.Manifest.Name), slog.Any("error", err))
		}
	}
	if full {
		m.Settings = h.settingViews(ctx, l)
	}
	return m
}

// Orphans is every add-on schema in this database that no installed module owns,
// with its size measured now.
//
// [Host.OrphanSchemas] is the enumeration M63 built and this is the manager's
// reading of it: the same subtraction, plus the three measurements that make a
// purge offer honest — what the drop takes, and the two things keyed on the name
// that it leaves. Requires `addons.manage`.
func (h *Host) Orphans(ctx context.Context, actor *auth.Identity) ([]Orphan, error) {
	if !actor.Can(auth.PermAddonsManage) {
		return nil, fmt.Errorf("%w: listing orphaned add-on data requires %s",
			domain.ErrForbidden, auth.PermAddonsManage)
	}
	schemas, err := h.OrphanSchemas(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Orphan, 0, len(schemas))
	for _, schema := range schemas {
		name := store.AddonSchemaSuffix(schema)
		if name == "" {
			// Unreachable: OrphanSchemas selects on the prefix this function strips.
			// Skipped rather than rendered, because a row whose purge button would
			// refuse the name is worse than no row.
			continue
		}
		o := Orphan{Name: name, Schema: schema}
		if n, err := store.AddonSchemaBytes(ctx, h.db, name); err == nil {
			o.Bytes, o.Measured = n, true
		} else {
			h.log.Debug("could not measure an orphaned schema",
				slog.String("schema", schema), slog.Any("error", err))
		}
		if n, err := store.AddonLargeObjects(ctx, h.db, name); err == nil {
			o.LargeObjects = n
		}
		q := dbgen.New(h.db)
		if n, err := q.CountAddonIdentityLinks(ctx, name); err == nil {
			o.IdentityLinks = n
		} else {
			h.log.Debug("could not count an orphaned add-on's identity links",
				slog.String("addon", name), slog.Any("error", err))
		}
		if n, err := q.CountAddonSettings(ctx, name); err == nil {
			o.StoredSettings = n
		} else {
			h.log.Debug("could not count an orphaned add-on's stored settings",
				slog.String("addon", name), slog.Any("error", err))
		}
		out = append(out, o)
	}
	return out, nil
}

// PurgeData drops one orphaned add-on's schema and everything in it.
//
// # It refuses to purge an installed add-on's data, and that is the whole check
//
// The offer is made beside the orphan list, so the name always arrives from a row
// this instance drew. It is checked again here anyway: a `DELETE` is an address a
// client can type, the manager is not the only way to reach it, and dropping the
// schema out from under a running module is a failure mode with no upside — the
// add-on's next storage call would fail, its migrations would not re-run until the
// next load, and nothing would have been gained over removing the add-on first.
// The refusal is a conflict rather than a not-found, because the schema does
// exist; it is the state that is wrong.
//
// # And it refuses a name that names nothing
//
// A schema that is not in the enumeration is a 404 rather than a silent success.
// `DROP SCHEMA IF EXISTS` would answer "done" for a typo, and an operator who
// mistyped a name would be told their data was deleted.
func (h *Host) PurgeData(
	ctx context.Context, actor *auth.Identity, name string,
) (Orphan, error) {
	if !actor.Can(auth.PermAddonsManage) {
		return Orphan{}, fmt.Errorf("%w: purging an add-on's data requires %s",
			domain.ErrForbidden, auth.PermAddonsManage)
	}
	if h == nil || h.db == nil {
		return Orphan{}, ErrNoAddonDatabase
	}

	// Under installMu, with the guard below, because the guard is a time-of-check
	// otherwise (F352). `Install`, `Remove` and `Close` all hold this lock; this
	// function held nothing, so an install landing between the `h.find` below and
	// the `DROP SCHEMA` at the end returned success and had the schema it had just
	// created dropped underneath it. Reproduced 3/3 — `install: err=<nil>
	// schema="addon_racer"` then `installed=true schemaExists=false` — with the
	// window measured at 12.8–16.4ms, and that is the floor: `MigrateAddon` runs
	// inside it, so a real add-on's DDL widens it arbitrarily.
	//
	// Postgres does not guard it either: dropping a schema whose add-on holds an
	// open pooled connection returned no error at all in 1.44ms.
	//
	// Neither actor needs to be hand-racing. Install and purge are ordinary
	// concurrent HTTP handlers, so a reconcile script that installs and then purges
	// stale orphans meets this. The lock is held across the read and the drop
	// because that pair is the whole of the check — this function's own comment
	// already argues that dropping under a running module *is a failure mode with
	// no upside*.
	h.installMu.Lock()
	defer h.installMu.Unlock()

	if l := h.find(name); l != nil {
		return Orphan{}, fmt.Errorf("%w: %s is installed, so its data is not orphaned; "+
			"remove the add-on first and then purge what it leaves", domain.ErrConflict, name)
	}
	orphans, err := h.Orphans(ctx, actor)
	if err != nil {
		return Orphan{}, err
	}
	i := slices.IndexFunc(orphans, func(o Orphan) bool { return o.Name == name })
	if i < 0 {
		return Orphan{}, domain.ErrNotFound
	}
	gone := orphans[i]

	if err := store.PurgeAddonSchema(ctx, h.db, name); err != nil {
		return Orphan{}, err
	}
	fields := []any{
		slog.String("addon", gone.Name),
		slog.String("schema", gone.Schema),
		// Named because the purge did not take them and nothing else will. Zero for
		// every add-on that behaves.
		slog.Int64("large_objects_left", gone.LargeObjects),
		// The same, and the two somebody will come back for: an account mapping and
		// a saved setting written under this name both survive the drop and are
		// inherited by whatever is installed under the name next.
		slog.Int64("identity_links_left", gone.IdentityLinks),
		slog.Int64("stored_settings_left", gone.StoredSettings),
	}
	if gone.Measured {
		fields = append(fields, slog.Int64("bytes", gone.Bytes))
	} else {
		// See [Orphan.Measured]: an unmeasured schema is not written as `0`, which
		// is what an empty one measures.
		fields = append(fields, slog.Bool("bytes_measured", false))
	}
	h.log.Warn("an add-on's orphaned data was purged from the Add-on manager", fields...)
	h.recordPurge(ctx, actor, gone)
	return gone, nil
}

// recordPurge writes the audit row for a purge.
//
// Detached and bounded like every other record in this package. The size is in the
// metadata because nothing can measure the schema afterwards, which makes this row
// the **durable** answer to *how much did that remove*. The other two carriers are
// both momentary: the API's purge response hands the number to whoever made the
// call, and [Host.PurgeData] logs it — so the log's retention is what bounds one
// and nothing keeps the other.
func (h *Host) recordPurge(ctx context.Context, actor *auth.Identity, o Orphan) {
	if h.auditor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	meta := map[string]any{
		"module": o.Name,
		"schema": o.Schema,
	}
	if o.Measured {
		meta["bytes"] = o.Bytes
	} else {
		// **Left out rather than written as `0`.** This row is the durable record
		// and nothing can measure the schema afterwards, so a failed catalogue read
		// recorded as a figure would be indistinguishable from an empty schema
		// forever. A reader that finds no `bytes` and a `bytes_measured: false`
		// knows which of the two happened; one that finds `0` does not.
		meta["bytes_measured"] = false
	}
	if o.LargeObjects > 0 {
		meta["large_objects_left"] = o.LargeObjects
	}
	if o.IdentityLinks > 0 {
		meta["identity_links_left"] = o.IdentityLinks
	}
	if o.StoredSettings > 0 {
		meta["stored_settings_left"] = o.StoredSettings
	}
	if err := h.auditor.Record(ctx, actor, audit.Event{
		Action: audit.ActionAddonDataPurged, TargetType: "addon",
		Metadata: meta, InstanceWide: true,
	}); err != nil {
		h.log.Error("an add-on's data was purged and the audit record was not written",
			slog.String("addon", o.Name), slog.Any("error", err))
	}
}

// RemoveGrace is how long a removal waits for invocations already inside the
// module, exposed so the manager's confirmation can say what removing one costs
// rather than describing it in prose that could drift from the bound.
const RemoveGrace = removeGrace
