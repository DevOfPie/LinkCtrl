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

	dropped, err := store.DropExpiredPartitions(t.Context(), pool, 30, now)
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

func TestRetentionOfZeroKeepsEverything(t *testing.T) {
	pool := newDB(t)
	old := makePartition(t, pool, "click_events", 2020, time.January)

	dropped, err := store.DropExpiredPartitions(t.Context(), pool, 0, time.Now())
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

	const name = "click_events_archive_2019"
	if _, err := pool.Exec(t.Context(), fmt.Sprintf(
		`CREATE TABLE %s PARTITION OF click_events
		 FOR VALUES FROM ('2019-01-01 00:00:00+00') TO ('2019-02-01 00:00:00+00')`, name)); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DropExpiredPartitions(t.Context(), pool, 30,
		time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("DropExpiredPartitions: %v", err)
	}
	if !tableExists(t, pool, name) {
		t.Errorf("%s was dropped; only partitions matching the generated naming "+
			"scheme are ours to delete", name)
	}
}

func TestRetentionIsIdempotent(t *testing.T) {
	pool := newDB(t)
	makePartition(t, pool, "click_events", 2024, time.January)
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)

	first, err := store.DropExpiredPartitions(t.Context(), pool, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.DropExpiredPartitions(t.Context(), pool, 30, now)
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
