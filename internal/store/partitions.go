package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PartitionedTables are the RANGE-partitioned tables. All are keyed on a
// timestamptz and partitioned by month.
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

		for i := 0; i <= ahead; i++ {
			from := start.AddDate(0, i, 0)
			to := from.AddDate(0, 1, 0)
			ok, err := ensurePartition(ctx, pool, table, PartitionName(table, from), from, to)
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

// PartitionName returns the partition a timestamp belongs to.
func PartitionName(table string, at time.Time) string {
	at = at.UTC()
	return fmt.Sprintf("%s_%04d_%02d", table, at.Year(), int(at.Month()))
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
