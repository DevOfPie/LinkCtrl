//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/geoip"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// makePartition creates one monthly partition by hand, so a test can produce a
// past that the template database does not have.
func makePartition(t *testing.T, pool *pgxpool.Pool, table string, year int, month time.Month) string {
	t.Helper()
	from := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	name := store.PartitionName(table, from)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
		name, table,
		from.Format("2006-01-02 15:04:05-07"),
		from.AddDate(0, 1, 0).Format("2006-01-02 15:04:05-07")))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return name
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(t.Context(),
		"SELECT to_regclass('public.' || $1) IS NOT NULL", name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

// analyticsOnly is the policy with an analytics window and audit set to keep
// forever — the shipped default, and the one most of these tests want.
func analyticsOnly(days int) store.RetentionPolicy {
	return store.NewRetentionPolicy(days, 0)
}

// Retention is enforced by dropping whole months, and only months whose newest
// possible row is already outside the window.
func TestRetentionDropsWholeExpiredMonths(t *testing.T) {
	pool := newDB(t)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	old := makePartition(t, pool, "click_events", 2024, time.January)
	oldVisitors := makePartition(t, pool, "visitors", 2024, time.January)
	// Ends 2026-07-01, which is after a 30-day cutoff of 2026-06-30: it still
	// holds rows inside the window, so it must survive. This is the boundary the
	// whole design turns on — dropping it would delete retained data.
	edge := makePartition(t, pool, "click_events", 2026, time.June)
	// Audit logs are partitioned the same way and are deliberately not subject to
	// the analytics window.
	oldAudit := makePartition(t, pool, "audit_logs", 2024, time.January)

	// A row in the doomed partition, to show the data goes with it.
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO click_events (id, link_id, workspace_id, occurred_at)
		VALUES ($1, $2, $3, '2024-01-15 10:00:00+00')`,
		uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())); err != nil {
		t.Fatalf("seed old click: %v", err)
	}

	dropped, err := store.DropExpiredPartitions(t.Context(), pool, analyticsOnly(30), now)
	if err != nil {
		t.Fatalf("DropExpiredPartitions: %v", err)
	}

	if len(dropped) != 2 {
		t.Errorf("dropped %v, want exactly the two 2024-01 analytics partitions", dropped)
	}
	for _, name := range []string{old, oldVisitors} {
		if tableExists(t, pool, name) {
			t.Errorf("%s still exists; it is entirely outside the retention window", name)
		}
	}
	if !tableExists(t, pool, edge) {
		t.Error("click_events_2026_06 was dropped, but part of it is inside the window")
	}
	if !tableExists(t, pool, oldAudit) {
		t.Error("audit_logs_2024_01 was dropped by the analytics retention window; " +
			"audit retention is a separate policy")
	}
	// The default partition holds anything outside every explicit range, so its
	// contents cannot be dated — and it is the safety net that keeps a misrouted
	// click recoverable.
	if !tableExists(t, pool, "click_events_default") {
		t.Error("the default partition was dropped")
	}

	// Rollups are separate, unpartitioned tables: charts outlive raw events.
	if !tableExists(t, pool, "link_click_daily") {
		t.Error("a rollup table was dropped")
	}
}

// audit_logs joins partition retention under its own window. This is the
// partition-drop test M21 requires, and it asserts both halves of the claim in
// one run: the audit partition goes under the audit window, and the analytics
// partitions beside it do not, because their own window is longer.
func TestAuditRetentionUsesItsOwnWindow(t *testing.T) {
	pool := newDB(t)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	oldAudit := makePartition(t, pool, "audit_logs", 2024, time.January)
	oldClicks := makePartition(t, pool, "click_events", 2024, time.January)

	// A record in the doomed partition, so the test shows history actually
	// leaving rather than an empty table being renamed.
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO audit_logs (id, occurred_at, actor_label, action)
		VALUES ($1, '2024-01-15 10:00:00+00', 'gone@example.com', 'domain.root_redirect_changed')`,
		uuid.Must(uuid.NewV7())); err != nil {
		t.Fatalf("seed old audit record: %v", err)
	}

	// Audit keeps 30 days; analytics keeps ten years. Deliberately the reverse
	// of the shipped defaults, so a policy that quietly used one number for both
	// tables cannot pass by coincidence.
	dropped, err := store.DropExpiredPartitions(t.Context(), pool,
		store.NewRetentionPolicy(3650, 30), now)
	if err != nil {
		t.Fatalf("DropExpiredPartitions: %v", err)
	}

	if len(dropped) != 1 || dropped[0] != oldAudit {
		t.Fatalf("dropped %v, want exactly %s", dropped, oldAudit)
	}
	if tableExists(t, pool, oldAudit) {
		t.Error("audit_logs_2024_01 survived a 30-day audit window")
	}
	if !tableExists(t, pool, oldClicks) {
		t.Error("click_events_2024_01 was dropped by the audit window; the two " +
			"policies are separate and analytics kept ten years here")
	}
}

// The default: nothing configured, nothing deleted. This is decision D5 as a
// partition-level assertion — an upgrade must never silently start expiring
// audit history, and 0 is what an untouched instance runs with.
func TestAuditRetentionOfZeroKeepsEverything(t *testing.T) {
	pool := newDB(t)
	old := makePartition(t, pool, "audit_logs", 2018, time.January)

	// Analytics retention is aggressive and audit is left at its default, which
	// is the shipped configuration. The audit partition must be untouched even
	// though it is a decade outside the analytics window.
	dropped, err := store.DropExpiredPartitions(t.Context(), pool,
		store.NewRetentionPolicy(30, 0), time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("DropExpiredPartitions: %v", err)
	}
	for _, name := range dropped {
		if name == old {
			t.Fatal("an audit partition was dropped with AUDIT_RETENTION_DAYS=0; " +
				"0 means keep forever, and this is history an operator assumed permanent")
		}
	}
	if !tableExists(t, pool, old) {
		t.Error("audit_logs_2018_01 was dropped with audit retention disabled")
	}
}

// The size metric is what makes keep-forever defensible, so it has to report
// something real. A gauge stuck at zero would leave an operator believing an
// unbounded table was empty.
func TestAuditLogBytesReportsRealSize(t *testing.T) {
	pool := newDB(t)

	before, err := store.PartitionedTableBytes(t.Context(), pool, store.AuditTable)
	if err != nil {
		t.Fatalf("PartitionedTableBytes: %v", err)
	}

	for i := range 500 {
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO audit_logs (id, occurred_at, actor_label, action, metadata)
			VALUES ($1, now(), 'someone@example.com', 'domain.root_redirect_changed', $2::jsonb)`,
			uuid.Must(uuid.NewV7()),
			fmt.Sprintf(`{"from":"https://example.com/%d","to":"https://example.com/%d"}`, i, i+1),
		); err != nil {
			t.Fatalf("seed audit record %d: %v", i, err)
		}
	}
	// Sizes come from the catalogue, which lags until the relation's stats are
	// flushed; the write itself extends the file, so this is exact rather than
	// estimated, but the table must be analyzed for the space to be attributed.
	if _, err := pool.Exec(t.Context(), "VACUUM ANALYZE audit_logs"); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	after, err := store.PartitionedTableBytes(t.Context(), pool, store.AuditTable)
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Errorf("audit log size went from %d to %d bytes after 500 inserts; the "+
			"growth metric is not measuring the partitions", before, after)
	}
}

func TestRetentionOfZeroKeepsEverything(t *testing.T) {
	pool := newDB(t)
	old := makePartition(t, pool, "click_events", 2020, time.January)

	dropped, err := store.DropExpiredPartitions(t.Context(), pool, store.NewRetentionPolicy(0, 0), time.Now())
	if err != nil {
		t.Fatalf("DropExpiredPartitions: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped %v with retention disabled", dropped)
	}
	if !tableExists(t, pool, old) {
		t.Error("a partition was dropped with ANALYTICS_RETENTION_DAYS=0, which means keep forever")
	}
}

// A partition this code did not create is not one it should delete. An operator
// who attached a table by hand had a reason for it.
func TestRetentionIgnoresPartitionsItDoesNotRecognise(t *testing.T) {
	pool := newDB(t)

	// Two shapes of hand-attached partition: one whose name does not match the
	// generated pattern at all, and one that DOES match the _YYYY_MM pattern but
	// with a prefix that is not the parent table. The second is the trap — it
	// was droppable until the prefix comparison existed.
	for name, bounds := range map[string][2]string{
		"click_events_archive_2019":   {"2019-01-01", "2019-02-01"},
		"click_events_backup_2024_01": {"2019-02-01", "2019-03-01"},
		"click_events_import_2020_06": {"2019-03-01", "2019-04-01"},
	} {
		if _, err := pool.Exec(t.Context(), fmt.Sprintf(
			`CREATE TABLE %s PARTITION OF click_events
			 FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`,
			name, bounds[0], bounds[1])); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.DropExpiredPartitions(t.Context(), pool, analyticsOnly(30),
		time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("DropExpiredPartitions: %v", err)
	}
	for _, name := range []string{
		"click_events_archive_2019", "click_events_backup_2024_01", "click_events_import_2020_06",
	} {
		if !tableExists(t, pool, name) {
			t.Errorf("%s was dropped; only partitions matching the generated naming "+
				"scheme — parent table prefix included — are ours to delete", name)
		}
	}
}

// EnsurePartitionRange must cover every month from `from` to `to` inclusive,
// across a year boundary — the December→January rollover is where month
// arithmetic goes wrong, and a missed month silently routes rows to the
// default partition where they block that month's real partition forever.
func TestEnsurePartitionRangeCoversYearRollover(t *testing.T) {
	pool := newDB(t)
	ctx := t.Context()

	from := time.Date(2030, time.November, 15, 8, 0, 0, 0, time.UTC)
	to := time.Date(2031, time.February, 3, 8, 0, 0, 0, time.UTC)
	if _, err := store.EnsurePartitionRange(ctx, pool, from, to); err != nil {
		t.Fatalf("EnsurePartitionRange: %v", err)
	}

	for _, want := range []string{
		"click_events_2030_11", "click_events_2030_12",
		"click_events_2031_01", "click_events_2031_02",
		"visitors_2030_12", "audit_logs_2031_01",
	} {
		if !tableExists(t, pool, want) {
			t.Errorf("partition %s was not created", want)
		}
	}

	// A row on the first instant of the rollover month lands in an explicit
	// partition, not the default — the failure mode the default partition
	// exists to catch, and the one that blocks attaching the real partition.
	if _, err := pool.Exec(ctx, `
		INSERT INTO click_events (id, link_id, workspace_id, occurred_at)
		VALUES ($1, $2, $3, '2031-01-01 00:00:00+00')`,
		uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	var inDefault int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM click_events_default`).Scan(&inDefault); err != nil {
		t.Fatal(err)
	}
	if inDefault != 0 {
		t.Errorf("%d rows landed in the default partition; the explicit range has a gap", inDefault)
	}

	// Idempotent: a second call creates nothing.
	if n, err := store.EnsurePartitionRange(ctx, pool, from, to); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("second run created %d partitions, want 0", n)
	}
}

func TestRetentionIsIdempotent(t *testing.T) {
	pool := newDB(t)
	makePartition(t, pool, "click_events", 2024, time.January)
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)

	first, err := store.DropExpiredPartitions(t.Context(), pool, analyticsOnly(30), now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.DropExpiredPartitions(t.Context(), pool, analyticsOnly(30), now)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first run dropped nothing")
	}
	if len(second) != 0 {
		t.Errorf("second run dropped %v; the job runs hourly and must be a no-op "+
			"once there is nothing expired", second)
	}
}

// GeoIP enrichment, end to end: the country is resolved from the address at
// ingest, and the address itself still never reaches a column.
func TestClickEventsCarryAResolvedCountry(t *testing.T) {
	pool := newDB(t)

	geo, err := geoip.Open(filepath.Join("..", "..", "internal", "geoip", "testdata", "country-test.mmdb"))
	if err != nil {
		t.Fatalf("open test geoip database: %v", err)
	}
	t.Cleanup(func() { _ = geo.Close() })

	ingester := analytics.NewIngester(pool, analytics.NewSaltCache(pool), analytics.IngestConfig{
		QueueSize: 16, BatchSize: 4, FlushInterval: 10 * time.Millisecond,
		Geo: geo,
	})
	ingester.Start()

	linkID, workspaceID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, ip := range []string{"203.0.113.5", "198.51.100.7", "8.8.8.8"} {
		ingester.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: workspaceID, OccurredAt: time.Now(),
			IP: mustAddr(t, ip), UserAgent: "Mozilla/5.0", Language: "en-GB",
		})
	}
	if err := ingester.Close(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	rows, err := pool.Query(t.Context(),
		`SELECT coalesce(country, '-') FROM click_events WHERE link_id = $1 ORDER BY 1`, linkID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// '-' stands in for NULL: an address the database does not know produces no
	// country rather than a placeholder that would look like data.
	want := []string{"-", "DE", "GB"}
	if len(got) != len(want) {
		t.Fatalf("countries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("countries = %v, want %v", got, want)
		}
	}

	// Region and city are resolvable from the same database and deliberately not
	// stored. If this starts failing, something began collecting them.
	var stored int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM click_events
		  WHERE link_id = $1 AND (region IS NOT NULL OR city IS NOT NULL)`,
		linkID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("%d rows carry a region or city; only country is stored", stored)
	}
}

// Without a database configured, the column stays null and nothing fails.
func TestClickEventsWithoutGeoIPHaveNoCountry(t *testing.T) {
	pool := newDB(t)

	ingester := analytics.NewIngester(pool, analytics.NewSaltCache(pool), analytics.IngestConfig{
		QueueSize: 8, BatchSize: 2, FlushInterval: 10 * time.Millisecond,
	})
	ingester.Start()

	linkID := uuid.Must(uuid.NewV7())
	ingester.Record(analytics.Event{
		LinkID: linkID, WorkspaceID: uuid.Must(uuid.NewV7()), OccurredAt: time.Now(),
		IP: mustAddr(t, "203.0.113.5"), UserAgent: "Mozilla/5.0",
	})
	if err := ingester.Close(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var nulls int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM click_events WHERE link_id = $1 AND country IS NULL`,
		linkID).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 1 {
		t.Errorf("%d null countries, want 1: with no database the column stays null", nulls)
	}
}
