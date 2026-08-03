package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/platform/postgres"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// The demo dataset. Distinct from `lctl seed`, which exists to make the redirect
// SLO measurable and produces a hundred thousand links called ld0…ld99999 with
// no destinations rows. Pointing a person at that database teaches them nothing
// about the product.
//
// Two rules make this worth committing rather than keeping as a snippet.
//
// Links are created through the link service, the same call the REST API makes,
// so alias policy, destination validation, tag creation and the destinations
// row all happen exactly as they do for a client. A seeder that writes rows
// directly can invent states the product cannot reach, and then the dashboard is
// being debugged against data it could never have produced.
//
// Backfilled click events match what the ingester would have written, column for
// column: no address anywhere, referrers already reduced to a host,
// is_first_visit false, region and city null, and device/browser/OS strings from
// the same vocabulary analytics.Classify emits.

// demoLink is one row of the catalogue.
type demoLink struct {
	alias, url, title, desc string
	tags                    []string
	forwardQuery            bool
	forwardPath             bool
	// weight is relative daily traffic. 0 means the link exists and is never
	// clicked, which is a state worth showing.
	weight int
	// from and to bound the traffic in days-ago terms; to=0 means "still busy".
	from, to int
	// spikeDay and spikeMult model a launch or a post that got picked up.
	spikeDay  int
	spikeMult float64
	// age is how many days ago the link was created.
	age int
	// state is "", "archived", "trashed" or "expired".
	state string
}

func demoCatalogue() []demoLink {
	return []demoLink{
		{alias: "launch", url: "https://example.com/blog/introducing-linkctrl",
			title: "Introducing LinkCtrl 0.1", desc: "Launch announcement, shared everywhere at once.",
			tags: []string{"marketing", "blog"}, weight: 50, from: 29, spikeDay: 14, spikeMult: 7, age: 32},
		{alias: "handbook", url: "https://example.com/docs", title: "Documentation home",
			desc: "Path forwarding on: /handbook/api/quickstart reaches the destination's own /api/quickstart.",
			tags: []string{"docs"}, forwardPath: true, weight: 42, from: 29, age: 44},
		{alias: "quickstart", url: "https://example.com/docs/quickstart", title: "API quickstart",
			tags: []string{"docs", "dev"}, weight: 24, from: 29, age: 40},
		{alias: "pricing-2026", url: "https://example.com/pricing", title: "Pricing",
			tags: []string{"marketing"}, weight: 30, from: 29, age: 44},
		{alias: "repo", url: "https://github.com/DevOfPie/LinkCtrl", title: "Source on GitHub",
			tags: []string{"dev", "social"}, weight: 18, from: 29, spikeDay: 14, spikeMult: 4, age: 41},
		{alias: "newsletter", url: "https://example.com/subscribe", title: "Monthly newsletter",
			desc: "Query forwarding on: campaign parameters survive the hop.",
			tags: []string{"marketing"}, forwardQuery: true, weight: 14, from: 29,
			spikeDay: 7, spikeMult: 3, age: 38},
		{alias: "demo-call", url: "https://cal.example.com/linkctrl/demo", title: "Book a demo",
			tags: []string{"sales"}, weight: 11, from: 29, age: 36},
		{alias: "summer-sale", url: "https://shop.example.com/summer", title: "Summer sale, 30% off",
			desc: "Expired campaign: answers 410 Gone, not 404.",
			tags: []string{"campaign"}, weight: 26, from: 26, to: 4, age: 26, state: "expired"},
		{alias: "webinar", url: "https://example.com/events/scaling-links",
			title: "Webinar: scaling short links", desc: "Expires when registration closes.",
			tags: []string{"events"}, weight: 12, from: 12, age: 12},
		{alias: "help-centre", url: "https://help.example.com", title: "Help centre",
			tags: []string{"support"}, weight: 16, from: 29, age: 43},
		{alias: "whats-new", url: "https://example.com/changelog", title: "Changelog",
			tags: []string{"product"}, weight: 9, from: 29, age: 39},
		{alias: "roadmap-2026", url: "https://example.com/roadmap", title: "Public roadmap",
			tags: []string{"product"}, weight: 7, from: 29, age: 34},
		{alias: "uptime", url: "https://status.example.com", title: "Status page",
			tags: []string{"ops"}, weight: 5, from: 29, age: 42},
		{alias: "hiring", url: "https://example.com/careers", title: "We are hiring",
			tags: []string{"hr"}, weight: 10, from: 20, age: 20},
		{alias: "intro-video", url: "https://www.youtube.com/watch?v=aqz-KE-bpKQ",
			title: "Two-minute intro video", tags: []string{"social", "video"},
			weight: 15, from: 29, spikeDay: 14, spikeMult: 3, age: 30},
		{alias: "press-kit", url: "https://example.com/press", title: "Press kit",
			tags: []string{"marketing"}, weight: 4, from: 24, age: 24},
		{alias: "partitioning", url: "https://example.com/blog/postgres-partitioning",
			title: "Blog: partitioning by month", tags: []string{"blog", "dev"},
			weight: 13, from: 18, spikeDay: 17, spikeMult: 5, age: 18},
		{alias: "nps", url: "https://forms.example.com/nps-2026", title: "NPS survey (closed)",
			desc: "Archived: keeps its alias and its analytics, and stops redirecting.",
			tags: []string{"product"}, weight: 6, from: 29, to: 6, age: 35, state: "archived"},
		{alias: "conf-2026", url: "https://example.com/events/conf-2026",
			title: "Conference landing page",
			desc:  "In the trash: holds its alias until the purge job takes it.",
			tags:  []string{"events"}, weight: 8, from: 28, to: 3, age: 28, state: "trashed"},
		// No alias: the generator picks one, so the demo shows both styles.
		{url: "https://example.com/whitepaper/short-links.pdf", title: "Whitepaper (generated alias)",
			tags: []string{"marketing"}, weight: 7, from: 15, age: 15},
		{url: "https://example.com/webinars/replay", title: "Webinar replay (generated alias)",
			tags: []string{"events"}, weight: 7, from: 15, age: 15},
	}
}

func demoCmd(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var (
		user    = fs.String("user", "", "email of the owning user (default: the first user)")
		days    = fs.Int("days", 30, "days of click history to generate")
		reset   = fs.Bool("reset", false, "delete the demo links and all click events first")
		force   = fs.Bool("force", false, "allow seeding when APP_ENV=production")
		volume  = fs.Float64("volume", 4, "traffic multiplier; 4 gives roughly 1,200 clicks a day")
		prngNum = fs.Uint64("seed", 1, "PRNG seed, so two runs produce the same dataset")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: lctl demo [flags]

Fills an empty instance with an installation worth looking at.

Two workspaces: the first with around twenty links, their titles, tags and
destinations, and a month of click history with weekday seasonality, a launch
spike, bots, and every status the dashboard can render — an archived link, an
expired campaign, one in the trash; the second with a handful of its own, so the
workspace switcher has something to switch between.

Around that, what Phase 2 added: two more accounts and their memberships, an
outstanding invitation and two redeemed ones, an inbox with unread items, an
audit trail spanning several actions, a blocked destination, a dispute in each
of its three states, and bot blocking switched on for exactly one link.

Everything is created through the same service calls the dashboard and the REST
API make, so the data cannot describe a state the product could not reach. Click
history is written directly, because the redirect path can only produce traffic
for right now; every column matches what the ingester would have written.

No mailer is needed and none is used: the invitation it leaves outstanding is
reachable by the link printed below. No reputation feed is enabled, so no
destination leaves this instance. LINKCTRL_SIGNUP_MODE is not read and not
changed.

For load testing rather than looking at, use `+"`lctl seed`"+` instead.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *days < 1 || *days > 365 {
		return fmt.Errorf("--days must be between 1 and 365")
	}
	if *volume <= 0 {
		return fmt.Errorf("--volume must be positive")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.AppEnv.IsProduction() && !*force {
		return fmt.Errorf("refusing to seed demo data with APP_ENV=production; pass --force if you mean it")
	}

	ctx := context.Background()
	pools, err := postgres.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pools.Close()

	return demoSeed(ctx, pools.App, cfg, demoOptions{
		user: *user, days: *days, reset: *reset, volume: *volume, prng: *prngNum,
	})
}

type demoOptions struct {
	user   string
	days   int
	reset  bool
	volume float64
	prng   uint64
}

func demoSeed(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, opt demoOptions) error {
	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: auth.Params{
			MemoryKiB:   cfg.Auth.Argon2MemoryKiB,
			Iterations:  cfg.Auth.Argon2Iterations,
			Parallelism: cfg.Auth.Argon2Parallelism,
		},
	})

	email := strings.TrimSpace(opt.user)
	if email == "" {
		if err := pool.QueryRow(ctx,
			`SELECT email FROM users ORDER BY created_at LIMIT 1`).Scan(&email); err != nil {
			return fmt.Errorf("no user to own the demo data; claim the instance first, "+
				"or pass --user: %w", err)
		}
	}
	actor, err := authSvc.IdentityForEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("resolve user %s: %w", email, err)
	}

	catalogue := demoCatalogue()

	if opt.reset {
		if err := demoReset(ctx, pool, actor, catalogue); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "reset: previous demo data removed")
		// The reset deleted the second workspace, and with it whatever the last
		// run pinned as this account's last-used one. Re-resolving keeps the
		// identity below pointing at a workspace that still exists.
		if actor, err = authSvc.IdentityForEmail(ctx, email); err != nil {
			return fmt.Errorf("resolve user %s: %w", email, err)
		}
	}

	// The backfill reaches into last month, and an insert with no matching
	// partition lands in the default one, where it silently blocks attaching
	// the partition that should have held it.
	now := time.Now().UTC()
	created, err := store.EnsurePartitionRange(ctx, pool,
		now.AddDate(0, 0, -opt.days), now.AddDate(0, 1, 0))
	if err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}
	if created > 0 {
		fmt.Fprintf(os.Stderr, "created %d partitions covering the demo range\n", created)
	}

	auditSvc := audit.NewService(pool)
	gateSvc := gate.NewService(pool, gate.Config{Hasher: authSvc.Hasher()})
	linkSvc := link.NewService(pool,
		demoLinkConfig(cfg, auditSvc, authSvc.Hasher(), gateSvc))

	ids, err := demoCreateLinks(ctx, pool, linkSvc, actor, catalogue, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "links: %d\n", len(ids))

	clicks, err := demoClicks(ctx, pool, actor.WorkspaceID.String(), catalogue, ids, opt, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "clicks: %d\n", clicks)

	// Everything Phase 2 added. Before the rollup below, because the second
	// workspace writes click events of its own and a rollup that ran first would
	// leave its analytics pages empty.
	seeder, err := newDemoSeeder(pool, cfg, authSvc, linkSvc, gateSvc, actor, opt, now)
	if err != nil {
		return err
	}
	if err := seeder.run(ctx, catalogue, ids); err != nil {
		return err
	}

	// Roll the whole window up. The application's job only ever recomputes
	// yesterday and today, because that is all live traffic can change;
	// backfilled history needs the same statements over the full range.
	roller := analytics.NewRoller(pool, discardLogger())
	from := now.AddDate(0, 0, -opt.days).Truncate(24 * time.Hour)
	if err := roller.Run(ctx, from, now.AddDate(0, 0, 1)); err != nil {
		return fmt.Errorf("roll up demo history: %w", err)
	}
	fmt.Fprintln(os.Stderr, "rollups computed")

	if _, err := pool.Exec(ctx, `ANALYZE click_events`); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\ndemo data ready for %s\n", email)
	fmt.Fprintf(os.Stderr, "links are at %s/<alias>, dashboard at %s\n",
		cfg.LinkOrigin(), cfg.AppOrigin())
	return nil
}
