package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/platform/postgres"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// Seeding exists for one purpose: making the redirect SLO measurable. The target
// is defined against 100k links and 5M click events, and a load test on an empty
// database measures an empty database — index depth, planner choices and cache
// hit ratios all change with size, and every one of them is on the path being
// measured.
//
// Two simplifications are deliberate, and both are stated in the help text
// because a seeded row is not quite a real one:
//
// Links carry primary_url directly and have no destinations row. The redirect
// path reads links.primary_url and nothing else, so resolution is identical; what
// a seeded link cannot do is exercise the editing surface. Writing destinations
// would also fire the sync trigger 100,000 times, one single-row UPDATE each.
//
// Click events are generated, not replayed. Visitor hashes are random bytes
// rather than real HMACs, which is what a hash looks like to every query that
// reads it, and no analytics_salts rows are needed to produce them.
const (
	seedLinkBatch  = 10_000
	seedClickBatch = 50_000

	// Zipf-ish traffic: most clicks land on a small share of links. A uniform
	// distribution would make every cache tier behave identically and hide the
	// thing the SLO is about.
	seedHotShare      = 0.2
	seedHotClickShare = 0.8
)

func seedCmd(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	var (
		links  = fs.Int("links", 100_000, "links to create")
		clicks = fs.Int("clicks", 5_000_000, "click events to create")
		days   = fs.Int("days", 90, "spread click events over this many past days")
		prefix = fs.String("prefix", "ld", "alias prefix; aliases are <prefix><n>")
		user   = fs.String("user", "", "email of the owning user (default: the first user)")
		reset  = fs.Bool("reset", false, "delete previously seeded links and every click event first")
		force  = fs.Bool("force", false, "allow seeding when APP_ENV=production")
		seedNo = fs.Uint64("seed", 1, "PRNG seed, so two runs produce the same dataset")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: lctl seed [flags]

Generates a dataset for load testing: links resolvable at /<prefix><n> and click
events spread over the recent past. Defaults match the numbers the redirect SLO
is defined against.

Seeded links carry their destination URL directly and have no destinations row,
and click events carry random visitor hashes. Resolution is identical to a real
link; the editing surface and the visitor-uniqueness maths are not exercised.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *links < 1 {
		return fmt.Errorf("--links must be at least 1")
	}
	if *clicks < 0 {
		return fmt.Errorf("--clicks must not be negative")
	}
	if *days < 1 {
		return fmt.Errorf("--days must be at least 1")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// A load-test dataset in a production database is not a mistake anyone should
	// be able to make by pressing up-arrow in the wrong terminal.
	if cfg.AppEnv.IsProduction() && !*force {
		return fmt.Errorf("refusing to seed with APP_ENV=production; pass --force if you mean it")
	}

	ctx := context.Background()
	pools, err := postgres.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pools.Close()

	return seed(ctx, pools.App, cfg, seedOptions{
		links: *links, clicks: *clicks, days: *days, prefix: *prefix,
		user: *user, reset: *reset, prng: *seedNo,
	})
}

type seedOptions struct {
	links, clicks, days int
	prefix, user        string
	reset               bool
	prng                uint64
}

func seed(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, opt seedOptions) error {
	start := time.Now()

	// Membership rather than a created_by column, because a workspace does not
	// have one: Phase 1 gives every user one organization-wide membership, so the
	// oldest membership is the owner of the oldest workspace.
	const ownerQuery = `
		SELECT w.id, u.id
		  FROM workspaces w
		  JOIN memberships m ON m.organization_id = w.organization_id
		  JOIN users u       ON u.id = m.user_id
		 WHERE w.deleted_at IS NULL
		   AND u.deleted_at IS NULL
		   AND ($1 = '' OR lower(u.email) = lower($1))
		 ORDER BY w.created_at, m.created_at
		 LIMIT 1`

	var workspaceID, userID, domainID uuid.UUID
	if err := pool.QueryRow(ctx, ownerQuery, opt.user).Scan(&workspaceID, &userID); err != nil {
		return fmt.Errorf("no workspace to seed into (claim the instance first, or check --user): %w", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM domains WHERE is_default AND deleted_at IS NULL`).Scan(&domainID); err != nil {
		return fmt.Errorf("resolve default domain: %w", err)
	}

	if opt.reset {
		// Click events go wholesale: they are partitioned, so this is a truncate
		// rather than a scan, and half a dataset would make the numbers
		// meaningless anyway.
		if _, err := pool.Exec(ctx, `TRUNCATE click_events, visitors`); err != nil {
			return fmt.Errorf("reset click events: %w", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM links WHERE alias LIKE $1 || '%'`, opt.prefix); err != nil {
			return fmt.Errorf("reset links: %w", err)
		}
		fmt.Fprintln(os.Stderr, "reset: previous seed removed")
	}

	// Clicks land in the past, and an insert with no matching partition goes to
	// the default one — where it blocks attaching the partition that should have
	// held it. So the months are created first.
	from := time.Now().UTC().AddDate(0, 0, -opt.days)
	if n, err := store.EnsurePartitionRange(ctx, pool, from, time.Now().UTC()); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "created %d partitions covering the seeded range\n", n)
	}

	linkIDs, err := seedLinks(ctx, pool, opt, workspaceID, userID, domainID)
	if err != nil {
		return err
	}
	if err := seedClicks(ctx, pool, opt, workspaceID, linkIDs, from); err != nil {
		return err
	}

	// Without stats the planner is working from defaults on tables that just grew
	// by millions of rows, which is a good way to measure the wrong thing.
	fmt.Fprintln(os.Stderr, "analyzing…")
	if _, err := pool.Exec(ctx, `ANALYZE links, click_events`); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}

	fmt.Printf("seeded %d links and %d click events in %s\n",
		opt.links, opt.clicks, time.Since(start).Round(time.Second))
	fmt.Printf("aliases: %s0 … %s%d on %s\n",
		opt.prefix, opt.prefix, opt.links-1, cfg.Host())
	return nil
}

func seedLinks(ctx context.Context, pool *pgxpool.Pool, opt seedOptions,
	workspaceID, userID, domainID uuid.UUID,
) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, opt.links)
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV7())
	}

	cols := []string{
		"id", "workspace_id", "domain_id", "alias", "primary_url",
		"title", "created_by", "created_at",
	}
	created := time.Now().UTC().AddDate(0, 0, -opt.days)

	for lo := 0; lo < opt.links; lo += seedLinkBatch {
		hi := min(lo+seedLinkBatch, opt.links)
		rows := make([][]any, 0, hi-lo)
		for i := lo; i < hi; i++ {
			rows = append(rows, []any{
				ids[i],
				workspaceID,
				domainID,
				fmt.Sprintf("%s%d", opt.prefix, i),
				fmt.Sprintf("https://example.com/seed/%d", i),
				fmt.Sprintf("Seeded link %d", i),
				userID,
				created,
			})
		}
		if _, err := pool.CopyFrom(ctx, pgx.Identifier{"links"}, cols,
			pgx.CopyFromRows(rows)); err != nil {
			return nil, fmt.Errorf("copy links: %w", err)
		}
		fmt.Fprintf(os.Stderr, "\rlinks: %d/%d", hi, opt.links)
	}
	fmt.Fprintln(os.Stderr)
	return ids, nil
}

func seedClicks(ctx context.Context, pool *pgxpool.Pool, opt seedOptions,
	workspaceID uuid.UUID, linkIDs []uuid.UUID, from time.Time,
) error {
	if opt.clicks == 0 {
		return nil
	}

	// Deterministic by default: two runs produce the same dataset, so two load
	// results are comparable. Not a security decision — these are fake clicks.
	rng := rand.New(rand.NewPCG(opt.prng, 0x9E3779B97F4A7C15)) //nolint:gosec // G404: test data, determinism wanted

	devices := []string{"desktop", "mobile", "tablet", "bot"}
	browsers := []string{"Chrome", "Safari", "Firefox", "Edge", ""}
	oses := []string{"Windows", "macOS", "iOS", "Android", "Linux"}
	languages := []string{"en", "de", "fr", "es", "ja"}
	referrers := []string{"", "google.com", "t.co", "news.ycombinator.com", "example.org"}
	countries := []string{"GB", "DE", "US", "JP", "FR"}

	hot := max(1, int(float64(len(linkIDs))*seedHotShare))
	window := time.Since(from)

	cols := []string{
		"id", "link_id", "workspace_id", "occurred_at", "visitor_hash",
		"is_first_visit", "country", "region", "city", "device", "browser",
		"os", "language", "referrer_host", "is_bot", "latency_us",
	}

	for lo := 0; lo < opt.clicks; lo += seedClickBatch {
		hi := min(lo+seedClickBatch, opt.clicks)
		rows := make([][]any, 0, hi-lo)
		for range hi - lo {
			// Most traffic to a few links, which is what real traffic looks like
			// and what makes a cache hit ratio mean something.
			idx := rng.IntN(len(linkIDs))
			if rng.Float64() < seedHotClickShare {
				idx = rng.IntN(hot)
			}

			// Two words rather than sixteen narrowing conversions: same 16 bytes,
			// no int-to-byte truncation for a reader (or a linter) to check.
			var hash [16]byte
			binary.LittleEndian.PutUint64(hash[0:8], rng.Uint64())
			binary.LittleEndian.PutUint64(hash[8:16], rng.Uint64())

			device := devices[rng.IntN(len(devices))]
			rows = append(rows, []any{
				uuid.Must(uuid.NewV7()),
				linkIDs[idx],
				workspaceID,
				from.Add(time.Duration(rng.Int64N(int64(window)))),
				hash[:],
				rng.Float64() < 0.3,
				countries[rng.IntN(len(countries))],
				nil,
				nil,
				device,
				browsers[rng.IntN(len(browsers))],
				oses[rng.IntN(len(oses))],
				languages[rng.IntN(len(languages))],
				referrers[rng.IntN(len(referrers))],
				device == "bot",
				rng.Int32N(5000),
			})
		}
		if _, err := pool.CopyFrom(ctx, pgx.Identifier{"click_events"}, cols,
			pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("copy click events: %w", err)
		}
		fmt.Fprintf(os.Stderr, "\rclicks: %d/%d", hi, opt.clicks)
	}
	fmt.Fprintln(os.Stderr)

	// The denormalized counter is what the dashboard sorts by, so a seeded
	// dataset with zeroes there looks broken in a way that has nothing to do with
	// what is being measured.
	fmt.Fprintln(os.Stderr, "updating click counts…")
	if _, err := pool.Exec(ctx, `
		UPDATE links l
		   SET click_count = c.n, last_click_at = c.last
		  FROM (SELECT link_id, count(*) AS n, max(occurred_at) AS last
		          FROM click_events GROUP BY link_id) c
		 WHERE c.link_id = l.id`); err != nil {
		return fmt.Errorf("update click counts: %w", err)
	}
	return nil
}
