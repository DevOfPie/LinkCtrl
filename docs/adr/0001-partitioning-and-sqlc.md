# ADR 0001: Analytics partitioning and how sqlc sees it

Status: accepted, 2026-07-29
Verified empirically against PostgreSQL 17.10 and sqlc v1.31.1 before any
schema work (milestone M0.5).

## Context

`click_events` is the largest table in the system and is range-partitioned by
month. The schema is also the input to `sqlc`, which generates the database
layer. Before committing to ~1400 lines of DDL written against assumptions
about how those two interact, the assumptions were tested.

## Findings

**1. sqlc parses a partitioned parent without complaint.**
`CREATE TABLE ... PARTITION BY RANGE (occurred_at)` generates a correct
`ClickEvent` model. The concern that it would fail outright was unfounded.

**2. But sqlc emits a junk model for every child partition.**
With `CREATE TABLE click_events_2026_08 PARTITION OF click_events ...` in the
schema, sqlc generated `ClickEvents202608` and `ClickEventsDefault` structs —
full duplicates of `ClickEvent`. Left unchecked this means the generated code
grows a new dead struct every month, and `make generate` produces a diff on a
schedule rather than in response to a change.

Consequence, and the actual rule: **`PARTITION OF` statements must not appear in
the SQL that sqlc reads.** Partitions are created by a Go migration and by the
`partition_maintain` runtime job, never in the declarative schema. Removing them
leaves exactly `ClickEvent` and `Link`.

This is the same conclusion the plan reached, but for a different reason than
expected — the failure is silent generated-code churn, not a parse error, which
is why it needed testing rather than assuming.

**3. Nullable columns need their own type overrides.**
An override of `db_type: timestamptz -> time.Time` applies only to the NOT NULL
case; nullable columns fell back to `pgtype.Timestamptz` even with
`emit_pointers_for_null_types: true`, because the explicit override shadows it.
Both forms must be declared:

```yaml
- db_type: timestamptz
  go_type: time.Time
- db_type: timestamptz
  nullable: true
  go_type: { type: "time.Time", pointer: true }
```

That yields `ExpiresAt *time.Time` rather than a pgtype wrapper leaking into
every domain mapping.

**4. `sqlc vet` with `sqlc/db-prepare` works against a live database.**
Every query is PREPAREd server-side, so a typo'd column or an invalid cast fails
the build with no test written. This is wired into CI as its own job.

**5. Partition bounds are resolved using the session TimeZone at DDL time.**
This is the one that would have caused a genuinely baffling production
incident. The identical DDL text, executed under two different session
timezones:

```
tzprobe_utc | FOR VALUES FROM ('2026-08-01 00:00:00+00') TO ('2026-09-01 00:00:00+00')
tzprobe_ny  | FOR VALUES FROM ('2026-09-01 04:00:00+00') TO ('2026-10-01 04:00:00+00')
```

The August partition ends at 2026-09-01 00:00 UTC and the September partition
begins at 04:00 UTC. Four hours of click events fall into the gap — routed to
the default partition if one exists, rejected outright if not, and in either
case invisible until someone investigates a hole in the analytics.

## Decisions

1. `PARTITION OF` never appears in `internal/store/migrations/*.sql`. Partition
   creation lives in a Go migration and the `partition_maintain` job.
2. UTC is enforced in three places, none of which is sufficient alone: the pgx
   pool's `RuntimeParams`, the Postgres server (`-c timezone=UTC`), and the
   container environment (`TZ=UTC`). A test asserts the session timezone is UTC.
3. Both nullable and non-nullable overrides are declared for every overridden
   type in `sqlc.yaml`.
4. `sqlc vet` runs in CI against a migrated database.
5. Never write `occurred_at::date`; always `occurred_at AT TIME ZONE 'UTC'`.
   Mixing the two produces off-by-one-day analytics.

## Reproducing

The spike was run in a scratch directory against a throwaway
`postgres:17-alpine` container and deliberately not committed. The findings
above are the durable part; re-running it requires only the schema fragment,
one query file, and `sqlc generate`.
