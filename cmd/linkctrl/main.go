// Command linkctrl is the LinkCtrl server.
//
// It is deliberately a single binary serving the redirect hot path, the REST
// API, the dashboard, and the background job scheduler. Plan.md lists these as
// separate logical services and the internal package boundaries follow those
// seams, but shipping eleven containers for an MVP would make `docker compose
// up` hostile to the self-hosters this project is for.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/build"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/platform/httpserver"
	"github.com/DevOfPie/LinkCtrl/internal/platform/postgres"
	"github.com/DevOfPie/LinkCtrl/internal/platform/redis"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/store"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

func main() {
	if err := main2(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "linkctrl: %v\n", err)
		os.Exit(1)
	}
}

func main2(args []string, stdout, stderr io.Writer) error {
	// Subcommands are matched before flags so `linkctrl healthcheck` works in
	// a distroless image, which has no shell or curl for the compose
	// healthcheck to use.
	if len(args) > 0 {
		switch args[0] {
		case "healthcheck":
			return healthcheck(args[1:])
		case "version":
			fmt.Fprintln(stdout, build.Get())
			return nil
		}
	}

	fs := flag.NewFlagSet("linkctrl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		showVersion = fs.Bool("version", false, "print version information and exit")
		asJSON      = fs.Bool("json", false, "with --version, print as JSON")
		checkConfig = fs.Bool("check-config", false, "validate configuration and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: linkctrl [flags]
       linkctrl healthcheck
       linkctrl version

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		info := build.Get()
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(info)
		}
		fmt.Fprintln(stdout, info)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		// Printed rather than wrapped, because Validate returns every problem
		// joined by newlines and the operator should see them as a list.
		fmt.Fprintf(stderr, "configuration is invalid:\n\n%v\n\n"+
			"See .env.example for the full reference.\n", err)
		return errors.New("invalid configuration")
	}

	if *checkConfig {
		fmt.Fprintln(stdout, "configuration OK")
		return nil
	}

	return run(cfg, stdout)
}

func run(cfg config.Config, _ io.Writer) error {
	log := observability.NewLogger(cfg, os.Stdout)
	slog.SetDefault(log)

	info := build.Get()
	// version is already on every record via the base logger; only the
	// details that are not are repeated here.
	log.Info("starting",
		slog.String("commit", info.ShortCommit()),
		slog.String("go", info.GoVersion),
		slog.String("env", string(cfg.AppEnv)),
		slog.String("base_url", cfg.BaseURL),
	)

	// Signals are trapped before any dependency is opened, so a Ctrl-C during
	// a slow database connect still exits promptly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !cfg.SecureCookies {
		log.Warn("insecure cookies enabled: sessions will not be protected in transit; " +
			"acceptable only for local HTTP development")
	}

	// Migrations run before the pools open and before the listener does, so a
	// request can never reach a half-migrated schema. A Postgres session lock
	// serializes replicas racing at startup.
	if cfg.MigrateOnStart {
		if err := store.Migrate(ctx, cfg.DB.URL.Reveal(), log); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	} else {
		log.Info("migrations skipped (MIGRATE_ON_START=false); run: lctl migrate up")
	}

	pools, err := postgres.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pools.Close()

	if err := postgres.VerifyUTC(ctx, pools.App); err != nil {
		return err
	}

	if cfg.MigrateOnStart {
		created, err := store.EnsurePartitions(ctx, pools.App, store.PartitionLookahead)
		if err != nil {
			return fmt.Errorf("ensure partitions: %w", err)
		}
		if created > 0 {
			log.Info("partitions created", slog.Int("count", created))
		}
		// A non-empty default partition means rows arrived outside every
		// explicit range, and attaching the partition that should have held
		// them will fail until they are moved. Worth saying loudly.
		if counts, err := store.DefaultPartitionCounts(ctx, pools.App); err == nil {
			for table, n := range counts {
				if n > 0 {
					log.Warn("rows in default partition; new partitions may fail to attach",
						slog.String("table", table), slog.Int64("rows", n))
				}
			}
		}
	}
	log.Info("postgres connected",
		slog.Int("app_pool_max", int(cfg.DB.MaxConns)),
		slog.Int("redirect_pool_max", int(cfg.DB.RedirectMaxConns)),
	)

	// A cache failure is not a startup failure. The service is fully correct
	// without Redis, only slower, so this logs and continues.
	var rdb *redis.Client
	if cfg.Redis.CacheEnabled {
		rdb, err = redis.Open(ctx, cfg)
		if err != nil {
			log.Warn("redis unavailable at startup; continuing without cache",
				slog.Any("error", err))
		} else {
			log.Info("redis connected")
		}
		if rdb != nil {
			defer func() { _ = rdb.Close() }()
		}
	} else {
		log.Info("cache disabled by configuration")
	}

	authSvc := auth.NewService(pools.App, auth.ServiceConfig{
		Params: auth.Params{
			MemoryKiB:   cfg.Auth.Argon2MemoryKiB,
			Iterations:  cfg.Auth.Argon2Iterations,
			Parallelism: cfg.Auth.Argon2Parallelism,
		},
		TTL: auth.SessionTTL{
			Absolute: cfg.Auth.SessionAbsoluteTTL,
			Idle:     cfg.Auth.SessionIdleTTL,
		},
		Lockout: auth.LockoutPolicy{
			Threshold: cfg.Auth.LockoutThreshold,
			Window:    15 * time.Minute,
		},
	})

	// The resolver runs on the dedicated redirect pool, so a slow analytics
	// query on the application pool cannot leave a redirect waiting to acquire
	// a connection.
	resolver := redirect.NewResolver(pools.Redirect, rdb, redirect.Options{
		TTL:          cfg.Redirect.TTL,
		NegativeTTL:  cfg.Redirect.NegativeTTL,
		RedisTimeout: cfg.Redis.ReadTimeout,
		Logger:       log,
	})

	linkSvc := link.NewService(pools.App, link.Config{
		Policy: link.DestinationPolicy{
			Schemes:             cfg.Alias.DestSchemes,
			MaxLength:           cfg.Alias.DestMaxLength,
			BlockPrivateIPs:     cfg.Alias.DestBlockPrivateIPs,
			BlockedHostSuffixes: cfg.Alias.DestBlocklist,
		},
		BaseURL: cfg.BaseURL,
		// Editing a link must drop its cached snapshot, and creating one must
		// drop any negative entry left by an earlier probe of the same alias.
		Cache: resolver,
	})

	// Resolved once at boot. A per-request lookup would add a query to the
	// path the whole cache design exists to keep short.
	defaultDomain, err := dbgen.New(pools.App).ResolveDefaultDomain(ctx)
	if err != nil {
		return fmt.Errorf("resolve default domain: %w", err)
	}

	// Analytics. The ingester buffers clicks and writes them in batches, so
	// recording never delays a redirect.
	salts := analytics.NewSaltCache(pools.App)
	ingester := analytics.NewIngester(pools.App, salts, analytics.IngestConfig{
		QueueSize:     cfg.Ingest.QueueSize,
		BatchSize:     cfg.Ingest.BatchSize,
		FlushInterval: cfg.Ingest.FlushInterval,
		Logger:        log,
	})
	ingester.Start()

	roller := analytics.NewRoller(pools.App, log)
	jobs := newJobRunner(pools.App, salts, roller, log)
	jobs.start(ctx)
	defer jobs.stop()

	redirectHandler := &httpx.RedirectHandler{
		Resolver:  resolver,
		DomainID:  defaultDomain.ID,
		Status:    cfg.Redirect.DefaultStatus,
		Logger:    log,
		LogSample: int64(cfg.Redirect.LogSample),
		Recorder:  clickRecorder{ingester: ingester},
	}

	if needsSetup, err := authSvc.NeedsSetup(ctx); err == nil && needsSetup {
		log.Warn("no users exist; claim this instance with " +
			"POST " + httpx.APIPrefix + "/auth/setup")
	}

	health := &httpx.Health{DB: pools.App, Redis: rdb}
	handler := httpx.NewRouter(httpx.Deps{
		Config: cfg, Health: health, Auth: authSvc, Links: linkSvc, Redirect: redirectHandler,
		Stats: analytics.NewReader(pools.App),
	})

	srv := httpserver.New(httpserver.Options{
		Addr:              cfg.HTTP.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		Logger:            log,
	})
	srv.OnDrain = health.StartDraining
	srv.DrainDelay = cfg.Shutdown.DrainDelay
	srv.ShutdownTimeout = cfg.Shutdown.Timeout
	// Runs after the listener has closed and in-flight requests have finished,
	// so no new clicks can arrive while the buffer drains. Without it every
	// restart loses up to a full batch.
	srv.OnShutdown = ingester.Close

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// healthcheck is the container healthcheck. The runtime image is distroless
// and has neither a shell nor curl, so the binary probes itself.
func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "", "address to probe (default: from LINKCTRL_HTTP_ADDR)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := *addr
	if target == "" {
		target = os.Getenv("LINKCTRL_HTTP_ADDR")
	}
	if target == "" {
		target = ":8080"
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", target, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("http://%s/readyz", net.JoinHostPort(host, port))

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz returned %d", resp.StatusCode)
	}
	return nil
}
