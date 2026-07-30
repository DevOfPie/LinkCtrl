// Package postgres builds the connection pools.
//
// There are deliberately two. The redirect pool is small, separate, and used
// only by the hot path, so that a slow analytics query holding application
// connections cannot leave a redirect waiting to acquire one. Milestone M13
// asserts this empirically by running a two-second analytics query and
// checking that the redirect pool's acquire-wait stays at zero.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/config"
)

// Pools holds the application and redirect pools.
type Pools struct {
	App      *pgxpool.Pool
	Redirect *pgxpool.Pool
}

// Close releases both pools. Safe to call with nil fields.
func (p *Pools) Close() {
	if p == nil {
		return
	}
	if p.Redirect != nil {
		p.Redirect.Close()
	}
	if p.App != nil {
		p.App.Close()
	}
}

// Open creates both pools and verifies connectivity.
func Open(ctx context.Context, c config.Config) (*Pools, error) {
	app, err := open(ctx, c, c.DB.MaxConns, c.DB.MinConns, "app")
	if err != nil {
		return nil, fmt.Errorf("application pool: %w", err)
	}

	// The redirect pool keeps a warm minimum: acquiring a cold connection on
	// the hot path costs a TCP handshake and a startup round trip, which alone
	// would blow the 20ms budget.
	minRedirect := min(c.DB.RedirectMaxConns, 2)
	redirect, err := open(ctx, c, c.DB.RedirectMaxConns, minRedirect, "redirect")
	if err != nil {
		app.Close()
		return nil, fmt.Errorf("redirect pool: %w", err)
	}

	return &Pools{App: app, Redirect: redirect}, nil
}

func open(ctx context.Context, c config.Config, maxConns, minConns int32, name string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(c.DB.URL.Reveal())
	if err != nil {
		// Do not wrap: the error may echo the DSN, which contains the password.
		return nil, fmt.Errorf("parse DATABASE_URL: invalid connection string")
	}

	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = c.DB.MaxConnLifetime
	cfg.MaxConnIdleTime = c.DB.MaxConnIdleTime
	cfg.ConnConfig.ConnectTimeout = c.DB.ConnectTimeout

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// UTC is pinned on the session as well as on the server and in the
	// container environment. None of the three is sufficient alone: partition
	// bounds on timestamptz resolve against the session timezone at DDL time,
	// so a connection running in a local zone silently creates partitions
	// offset from the intended range. See docs/adr/0001-partitioning-and-sqlc.md.
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	// Identifies the pool in pg_stat_activity, which is what makes "which pool
	// is holding this connection" answerable during an incident.
	cfg.ConnConfig.RuntimeParams["application_name"] = "linkctrl-" + name

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, c.DB.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// VerifyUTC checks that sessions really are running in UTC.
//
// Called at startup because the failure it prevents is silent and only becomes
// visible later, as gaps or overlaps in analytics partitions.
func VerifyUTC(ctx context.Context, pool *pgxpool.Pool) error {
	var tz string
	if err := pool.QueryRow(ctx, "SHOW timezone").Scan(&tz); err != nil {
		return fmt.Errorf("read session timezone: %w", err)
	}
	if tz != "UTC" {
		return fmt.Errorf("session timezone is %q, want UTC; partition bounds would be "+
			"created at the wrong offset", tz)
	}
	return nil
}

// PingTimeout is the budget for a readiness probe's database check. Kept short
// so a probe cannot pile up behind a struggling database.
const PingTimeout = 750 * time.Millisecond
