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
	"strings"
	"syscall"
	"time"

	// The IANA timezone database, embedded in the binary (M34).
	//
	// A routing rule's time window carries an IANA name — "Europe/London" —
	// rather than an offset, because an offset is wrong twice a year. Resolving
	// that name needs zoneinfo, and the runtime looks for it on the filesystem:
	// present in this project's distroless base, absent from `scratch`, and
	// absent from a great many images an operator might rebuild on. A rule that
	// silently fell back to UTC because of the base image somebody chose would
	// fire an hour late for half the year with nothing anywhere saying why.
	//
	// The cost is about 450KB of binary. That is a fair price for a rule
	// evaluating the same way wherever it runs.
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/alias"
	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/automation"
	"github.com/DevOfPie/LinkCtrl/internal/build"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/dnsx"
	"github.com/DevOfPie/LinkCtrl/internal/feed"
	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/geoip"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/invite"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/mail"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/platform/httpserver"
	"github.com/DevOfPie/LinkCtrl/internal/platform/postgres"
	"github.com/DevOfPie/LinkCtrl/internal/platform/redis"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
	"github.com/DevOfPie/LinkCtrl/internal/store"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/team"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
	"github.com/DevOfPie/LinkCtrl/internal/webhook"
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
		for _, w := range config.RemovedInUse() {
			fmt.Fprintf(stderr, "warning: %s\n", w)
		}
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
	// Logged only when the two differ. On a single-host instance this would be
	// the same string twice, in every startup.
	if cfg.SplitHosts() {
		log.Info("serving the dashboard and short links on separate hostnames",
			slog.String("app_base_url", cfg.AppBaseURL),
			slog.String("link_base_url", cfg.LinkBaseURL))
	}

	// Signals are trapped before any dependency is opened, so a Ctrl-C during
	// a slow database connect still exits promptly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !cfg.SecureCookies {
		log.Warn("insecure cookies enabled: sessions will not be protected in transit; " +
			"acceptable only for local HTTP development")
	}

	// Variables that used to exist. Warned about rather than ignored: an operator
	// who still has the line believes it does something, which is the same defect
	// as a knob that parses and changes nothing.
	for _, w := range config.RemovedInUse() {
		log.Warn(w)
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

	// Metrics come up before the services they observe, so a collector can be
	// registered at the point the thing it reads on is created.
	metrics := observability.NewMetrics()
	metrics.Register(observability.NewPoolCollector(map[string]*pgxpool.Pool{
		"app":      pools.App,
		"redirect": pools.Redirect,
	}))

	// Request limits. Built once here and shared: the router enforces two of
	// them, the redirect handler the third, and the collector reports on all
	// three, so none of them re-derives a limit from configuration.
	limits := httpx.NewLimiters(cfg, rdb, log)
	metrics.Register(observability.NewLimiterCollector(limits.Stats()))
	for name, on := range map[string]bool{
		"login":        limits.Login != nil,
		"api":          limits.API != nil,
		"redirect_404": limits.NotFound != nil,
	} {
		if !on {
			// Said out loud, because a limit silently set to zero is exactly the
			// kind of thing an operator means to change back and forgets.
			log.Warn("rate limit disabled by configuration", slog.String("limit", name))
		}
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

	// Built here rather than at the other services' construction point below
	// because the key service needs it and the key service is built first — the
	// dashboard and every API handler resolve their identity through it.
	auditSvc := audit.NewService(pools.App)

	keySvc, err := auth.NewAPIKeyService(pools.App, authSvc, auth.APIKeyConfig{
		Pepper: []byte(cfg.APIKeyPepper.Reveal()),
		// Rotation is the one key operation no human is present for, so it is the
		// one that has to leave a record (M44).
		Auditor: auditSvc,
		Logger:  log,
	})
	if err != nil {
		return err
	}
	keySvc.Start()

	// The resolver runs on the dedicated redirect pool, so a slow analytics
	// query on the application pool cannot leave a redirect waiting to acquire
	// a connection.
	resolver := redirect.NewResolver(pools.Redirect, rdb, redirect.Options{
		TTL:              cfg.Redirect.TTL,
		NegativeTTL:      cfg.Redirect.NegativeTTL,
		RedisTimeout:     cfg.Redis.ReadTimeout,
		InvalidateBudget: cfg.Redis.InvalidateBudget,
		DBTimeout:        cfg.Redirect.Timeout,
		Logger:           log,
	})

	// Created before the service and completed after it: the handler reads
	// through the service, and the service invalidates the handler's cache when
	// the setting changes. A pointer assigned in two steps rather than a setter
	// on the service, so neither side has to know the other exists first.
	rootRedirect := &httpx.RootRedirect{Status: cfg.Redirect.DefaultStatus}

	// The verified-hostname set (M40). On the application pool rather than the
	// redirect one: it is loaded at boot and on invalidation, never on a
	// request, and a reload must not compete for the small pool a redirect
	// acquires from.
	//
	// It is the whole of the custom-domain gate on the serving side — the router
	// resolves an alias on a Host header if and only if this map holds it, and
	// the one query that fills it filters on `verified_at`.
	hostCache := redirect.NewHostCache(pools.App, log)

	// The audit log is constructed above, beside the key service — emission is a
	// dependency the emitting features are built against, not something added to
	// them afterwards, and key rotation is one of the emitters.

	// The inbox. Its first consumer is the audit-growth warning in the job
	// runner below, which is why it is built here rather than beside the API.
	notifySvc := notify.NewService(pools.App)

	// The dashboard's templates, and the mail bodies with them. Parsing happens
	// here, at boot: a template error fails startup rather than the first
	// request to reach that page, or the first mail nobody is watching for.
	//
	// Before the mailer rather than beside the router, because the mailer
	// renders through it.
	renderer, err := ui.New()
	if err != nil {
		return fmt.Errorf("parse dashboard templates: %w", err)
	}
	// A missing stylesheet is a degraded start, not a failed one — the pages
	// still work unstyled. Loud in the log, because the fix is one command.
	for _, name := range renderer.MissingAssets() {
		log.Warn("embedded asset missing; the dashboard will render unstyled",
			slog.String("asset", name),
			slog.String("fix", "run `make css` (or `task css`) before `go build`"))
	}

	// The mailer. Optional and off unless SMTP_HOST is set, so an instance that
	// configures nothing behaves exactly as it did before this existed: no
	// sender, no job, and a nil Enqueuer at every consumer.
	var mailSvc *mail.Service
	if cfg.SMTP.Enabled() {
		sender, err := mail.NewSMTPSender(mail.SMTPOptions{
			Host:     cfg.SMTP.Host,
			Port:     cfg.SMTP.Port,
			Username: cfg.SMTP.Username,
			Password: cfg.SMTP.Password.Reveal(),
			From:     cfg.SMTP.From,
			TLS:      cfg.SMTP.TLS,
			Timeout:  cfg.SMTP.Timeout,
		})
		if err != nil {
			return err
		}
		// Connection details are checked once, here, by greeting the relay and
		// hanging up. A wrong host, a closed port or a rejected password is
		// reported by the process that could have told you, instead of showing
		// up weeks later as an invitation that never arrived.
		//
		// A warning and not a fatal error, for the same reason Redis is: the
		// relay being down is not a reason for a link shortener to stop serving
		// redirects, and anything queued meanwhile is retried from the outbox.
		// A *configuration* mistake is still fatal — config.Validate refuses an
		// unparseable sender or credentials that would go over the wire in
		// clear — so this is only ever about reachability.
		if err := sender.Verify(ctx); err != nil {
			log.Error("smtp relay did not accept a connection at startup; "+
				"queued mail will be retried until it does",
				slog.String("relay", sender.Addr()),
				slog.String("tls", cfg.SMTP.TLS),
				slog.Any("error", err))
		} else {
			log.Info("smtp relay reachable",
				slog.String("relay", sender.Addr()),
				slog.String("tls", cfg.SMTP.TLS),
				slog.Bool("authenticated", cfg.SMTP.Username != ""))
		}

		mailSvc, err = mail.NewService(pools.App, mail.Config{
			Renderer: renderer, Sender: sender, Logger: log,
		})
		if err != nil {
			return err
		}
		notifySvc = notifySvc.WithMail(mailSvc, cfg.AppOrigin())
	} else {
		log.Info("mail disabled (LINKCTRL_SMTP_HOST is empty); " +
			"notifications are delivered in the dashboard only")
	}

	// Whether this instance accepts new accounts, and by which paths.
	//
	// Built before invitations because both answer to the same mode. It is
	// LINKCTRL_SIGNUP_MODE and nothing else (D38) — no stored toggle, nothing a
	// session can move — with one derivation on top: a nil mailer lowers `open`
	// to `invite`, because there is then no way to verify an address (D1). The
	// string conversion is safe by construction, since config validation has
	// already refused anything but the three words this type also uses.
	signupSvc, err := signup.NewService(pools.App, signup.Config{
		Mode:   signup.Mode(cfg.Auth.SignupMode),
		AppURL: cfg.AppOrigin(),
		Hasher: authSvc.Hasher(),
		Mail:   signupMailer(mailSvc),
	})
	if err != nil {
		return err
	}
	// Said once, at boot, because it is the one way an operator can configure
	// open sign-ups and not get them. The signup page refuses on GET, but
	// nobody watches for a page they are not being shown.
	if signupSvc.Configured() == signup.Open && signupSvc.Effective() != signup.Open {
		log.Warn("LINKCTRL_SIGNUP_MODE is open but no mailer is configured; "+
			"public registration verifies an address by email, so sign-ups are invitation-only",
			slog.String("effective_signup_mode", string(signupSvc.Effective())))
	}

	// Invitations. Built after the mailer, because whether one exists is the
	// whole difference between "we emailed it" and "copy this link" — and a nil
	// Enqueuer here is the mail-free instance, not an error.
	//
	// NewAccounts follows the effective signup mode and is computed here rather
	// than passed as the mode, so the service reads a property of what it may do
	// instead of re-deciding what a configuration word means (D7).
	inviteSvc, err := invite.NewService(pools.App, invite.Config{
		AppURL:      cfg.AppOrigin(),
		TTL:         cfg.Auth.InviteTTL,
		NewAccounts: signupSvc.Effective().AdmitsNewAccounts(),
		Hasher:      authSvc.Hasher(),
		Audit:       auditSvc,
		Notify:      notifySvc,
		Mail:        inviteMailer(mailSvc),
		Log:         log,
	})
	if err != nil {
		return err
	}

	// Member management, workspace lifecycle and organization creation. The
	// other half of invitations: one decides who may join, the other what
	// happens to them afterwards.
	teamSvc := team.NewService(pools.App, team.Config{Audit: auditSvc, Log: log})

	// The opt-in reputation feed (M32). Off unless LINKCTRL_FEED_URL names one,
	// which is the default, and off means there is no client — not a client with
	// a false flag in it.
	//
	// The nil is assigned into the interface deliberately and only when a client
	// exists. A typed nil stored in link.Config.Feed would be a non-nil
	// interface holding nothing, and the guard that keeps destinations on this
	// box is `s.feed == nil`.
	var feedChecker link.FeedChecker
	feedClient, err := feed.New(feed.Config{
		Name:         cfg.Feed.Name,
		URL:          cfg.Feed.URL,
		Method:       strings.ToUpper(cfg.Feed.Method),
		Param:        cfg.Feed.Param,
		AuthHeader:   cfg.Feed.AuthHeader,
		AuthToken:    cfg.Feed.AuthToken.Reveal(),
		VerdictField: cfg.Feed.VerdictField,
		Timeout:      cfg.Feed.Timeout,
	})
	if err != nil {
		return fmt.Errorf("configure reputation feed: %w", err)
	}
	if feedClient != nil {
		feedChecker = feedClient
		// Said at boot, at info, and worded as what it does rather than as what
		// was configured. An operator who inherits a box should be able to find
		// this in the first screen of its log: it is the one setting on this
		// instance that sends its users' data somewhere else.
		log.Info("reputation feed enabled; destinations are sent to a third party "+
			"when a link is created, edited, or a refusal is disputed",
			slog.String("feed", feedClient.Name()),
			slog.String("endpoint", feedClient.Endpoint()),
			slog.Duration("timeout", cfg.Feed.Timeout),
			slog.String("disclosed_at", cfg.AppOrigin()+"/feeds"))
	} else {
		log.Info("no reputation feed configured (LINKCTRL_FEED_URL is empty); " +
			"no destination leaves this instance")
	}

	// The gates (M35). On the application pool rather than the redirect pool,
	// and that is not an oversight: every one of its queries is either a
	// management read or a write that only a *gated* link performs, so putting
	// them on the small pool the redirect path guards for itself would let a
	// burst of password submissions starve ordinary redirects.
	gateSvc := gate.NewService(pools.App, gate.Config{Hasher: authSvc.Hasher()})

	// Outbound webhooks (M42). Built before the link service, because the link
	// service holds it as the thing it hands events to.
	//
	// Always built, unlike the mailer and the feed: there is no operator switch,
	// because a webhook is registered by a workspace rather than enabled by the
	// operator. What the two numbers here decide is how long one delivery may
	// take and how long the log of what was attempted is kept.
	//
	// Its delivery client dials addresses somebody with a workspace chose, so it
	// carries a dialer that re-checks the resolved address at connect and
	// follows no redirect. See internal/webhook/client.go.
	webhookSvc := webhook.NewService(pools.App, webhook.Config{
		Timeout:       cfg.Webhooks.Timeout,
		RetentionDays: cfg.Webhooks.RetentionDays,
		Logger:        log,
		Observer:      metrics,
	})

	linkSvc := link.NewService(pools.App, link.Config{
		// The unappealable tier is not configured here and cannot be: private
		// and metadata addresses are refused unconditionally, and the scheme
		// list is confined by config validation to a subset of http,https. What
		// remains is a length bound.
		Policy: link.DestinationPolicy{
			Schemes:   cfg.Alias.DestSchemes,
			MaxLength: cfg.Alias.DestMaxLength,
		},
		Aliases: alias.Policy{
			ReservedExtra: cfg.Alias.ReservedExtra,
			// The configuration variable names the state an operator wants, and
			// the policy field names the state it disables. Inverting here rather
			// than in the policy keeps the zero Policy the safe one.
			ProfanityDisabled: !cfg.Alias.ProfanityFilter,
			MinUserLength:     cfg.Alias.MinUserLength,
			GeneratedLength:   cfg.Alias.Length,
		},
		// Short URLs are built from the link origin, which is the same as
		// BaseURL unless the deployment splits the two hosts.
		BaseURL: cfg.LinkOrigin(),
		// Editing a link must drop its cached snapshot, and creating one must
		// drop any negative entry left by an earlier probe of the same alias.
		Cache: resolver,
		// The root-redirect setting is refused unless short links have a
		// hostname of their own, where "/" is not the dashboard.
		SplitHosts: cfg.SplitHosts(),
		// Wrapped so a root-redirect change clears this process's copy and
		// tells every other replica to clear theirs.
		RootCache: redirect.BroadcastRootInvalidator{Local: rootRedirect, Publisher: resolver},
		Audit:     auditSvc,
		// Consulted last and only on a destination every built-in tier
		// accepted, so the protection an operator gets with no feed configured
		// is the protection they keep when one stops answering.
		Feed:        feedChecker,
		FeedMetrics: metrics,
		// The gates (M35). The hasher is the account hasher, deliberately: a
		// link password lands in the same database dump an account password
		// does, so it gets the same argon2 parameters rather than a cheaper set
		// justified by being "only" a link.
		Hasher: authSvc.Hasher(),
		Gates:  gateSvc,
		// Custom domains (M40). The resolver is the host's own, bounded per
		// lookup; the invalidator is the pair that refreshes this replica and
		// tells the others; and the notifier is what makes D70's grace window
		// fair rather than arbitrary — the workspace hears at the first failed
		// check, not at the stop.
		DNS: dnsx.Resolver{Timeout: cfg.Domains.VerifyDNSTimeout},
		Hosts: redirect.BroadcastHostInvalidator{
			Local: hostCache, Publisher: resolver,
		},
		DomainNotify: notifySvc,
		VerifyGrace:  cfg.Domains.VerifyGrace,
		// Webhook events (M42). A link write queues one row per subscribed
		// webhook and returns; nothing dials anything on the request path.
		Events: webhookSvc,
		Log:    log,
	})
	rootRedirect.Load = linkSvc.LoadRootRedirect
	// A rename changes the hostname a short URL is built from, and that string is
	// cached per process. The same signal that reloads the verified set drops it,
	// so a rename on one replica reaches the URLs every other one prints.
	hostCache.OnReload = linkSvc.ForgetHostnames

	// LINKCTRL_DESTINATION_BLOCKLIST becomes rows, once per boot.
	//
	// Before the listener opens, because a link created in the window between
	// accepting requests and reconciling the list is a link the operator's own
	// blocklist did not see. Fatal on failure for the same reason: an instance
	// that came up with a stale blocklist is one whose refusals do not match its
	// configuration, and it is better to not start than to be quietly wrong
	// about what it refuses.
	if err := linkSvc.SeedBlocklist(ctx, cfg.Alias.DestBlocklist); err != nil {
		return fmt.Errorf("seed destination blocklist: %w", err)
	}

	// The appeal path for that list, and the queue an owner works through.
	//
	// It takes the link service as its judge rather than re-deriving anything:
	// which tier refused a destination has exactly one answer in this program,
	// and a second evaluator here would be a second answer waiting to disagree.
	disputeSvc, err := dispute.NewService(pools.App, dispute.Config{
		Judge:  linkSvc,
		Audit:  auditSvc,
		Notify: notifySvc,
		Log:    log,
	})
	if err != nil {
		return err
	}

	// The other half of invalidation: this replica hearing what the others
	// published. Off the request path entirely — it only ever deletes from the
	// in-process tiers — and a nil Redis client makes Run return immediately,
	// which is the cache-disabled deployment falling back to TTL staleness.
	//
	// Started before the listener so a redirect served in the first moments of
	// this process's life is not served from a tier nothing is watching.
	// Root is the plain cache, deliberately not the broadcasting wrapper above.
	// Handing the wrapper to the subscriber would make every received root
	// invalidation publish another one, and every replica would answer every
	// other replica's message forever.
	//
	// ReadTimeout is what stops silence being mistaken for freshness: the read
	// is bounded so that a Redis holding the connection open and answering
	// nothing gets noticed, rather than blocking here for the rest of the
	// entry TTL (F30).
	subscriber := &redirect.Subscriber{
		Redis: rdb, Resolver: resolver, Root: rootRedirect, Hosts: hostCache, Log: log,
		ReadTimeout: cfg.Redis.SubscriberReadTimeout,
	}
	subCtx, stopSubscriber := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSubscriber()
	go subscriber.Run(subCtx)

	// Resolved once at boot. A per-request lookup would add a query to the
	// path the whole cache design exists to keep short.
	defaultDomain, err := dbgen.New(pools.App).ResolveDefaultDomain(ctx)
	if err != nil {
		return fmt.Errorf("resolve default domain: %w", err)
	}

	// The verified set, before the listener opens. Fatal on failure rather than
	// degraded: the cache fails closed, so a process that started without it
	// would answer the operational 404 on every custom hostname it is supposed
	// to serve — an outage that looks exactly like the domains having been
	// unverified. Refusing to start says what actually happened.
	if err := hostCache.Reload(ctx); err != nil {
		return fmt.Errorf("load verified domains: %w", err)
	}
	log.Info("verified custom domains loaded", slog.Int("hostnames", hostCache.Size()))

	// Geographic enrichment, if an operator supplied a database. Opened before
	// the ingester because the ingester needs it, and opened rather than trusted:
	// config validation only checks the file is readable, and a truncated
	// database would otherwise show up as permanently empty countries.
	geo, err := geoip.Open(cfg.Analytics.GeoIPPath)
	if err != nil {
		return err
	}
	// Deliberately never closed. Close unmaps the file and is not safe against
	// concurrent lookups, and on the one shutdown path where it would run with
	// work still in flight — an analytics flush timing out, Ingester.Close
	// returning while its worker is still enriching a draining batch — a
	// deferred Close here turned an orderly error exit into a potential
	// segfault. The mapping's lifetime is the process's lifetime; the OS
	// reclaims it at exit, which is the only moment it stops being needed.
	if geo.Enabled() {
		log.Info("geoip database loaded", slog.String("database", geo.Description()))
	} else {
		log.Info("geographic analytics disabled (GEOIP_MMDB_PATH is empty)")
	}

	// Analytics. The ingester buffers clicks and writes them in batches, so
	// recording never delays a redirect.
	salts := analytics.NewSaltCache(pools.App)
	ingestCfg := analytics.IngestConfig{
		QueueSize:     cfg.Ingest.QueueSize,
		BatchSize:     cfg.Ingest.BatchSize,
		FlushInterval: cfg.Ingest.FlushInterval,
		Logger:        log,
	}
	// Assigned only when there is a database. Handing over a nil *Resolver would
	// still satisfy the interface — a nil pointer in an interface is not a nil
	// interface — and the ingester's "is geography configured" check would then
	// depend on the resolver's nil-tolerance rather than saying what it means.
	if geo.Enabled() {
		ingestCfg.Geo = geo
	}

	// The within-day returning-visitor set (M34). Nil without Redis, and then
	// every visitor reads as new — the documented degradation, not a silent one.
	returning := analytics.NewReturningSet(rdb, salts, cfg.Redis.ReadTimeout, log)
	ingestCfg.Returning = returning

	// Today's salt, loaded before the listener opens.
	//
	// The returning-visitor check on the redirect path reads the salt cache
	// without being allowed to fall through to Postgres, because M34 claims rule
	// evaluation adds no database query per request. Warming it here is what
	// makes that claim cost nothing: without it, a process that had just started
	// would answer "not returning" to everybody until its first click batch
	// flushed. Failure is logged and not fatal — a missing salt degrades one
	// condition, and refusing to boot over it would be worse than the
	// degradation.
	if _, err := salts.For(ctx, time.Now()); err != nil {
		log.Warn("could not warm today's analytics salt; returning-visitor rules "+
			"will treat every visitor as new until the first click batch flushes",
			slog.Any("error", err))
	}

	ingester := analytics.NewIngester(pools.App, salts, ingestCfg)
	ingester.Start()
	metrics.Register(observability.NewIngestCollector(ingester))

	// Automation rules (M43). Built after the link service, because it is the
	// link service it hands an archive to — the one-way graph internal/webhook
	// already has, in the other direction.
	//
	// Always built and always wired into the scheduler, with no operator switch:
	// a rule is a workspace's instruction, not an operator's feature. What
	// switches it off is the workspace having no enabled rules, which costs one
	// indexed query per minute that returns nothing.
	automationSvc := automation.NewService(pools.App, automation.Config{
		Links:    linkSvc,
		Notifier: notifySvc,
		Events:   webhookSvc,
		Audit:    auditSvc,
		Logger:   log,
		Observer: metrics,
	})

	roller := analytics.NewRoller(pools.App, log)
	jobs := newJobRunner(pools.App, salts, roller, log, metrics, notifySvc, mailSvc, signupSvc,
		linkSvc, webhookSvc, automationSvc, cfg.Domains,
		cfg.Analytics.RetentionDays, cfg.Audit.RetentionDays, cfg.Audit.SizeWarnBytes)
	jobs.start(ctx)
	defer jobs.stop()

	redirectHandler := &httpx.RedirectHandler{
		Resolver:        resolver,
		DomainID:        defaultDomain.ID,
		Status:          cfg.Redirect.DefaultStatus,
		Logger:          log,
		LogSample:       int64(cfg.Redirect.LogSample),
		Recorder:        clickRecorder{ingester: ingester},
		Metrics:         metrics,
		NotFoundLimiter: limits.NotFound,
		// The gates (M35). Consulted only for a link whose snapshot says it is
		// gated, which is no link at all on a default instance.
		Gates:           gateSvc,
		PasswordLimiter: limits.LinkPassword,
		// Routing rules (M34). Assigned only when there is a database, for the
		// same reason ingestCfg.Geo is: a nil *geoip.Resolver in an interface is
		// not a nil interface, and the handler's "is geography available" check
		// would then rest on the resolver's nil-tolerance rather than saying what
		// it means.
		Returning: returning,
	}
	if geo.Enabled() {
		redirectHandler.Geo = geo
	}

	if needsSetup, err := authSvc.NeedsSetup(ctx); err == nil && needsSetup {
		log.Warn("no users exist; claim this instance with " +
			"POST " + httpx.APIPrefix + "/auth/setup")
	}

	stats := analytics.NewReader(pools.App)
	health := &httpx.Health{DB: pools.App, Redis: rdb}
	handler := httpx.NewRouter(httpx.Deps{
		Config: cfg, Health: health, Auth: authSvc, Keys: keySvc,
		Links: linkSvc, Redirect: redirectHandler,
		RootRedirect: rootRedirect,
		// Custom domains (M40). Hosts is the gate: a Host header reaches the
		// redirect tree only if this map holds it.
		Hosts:      hostCache,
		DomainRoot: &httpx.DomainRootRedirect{Status: cfg.Redirect.DefaultStatus},
		TLSAsk: &httpx.TLSAsk{
			Hosts: hostCache,
			Activate: func(ctx context.Context, d redirect.VerifiedDomain) {
				if _, err := dbgen.New(pools.App).MarkDomainTLSActive(ctx, d.ID); err != nil {
					log.Debug("could not record that the TLS ask was answered",
						slog.String("hostname", d.Hostname), slog.Any("error", err))
				}
			},
		},
		Stats:    stats,
		Audit:    auditSvc,
		Notify:   notifySvc,
		Invites:  inviteSvc,
		Team:     teamSvc,
		Signup:   signupSvc,
		Disputes: disputeSvc,
		Metrics:  metrics,
		Limits:   limits,
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc, Keys: keySvc,
			Links: linkSvc, Stats: stats, Notify: notifySvc, Invites: inviteSvc,
			Team: teamSvc, Signup: signupSvc, Disputes: disputeSvc,
		},
	})

	// The scrape endpoint lives on its own listener, on a port compose does not
	// publish. Queue depths, pool saturation and traffic shape are operational
	// detail, and putting them behind the same listener as the public site is
	// how they end up on the internet by accident.
	metricsSrv := httpserver.New(httpserver.Options{
		Addr:              cfg.HTTP.MetricsAddr,
		Handler:           metricsMux(metrics),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		Logger:            log,
	})
	// No drain delay: nothing routes user traffic here, so there is nothing to
	// deregister from. A scrape lost during shutdown is a gap in a graph.
	metricsSrv.ShutdownTimeout = 2 * time.Second
	metricsDone := make(chan struct{})
	go func() {
		defer close(metricsDone)
		if err := metricsSrv.Run(ctx); err != nil {
			// Logged, never fatal. Losing metrics must not take down a healthy
			// instance — the alternative is an unmonitored outage caused by the
			// monitoring.
			log.Error("metrics listener stopped", slog.Any("error", err))
		}
	}()
	defer func() { <-metricsDone }()

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
	// so no new clicks can arrive while the buffers drain. Without it every
	// restart loses up to a full batch of clicks, and the last few minutes of
	// API key usage timestamps.
	srv.OnShutdown = func(ctx context.Context) error {
		// Clicks first: they are the data a user would notice missing. A failed
		// key flush must not skip it, so both run and the errors are joined.
		return errors.Join(ingester.Close(ctx), keySvc.Close(ctx))
	}

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// metricsMux is the internal listener's routing table: the scrape endpoint and
// a hint for whoever opens the port in a browser.
func metricsMux(metrics *observability.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("LinkCtrl internal metrics listener: GET /metrics\n"))
	})
	return mux
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

	// G704 reads the address as attacker-controlled input. It is this process's own
	// listen address, and probing itself is the entire purpose: the runtime image
	// is distroless, with no shell and no curl for the container healthcheck to
	// use. Anyone able to set the environment already controls the process.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil) //nolint:gosec // G704: self-probe of our own listener
	if err != nil {
		return err
	}
	resp, err := client.Do(req) //nolint:gosec // G704: same request, same reason
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

// inviteMailer hands the outbox to the invitation service, or nothing at all.
//
// A typed nil pointer inside an interface is not a nil interface, so passing
// mailSvc straight through on a mail-free instance would give the service an
// Enqueuer that is non-nil and panics on first use — and the first use is
// somebody being invited. This is the one place that conversion happens.
func inviteMailer(m *mail.Service) invite.Enqueuer {
	if m == nil {
		return nil
	}
	return m
}

// signupMailer is inviteMailer for the signup service, and exists for exactly
// the same typed-nil reason. It matters more here: a nil Enqueuer is what drops
// the signup ceiling to `invite` (D1), so getting this conversion wrong would
// offer open sign-ups on an instance that cannot verify an address.
func signupMailer(m *mail.Service) signup.Enqueuer {
	if m == nil {
		return nil
	}
	return m
}
