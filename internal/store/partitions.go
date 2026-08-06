package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PartitionedTables are the RANGE-partitioned tables. All are keyed on a
// timestamptz and partitioned by month.
//
// Maintaining partitions for a table nothing writes to yet is deliberate rather
// than an oversight, and it paid off here. audit_logs was maintained through the
// whole of Phase 1 with no writer; when M21 gave it one, partitions already
// existed for every month and no backfill was needed. `visitors` is still in
// that position. The cost is one to_regclass check per table per month — the
// partition already exists on all but one run an hour. The alternative fails in
// the direction that matters: rows landing in the default partition, which
// retention never drops, so a dormant table would quietly become the one place
// data is kept forever.
var PartitionedTables = []string{"click_events", "visitors", "audit_logs"}

// EnsurePartitions creates monthly partitions for the current month and the
// next `ahead` months, plus a default partition per table. It reports how many
// it created and is safe to call repeatedly.
//
// Two things here are load-bearing.
//
// The session timezone is pinned to UTC for the DDL. Bounds on a timestamptz
// column resolve against the session timezone at DDL time, so the identical
// bound literal produces a different absolute range under a different
// timezone, leaving either a gap that silently routes rows to the default
// partition or an overlap that makes attaching fail. Demonstrated in
// docs/adr/0001-partitioning-and-sqlc.md.
//
// It looks more than one month ahead. Creating next month's partition on the
// last day of this one is a single point of failure with a hard deadline; two
// months of headroom turns a missed run into a warning rather than an outage.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, ahead int) (int, error) {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return EnsurePartitionRange(ctx, pool, start, start.AddDate(0, ahead, 0))
}

// EnsurePartitionRange creates monthly partitions covering every month from
// `from` to `to` inclusive, plus a default partition per table.
//
// Separate from EnsurePartitions because the months that need to exist are not
// always the ones around today: restoring a backup and seeding a load-test
// dataset both write into the past, and an insert with no matching partition
// lands in the default one, where it silently blocks attaching the partition
// that should have held it.
func EnsurePartitionRange(ctx context.Context, pool *pgxpool.Pool, from, to time.Time) (int, error) {
	from = monthStart(from)
	to = monthStart(to)

	created := 0
	for _, table := range PartitionedTables {
		// The default partition catches anything outside every explicit range.
		// Without it, a missing partition means the insert fails and the click
		// is lost. With it, the row lands somewhere recoverable. The trade-off
		// is that attaching a new partition scans the default and fails if it
		// holds conflicting rows, which is why the maintenance job alerts when
		// the default is non-empty.
		ok, err := ensurePartition(ctx, pool, table, table+"_default", time.Time{}, time.Time{})
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}

		for m := from; !m.After(to); m = m.AddDate(0, 1, 0) {
			ok, err := ensurePartition(ctx, pool, table, PartitionName(table, m), m, m.AddDate(0, 1, 0))
			if err != nil {
				return created, err
			}
			if ok {
				created++
			}
		}
	}
	return created, nil
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// ensurePartition creates one partition if it does not exist, reporting
// whether it did. A zero `from` means the DEFAULT partition.
func ensurePartition(ctx context.Context, pool *pgxpool.Pool, table, name string, from, to time.Time) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check partition %s: %w", name, err)
	}
	if exists {
		return false, nil
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	// Belt and braces alongside the pool's RuntimeParams: this statement is
	// the one that actually depends on it.
	if _, err := conn.Exec(ctx, "SET timezone = 'UTC'"); err != nil {
		return false, fmt.Errorf("set timezone: %w", err)
	}

	var ddl string
	if from.IsZero() {
		ddl = fmt.Sprintf("CREATE TABLE %s PARTITION OF %s DEFAULT", name, table)
	} else {
		ddl = fmt.Sprintf(
			"CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
			name, table,
			from.Format("2006-01-02 15:04:05-07"),
			to.Format("2006-01-02 15:04:05-07"),
		)
	}

	if _, err := conn.Exec(ctx, ddl); err != nil {
		// A concurrent replica may have won the race; that is success, not
		// failure.
		var raced bool
		if e := conn.QueryRow(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&raced); e == nil && raced {
			return false, nil
		}
		return false, fmt.Errorf("create partition %s: %w", name, err)
	}
	return true, nil
}

// AnalyticsTables are the partitioned tables the analytics retention window
// applies to.
//
// audit_logs is partitioned identically and is deliberately not here. Audit
// retention is a different policy from analytics retention — the reason to keep
// an audit trail is that someone may need to ask what happened a long time
// afterwards — and quietly deleting it on the analytics setting would be a
// surprise of exactly the wrong kind. It has its own window; see
// RetentionPolicy.
var AnalyticsTables = []string{"click_events", "visitors"}

// AuditTable is the audit log, retained under its own window.
const AuditTable = "audit_logs"

// RetentionPolicy maps a partitioned table to the number of days its data is
// kept. Zero or less keeps that table forever, matching the configuration
// contract that 0 means "forever".
//
// A map rather than one number and a list of tables, because the two policies
// have different defaults and answer to different settings: 395 days from
// ANALYTICS_RETENTION_DAYS, and forever from AUDIT_RETENTION_DAYS. Expressing
// that as a single window over a table list was how audit_logs came to be
// exempt-by-omission, which worked only for as long as there was exactly one
// window.
type RetentionPolicy map[string]int

// NewRetentionPolicy builds the policy from the two configured windows.
func NewRetentionPolicy(analyticsDays, auditDays int) RetentionPolicy {
	p := RetentionPolicy{AuditTable: auditDays}
	for _, t := range AnalyticsTables {
		p[t] = analyticsDays
	}
	return p
}

// partitionMonth matches the names EnsurePartitions creates. Anything else is
// left alone: a partition this code did not create is not one it should drop.
var partitionMonth = regexp.MustCompile(`^(.+)_(\d{4})_(\d{2})$`)

// DropExpiredPartitions drops monthly partitions whose entire range is older
// than their table's retention window, and reports what it dropped.
//
// A window of zero or less keeps that table forever, matching the configuration
// contract that 0 means "forever". A table absent from the policy is never
// touched at all, which is what keeps a partitioned table added later from
// silently inheriting somebody else's window.
//
// Retention is enforced at month granularity, and only when the newest row a
// partition could hold is already outside the window. The alternative — deleting
// rows older than exactly N days — would mean a DELETE across the largest table
// in the system, then a VACUUM to reclaim the space, on a schedule. Dropping a
// partition is instant, reclaims the space immediately, and cannot half-finish.
// The cost is that data survives up to a month past the nominal window, which is
// the right way to be wrong: keeping data slightly too long is recoverable, and
// deleting it slightly too early is not.
//
// Daily rollups live in their own unpartitioned tables and are untouched, so
// historical charts keep working after the raw events are gone.
func DropExpiredPartitions(ctx context.Context, pool *pgxpool.Pool, policy RetentionPolicy, now time.Time) ([]string, error) {
	var dropped []string
	var errs []error
	// Iterated in a fixed order rather than over the map, so a run that drops
	// several tables' partitions logs them the same way every time.
	for _, table := range PartitionedTables {
		retentionDays, governed := policy[table]
		if !governed || retentionDays <= 0 {
			continue
		}
		cutoff := now.UTC().AddDate(0, 0, -retentionDays)

		names, err := expiredPartitions(ctx, pool, table, cutoff)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, name := range names {
			if err := dropPartition(ctx, pool, name); err != nil {
				// One partition failing must not stop the others. The usual cause
				// is a lock timeout, and the next run will pick it up.
				errs = append(errs, err)
				continue
			}
			dropped = append(dropped, name)
		}
	}
	return dropped, errors.Join(errs...)
}

// expiredPartitions lists the partitions of one table that are entirely older
// than the cutoff.
func expiredPartitions(ctx context.Context, pool *pgxpool.Pool, table string, cutoff time.Time) ([]string, error) {
	// Read from the catalogue rather than guessing which partitions exist, and
	// take the bound expression along so the DEFAULT partition can be excluded by
	// what it is rather than by what it is called.
	rows, err := pool.Query(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		  FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = $1
		   AND c.relispartition`, table)
	if err != nil {
		return nil, fmt.Errorf("list partitions of %s: %w", table, err)
	}
	defer rows.Close()

	var expired []string
	for rows.Next() {
		var name, bound string
		if err := rows.Scan(&name, &bound); err != nil {
			return nil, fmt.Errorf("scan partition of %s: %w", table, err)
		}
		// The default partition holds anything outside every explicit range, so
		// its contents cannot be dated from its bounds. Dropping it would also
		// remove the safety net that keeps a misrouted click recoverable.
		if strings.Contains(bound, "DEFAULT") {
			continue
		}
		m := partitionMonth.FindStringSubmatch(name)
		if m == nil {
			// Not a name this code created. Left alone deliberately: an operator's
			// hand-made partition is not ours to delete.
			continue
		}
		// The prefix must be exactly the parent table, not merely end in a
		// month. Without this, a hand-attached "click_events_backup_2024_01"
		// matches the pattern and gets dropped — precisely the partition the
		// rule above promises to leave alone.
		if m[1] != table {
			continue
		}
		year, _ := strconv.Atoi(m[2])
		month, _ := strconv.Atoi(m[3])
		if month < 1 || month > 12 {
			continue
		}
		// The instant after the last one the partition can hold. Comparing this
		// against the cutoff is what guarantees nothing inside the window goes.
		end := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		if !end.After(cutoff) {
			expired = append(expired, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list partitions of %s: %w", table, err)
	}
	return expired, nil
}

// dropPartition removes one partition under a short lock timeout.
//
// Detaching a partition needs an exclusive lock on the parent, which the click
// ingester holds briefly on every batch. Without a timeout the drop would sit in
// the lock queue — and everything arriving behind it would queue too, turning a
// housekeeping job into a stall on the write path. Five seconds is long enough
// to win the lock between batches and short enough that failing is cheap: the
// job runs again in an hour.
func dropPartition(ctx context.Context, pool *pgxpool.Pool, name string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin drop of %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return fmt.Errorf("set lock timeout: %w", err)
	}
	// Identifier interpolation, because a table name cannot be a bind parameter.
	// The value came from pg_class and matched partitionMonth, and Sanitize is the
	// belt to that braces.
	if _, err := tx.Exec(ctx, "DROP TABLE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("drop partition %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit drop of %s: %w", name, err)
	}
	return nil
}

// PartitionName returns the partition a timestamp belongs to.
func PartitionName(table string, at time.Time) string {
	at = at.UTC()
	return fmt.Sprintf("%s_%04d_%02d", table, at.Year(), int(at.Month()))
}

// PartitionedTableBytes reports the on-disk size of a partitioned table:
// every partition, including indexes and TOAST.
//
// This exists because the audit log's retention default is "keep forever", and
// that default is only defensible if the growth it permits is visible. An
// operator who never sets AUDIT_RETENTION_DAYS has chosen unbounded growth, and
// they should find that out from a graph rather than from a full disk.
//
// Summed over the partitions rather than read from the parent: a partitioned
// table has no storage of its own, so pg_total_relation_size on the parent
// answers 0 no matter how much data is underneath it.
//
// Catalogue and free-space-map arithmetic only — no scan of the table — so this
// stays cheap on the table it is most needed for.
func PartitionedTableBytes(ctx context.Context, pool *pgxpool.Pool, table string) (int64, error) {
	var bytes int64
	err := pool.QueryRow(ctx, `
		SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0)::bigint
		  FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = $1
		   AND c.relispartition`, table).Scan(&bytes)
	if err != nil {
		return 0, fmt.Errorf("size of %s: %w", table, err)
	}
	return bytes, nil
}

// DefaultPartitionCounts reports how many rows sit in each default partition.
//
// A non-zero count is an operational alert, not a curiosity: it means rows
// arrived outside every explicit range, and attaching the partition that
// should have held them will now fail until they are moved out.
func DefaultPartitionCounts(ctx context.Context, pool *pgxpool.Pool) (map[string]int64, error) {
	out := make(map[string]int64, len(PartitionedTables))
	for _, table := range PartitionedTables {
		name := table + "_default"
		var n int64
		err := pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", name)).Scan(&n)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", name, err)
		}
		out[table] = n
	}
	return out, nil
}
