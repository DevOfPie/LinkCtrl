//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newIngester(t *testing.T, pool *pgxpool.Pool, cfg analytics.IngestConfig) *analytics.Ingester {
	t.Helper()
	cfg.Logger = quietLogger()
	ing := analytics.NewIngester(pool, analytics.NewSaltCache(pool), cfg)
	ing.Start()
	return ing
}

// seedLink creates a link directly, bypassing the API, so analytics tests do
// not depend on the HTTP layer.
func seedLink(t *testing.T, pool *pgxpool.Pool) (linkID, workspaceID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	orgID := uuid.Must(uuid.NewV7())
	wsID := uuid.Must(uuid.NewV7())
	lnkID := uuid.Must(uuid.NewV7())

	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, 'T', $2)`,
		orgID, "org-"+orgID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, organization_id, name, slug) VALUES ($1, $2, 'W', 'default')`,
		wsID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO links (id, workspace_id, domain_id, alias, primary_url, status)
		VALUES ($1, $2, (SELECT id FROM domains WHERE is_default), $3, 'https://example.com/x', 'active')`,
		lnkID, wsID, "a"+lnkID.String()[:6]); err != nil {
		t.Fatal(err)
	}
	return lnkID, wsID
}

// TestNoIPAddressIsEverStored is the privacy guarantee, asserted against the
// live schema rather than trusted from the code.
//
// It checks two things: that click_events has no column capable of holding an
// address, and that no text column contains one after ingest. The first would
// catch someone adding a column; the second would catch an address smuggled
// into an existing field.
func TestNoIPAddressIsEverStored(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{BatchSize: 10, FlushInterval: 20 * time.Millisecond})

	const secret = "203.0.113.42"
	for i := 0; i < 20; i++ {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: time.Now(),
			IP:        netip.MustParseAddr(secret),
			UserAgent: "Mozilla/5.0 Chrome/120",
			Referrer:  "https://referrer.example/path?token=abc",
		})
	}
	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// No column of an address-bearing type.
	var addrCols int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'click_events'
		  AND (data_type IN ('inet','cidr') OR column_name ILIKE '%ip%' OR column_name ILIKE '%addr%')`,
	).Scan(&addrCols); err != nil {
		t.Fatal(err)
	}
	if addrCols != 0 {
		t.Errorf("click_events has %d address-shaped columns; the table must hold none", addrCols)
	}

	// And no text column contains the address.
	var found int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM click_events
		WHERE coalesce(country,'')||coalesce(region,'')||coalesce(city,'')
		   || coalesce(device,'')||coalesce(browser,'')||coalesce(os,'')
		   || coalesce(language,'')||coalesce(referrer_host,'') LIKE '%' || $1 || '%'`,
		secret).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found != 0 {
		t.Errorf("%d rows contain the client address in a text column", found)
	}

	// The referrer must be reduced to its host: full referrers routinely carry
	// tokens in the query string.
	var referrer *string
	if err := pool.QueryRow(ctx,
		"SELECT referrer_host FROM click_events LIMIT 1").Scan(&referrer); err != nil {
		t.Fatal(err)
	}
	if referrer == nil || *referrer != "referrer.example" {
		t.Errorf("referrer_host = %v, want just the host with the query string discarded", referrer)
	}
}

func TestIngestWritesEveryEventAndBumpsTheCounter(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{
		QueueSize: 20000, BatchSize: 500, FlushInterval: 50 * time.Millisecond,
	})

	const total = 10_000
	for i := 0; i < total; i++ {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: time.Now(),
			IP: netip.MustParseAddr("198.51.100.1"), UserAgent: "Mozilla/5.0 Chrome/120",
		})
	}
	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	if dropped := ing.Stats.Dropped.Load(); dropped != 0 {
		t.Errorf("%d events dropped with a queue larger than the batch", dropped)
	}

	var rows int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM click_events WHERE link_id = $1", linkID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != total {
		t.Errorf("%d click rows, want %d", rows, total)
	}

	// The counter is updated in the same transaction as the rows, so the two
	// must agree exactly.
	var counted int64
	if err := pool.QueryRow(ctx, "SELECT click_count FROM links WHERE id = $1", linkID).Scan(&counted); err != nil {
		t.Fatal(err)
	}
	if counted != total {
		t.Errorf("links.click_count = %d, want %d", counted, total)
	}
}

// TestFullQueueDropsRatherThanBlocks is the contract that protects the hot
// path: analytics must degrade, never apply backpressure to a redirect.
func TestFullQueueDropsRatherThanBlocks(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	// A queue of one, so it fills immediately.
	ing := newIngester(t, pool, analytics.IngestConfig{
		QueueSize: 1, BatchSize: 1, FlushInterval: time.Hour,
	})

	const attempts = 5000
	start := time.Now()
	for i := 0; i < attempts; i++ {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: time.Now(),
			IP: netip.MustParseAddr("198.51.100.1"), UserAgent: "curl/8",
		})
	}
	elapsed := time.Since(start)

	// The whole point: Record never blocks. 5000 calls against a full queue
	// should take microseconds, not seconds.
	if elapsed > 500*time.Millisecond {
		t.Errorf("%d Record calls took %s against a full queue; the hot path was blocked",
			attempts, elapsed)
	}

	dropped := ing.Stats.Dropped.Load()
	if dropped == 0 {
		t.Error("no drops recorded with a queue of one; drops must be counted, not silent")
	}
	if enq := ing.Stats.Enqueued.Load(); enq+dropped != attempts {
		t.Errorf("enqueued %d + dropped %d != %d attempted; events vanished unaccounted for",
			enq, dropped, attempts)
	}
	_ = ing.Close(ctx)
}

// TestCloseFlushesBufferedEvents: without this every restart silently loses up
// to a full batch.
func TestCloseFlushesBufferedEvents(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	// A long flush interval, so nothing is written before Close.
	ing := newIngester(t, pool, analytics.IngestConfig{
		QueueSize: 1000, BatchSize: 1000, FlushInterval: time.Hour,
	})

	const n = 250
	for i := 0; i < n; i++ {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: time.Now(),
			IP: netip.MustParseAddr("198.51.100.2"), UserAgent: "Mozilla/5.0",
		})
	}

	var before int64
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM click_events").Scan(&before)
	if before != 0 {
		t.Fatalf("%d rows written before Close; the test cannot prove the flush", before)
	}

	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	var after int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM click_events").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != n {
		t.Errorf("%d rows after Close, want %d; buffered events were lost on shutdown", after, n)
	}
}

func TestRecordAfterCloseIsSafe(t *testing.T) {
	pool := newDB(t)
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{BatchSize: 10, FlushInterval: 10 * time.Millisecond})
	if err := ing.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A late redirect finishing during shutdown must not panic on a closed
	// channel.
	ing.Record(analytics.Event{LinkID: linkID, WorkspaceID: wsID, OccurredAt: time.Now()})
}

// TestSaltRotationChangesVisitorHashes exercises the mechanism end to end
// against real rows.
func TestSaltRotationChangesVisitorHashes(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{BatchSize: 1, FlushInterval: 10 * time.Millisecond})

	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	today := time.Now().UTC()

	for _, at := range []time.Time{yesterday, today} {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: at,
			IP: netip.MustParseAddr("203.0.113.99"), UserAgent: "Mozilla/5.0 Chrome/120",
		})
	}
	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	var distinct int
	if err := pool.QueryRow(ctx,
		"SELECT count(DISTINCT visitor_hash) FROM click_events WHERE link_id = $1", linkID).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 2 {
		t.Errorf("%d distinct visitor hashes across two days, want 2; the salt is not rotating "+
			"and a visitor would be trackable indefinitely", distinct)
	}

	// Two salts should exist, each with a purge deadline.
	var salts int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM analytics_salts WHERE purge_at IS NOT NULL").Scan(&salts)
	if salts != 2 {
		t.Errorf("%d salts with a purge deadline, want 2", salts)
	}
}

// The dimension rollup computes six breakdowns in one pass over click_events.
// This checks that pass against a per-dimension aggregate written the other way
// round — one query per dimension, the shape the rollup used to have — so the
// two implementations have to agree about clicks, uniques, bot exclusion and
// every coalesce rule.
//
// Without this, rewriting that query for speed is a change nothing verifies, and
// a wrong breakdown looks exactly like a right one on a chart.
func TestDimensionRollupMatchesAPerDimensionAggregate(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{BatchSize: 50, FlushInterval: 20 * time.Millisecond})
	now := time.Now().UTC()

	// A spread wide enough that every dimension has several distinct values, and
	// some are empty so the coalesce rules are exercised rather than assumed.
	agents := []string{
		"Mozilla/5.0 (Windows NT 10.0) Chrome/120",
		"Mozilla/5.0 (Macintosh) Safari/17",
		"Mozilla/5.0 (iPhone) Safari/17",
		"Mozilla/5.0 (Linux; Android 14) Chrome/120",
		"curl/8.0",
	}
	languages := []string{"en-GB", "de", "", "ja-JP"}
	referrers := []string{"https://news.example.com/a?x=1", "", "https://t.co/abc"}

	for i := range 300 {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: now,
			IP:        netip.MustParseAddr("198.51.100." + itoa(i%17)),
			UserAgent: agents[i%len(agents)],
			Language:  languages[i%len(languages)],
			Referrer:  referrers[i%len(referrers)],
		})
	}
	// Bots must be excluded from every dimension, not only from the totals.
	for i := range 40 {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: now,
			IP:        netip.MustParseAddr("203.0.113." + itoa(i%4)),
			UserAgent: "Googlebot/2.1",
			Language:  "en",
		})
	}
	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	if err := analytics.NewRoller(pool, quietLogger()).RunRecent(ctx, now); err != nil {
		t.Fatal(err)
	}

	// expr is how the rollup derives each dimension's value. One query per
	// dimension here, deliberately: the rollup does all six at once, so agreeing
	// with six separate aggregates is evidence about the combining, not a
	// restatement of it.
	dimensions := map[string]string{
		"device":   `coalesce(device, 'unknown')`,
		"browser":  `coalesce(browser, 'Other')`,
		"os":       `coalesce(os, 'Other')`,
		"country":  `coalesce(country, 'unknown')`,
		"referrer": `coalesce(nullif(referrer_host, ''), 'direct')`,
		"language": `coalesce(nullif(language, ''), 'unknown')`,
	}

	type agg struct{ clicks, uniques int64 }

	for dim, expr := range dimensions {
		want := map[string]agg{}
		rows, err := pool.Query(ctx, `
			SELECT `+expr+` AS value, count(*), count(DISTINCT visitor_hash)
			  FROM click_events
			 WHERE link_id = $1 AND NOT is_bot
			 GROUP BY 1`, linkID)
		if err != nil {
			t.Fatalf("%s: expectation query: %v", dim, err)
		}
		for rows.Next() {
			var v string
			var a agg
			if err := rows.Scan(&v, &a.clicks, &a.uniques); err != nil {
				t.Fatal(err)
			}
			want[v] = a
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(want) == 0 {
			t.Fatalf("%s: the fixture produced no values to compare", dim)
		}

		got := map[string]agg{}
		rows, err = pool.Query(ctx, `
			SELECT value, clicks, unique_visitors
			  FROM link_dimension_daily
			 WHERE link_id = $1 AND dimension = $2`, linkID, dim)
		if err != nil {
			t.Fatalf("%s: rollup query: %v", dim, err)
		}
		for rows.Next() {
			var v string
			var a agg
			if err := rows.Scan(&v, &a.clicks, &a.uniques); err != nil {
				t.Fatal(err)
			}
			got[v] = a
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}

		if len(got) != len(want) {
			t.Errorf("%s: rollup has %d values, aggregate has %d\ngot:  %v\nwant: %v",
				dim, len(got), len(want), got, want)
		}
		for v, w := range want {
			g, ok := got[v]
			if !ok {
				t.Errorf("%s: rollup is missing value %q", dim, v)
				continue
			}
			if g != w {
				t.Errorf("%s[%q] = %+v, want %+v", dim, v, g, w)
			}
		}
	}
}

func TestRollupIsIdempotent(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{BatchSize: 100, FlushInterval: 20 * time.Millisecond})
	now := time.Now().UTC()

	// Human traffic: 240 clicks from 10 distinct addresses.
	//
	// Bots use a separate address range on purpose. An earlier version derived
	// bot-ness from the same index as the address, which made two addresses
	// permanently bots and left only 8 non-bot uniques — the test then failed
	// against correct code. Keeping the dimensions independent means the
	// expected numbers follow from the fixture rather than from the output.
	for i := 0; i < 240; i++ {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: now,
			IP:        netip.MustParseAddr("198.51.100." + itoa(i%10)),
			UserAgent: "Mozilla/5.0 Chrome/120",
		})
	}
	// Bot traffic: 60 clicks, counted separately and excluded from clicks.
	for i := 0; i < 60; i++ {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID, OccurredAt: now,
			IP:        netip.MustParseAddr("203.0.113." + itoa(i%4)),
			UserAgent: "Googlebot/2.1",
		})
	}
	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	roller := analytics.NewRoller(pool, quietLogger())
	if err := roller.RunRecent(ctx, now); err != nil {
		t.Fatal(err)
	}

	read := func() (clicks, uniques, bots int64) {
		if err := pool.QueryRow(ctx,
			"SELECT clicks, unique_visitors, bot_clicks FROM link_click_daily WHERE link_id = $1",
			linkID).Scan(&clicks, &uniques, &bots); err != nil {
			t.Fatal(err)
		}
		return
	}

	c1, u1, b1 := read()
	if c1 != 240 {
		t.Errorf("clicks = %d, want 240 non-bot of 300", c1)
	}
	if b1 != 60 {
		t.Errorf("bot_clicks = %d, want 60", b1)
	}
	if u1 != 10 {
		t.Errorf("unique_visitors = %d, want 10 distinct addresses", u1)
	}

	// Running again must converge, not accumulate. An incremental design
	// would double every figure here.
	if err := roller.RunRecent(ctx, now); err != nil {
		t.Fatal(err)
	}
	c2, u2, b2 := read()
	if c1 != c2 || u1 != u2 || b1 != b2 {
		t.Errorf("rollup is not idempotent: (%d,%d,%d) then (%d,%d,%d)", c1, u1, b1, c2, u2, b2)
	}
}

func TestRollupMatchesABruteForceAggregate(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{BatchSize: 200, FlushInterval: 20 * time.Millisecond})
	now := time.Now().UTC()
	for i := 0; i < 500; i++ {
		ing.Record(analytics.Event{
			LinkID: linkID, WorkspaceID: wsID,
			OccurredAt: now.Add(-time.Duration(i) * time.Second),
			IP:         netip.MustParseAddr("198.51.100." + itoa(i%25)),
			UserAgent:  "Mozilla/5.0 Chrome/120",
		})
	}
	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	if err := analytics.NewRoller(pool, quietLogger()).RunRecent(ctx, now); err != nil {
		t.Fatal(err)
	}

	// The rollup must agree with counting the raw events directly. This is
	// what makes the dashboard trustworthy despite never reading them.
	var rollupClicks, rawClicks int64
	_ = pool.QueryRow(ctx,
		"SELECT coalesce(sum(clicks),0) FROM link_click_daily WHERE link_id = $1", linkID).Scan(&rollupClicks)
	_ = pool.QueryRow(ctx,
		"SELECT count(*) FROM click_events WHERE link_id = $1 AND NOT is_bot", linkID).Scan(&rawClicks)

	if rollupClicks != rawClicks {
		t.Errorf("rollup says %d clicks, raw events say %d", rollupClicks, rawClicks)
	}
}

func TestSaltPurgeRemovesExpiredSalts(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	salts := analytics.NewSaltCache(pool)

	// A salt already past its purge deadline.
	if _, err := pool.Exec(ctx, `
		INSERT INTO analytics_salts (valid_on, salt, purge_at)
		VALUES (current_date - 10, '\x00112233', now() - interval '1 day')`); err != nil {
		t.Fatal(err)
	}

	n, err := salts.Purge(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("purged %d salts, want 1", n)
	}

	var remaining int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM analytics_salts WHERE purge_at < now()").Scan(&remaining)
	if remaining != 0 {
		t.Errorf("%d expired salts remain; the de-identification step did not run", remaining)
	}
}

func TestClicksLandInTheCorrectPartition(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	linkID, wsID := seedLink(t, pool)

	ing := newIngester(t, pool, analytics.IngestConfig{BatchSize: 1, FlushInterval: 10 * time.Millisecond})
	now := time.Now().UTC()
	ing.Record(analytics.Event{
		LinkID: linkID, WorkspaceID: wsID, OccurredAt: now,
		IP: netip.MustParseAddr("198.51.100.1"), UserAgent: "Mozilla/5.0",
	})
	if err := ing.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Nothing may fall into the default partition: a row there means the
	// month's partition was missing, and attaching it later would then fail.
	var inDefault int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM click_events_default").Scan(&inDefault); err != nil {
		t.Fatal(err)
	}
	if inDefault != 0 {
		t.Errorf("%d rows landed in the default partition; the monthly partition was missing", inDefault)
	}
}
