package addon

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// The schema name is derived from the add-on's name and recorded nowhere, which is
// what makes two add-ons contending for one schema impossible: the directory is the
// name, and loadOne refuses a manifest that disagrees with its directory.
func TestTheSchemaNameIsDerivedFromTheAddonName(t *testing.T) {
	if got := store.AddonSchema("clickstats"); got != "addon_clickstats" {
		t.Errorf("schema for clickstats is %q, want addon_clickstats", got)
	}
	if got := store.AddonSchemaSuffix("addon_clickstats"); got != "clickstats" {
		t.Errorf("suffix of addon_clickstats is %q, want clickstats", got)
	}
	if got := store.AddonSchemaSuffix("public"); got != "" {
		t.Errorf("public reads as add-on %q, want none", got)
	}
	// The longest name nameRe accepts, against Postgres's 63-byte identifier limit.
	// A name that produced a truncated schema would silently collide with another.
	longest := "a" + strings.Repeat("b", 30)
	if len(longest) != 31 {
		t.Fatalf("the longest name nameRe accepts is %d characters, not 31", len(longest))
	}
	if n := len(store.AddonSchema(longest)); n > 63 {
		t.Errorf("the longest add-on name makes a %d-byte schema name; Postgres truncates past 63", n)
	}
}

// An add-on that declared storage on a host with no database loads, and its
// storage calls answer StatusInternal — the status that means the host failed at
// something that is not the module's fault. Loud in the log, because in this
// product a host without a database is a test and never an instance.
func TestStorageWithNoDatabaseIsLoudAndInternal(t *testing.T) {
	code := fixture(t, "minimal")
	root := t.TempDir()
	m := manifestFor("minimal", ClassRequired, code)
	m.Permissions = []string{abi.PermissionStorage}
	install(t, root, m, code)

	h, sink, err := openHostWithLog(t, root)
	if err != nil {
		t.Fatalf("an add-on declaring storage did not load on a host with no database: %v", err)
	}
	if h.Len() != 1 {
		t.Fatalf("loaded %d add-ons, want 1", h.Len())
	}
	logs := sink.String()
	if !strings.Contains(logs, "this host has no database") {
		t.Errorf("the host did not say it has no database to honour the grant with\n%s", logs)
	}
	// No schema is claimed, so nothing about this add-on reaches the orphan report or
	// the size metric.
	if schemas := h.Schemas(); len(schemas) != 0 {
		t.Errorf("a host with no database published schemas %v", schemas)
	}
	if got := h.Addons()[0].Schema; got != "" {
		t.Errorf("the loaded add-on claims schema %q", got)
	}

	// The status a call gets, taken from the state the ABI answers against rather
	// than through a guest, because `minimal` calls nothing.
	st := h.hostState("minimal")
	if st == nil {
		t.Fatal("the host registered no state for the add-on it loaded")
	}
	if st.storage != nil {
		t.Error("the host built storage for an add-on with no database")
	}
	if got := st.noStorage("storage_query"); got != int32(abi.StatusInternal) {
		t.Errorf("a storage call answered %d, want %d (StatusInternal)", got, abi.StatusInternal)
	}
}

// An add-on that did not declare the grant gets no schema and no role. The host
// creating one anyway would be granting a capability nobody asked for, which is
// what the permission model exists to prevent.
func TestAnAddonThatDidNotAskForStorageGetsNoSchema(t *testing.T) {
	code := fixture(t, "minimal")
	root := t.TempDir()
	m := manifestFor("minimal", ClassRequired, code)
	m.Permissions = nil
	install(t, root, m, code)

	h, sink, err := openHostWithLog(t, root)
	if err != nil {
		t.Fatalf("the add-on did not load: %v", err)
	}
	if got := h.Addons()[0].Schema; got != "" {
		t.Errorf("an add-on that declared no storage claims schema %q", got)
	}
	if strings.Contains(sink.String(), "this host has no database") {
		t.Error("the host complained about a database for an add-on that asked for none")
	}
	st := h.hostState("minimal")
	if st == nil || st.storage != nil {
		t.Error("the host built storage for an add-on that did not declare it")
	}
}

// Every schema-facing method is nil-safe, because a nil *Host is the ordinary
// state of an instance that configured no add-ons and no caller should have to ask.
func TestSchemaMethodsAreNilSafe(t *testing.T) {
	var h *Host
	if s := h.Schemas(); s != nil {
		t.Errorf("Schemas on a nil host answered %v", s)
	}
	orphans, err := h.OrphanSchemas(t.Context())
	if err != nil || orphans != nil {
		t.Errorf("OrphanSchemas on a nil host answered %v, %v", orphans, err)
	}
	// Would panic rather than return if it were not nil-safe; there is nothing to
	// assert about the result of a measurement that took nothing.
	h.ObserveSchemaSizes(t.Context())
}

// A host with a directory and no database publishes no orphans and no error. An
// instance that configured no add-ons has no orphans to have, and an error here
// would make every caller ask first.
func TestOrphansOnAHostWithoutADatabase(t *testing.T) {
	root := t.TempDir()
	h, err := Open(t.Context(), Options{Dir: root, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("an empty add-ons directory did not open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })
	orphans, err := h.OrphanSchemas(t.Context())
	if err != nil {
		t.Errorf("OrphanSchemas on a host with no database returned %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("a host with no database found orphans %v", orphans)
	}
}
