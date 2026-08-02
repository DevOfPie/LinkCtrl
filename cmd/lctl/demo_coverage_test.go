//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// What the demo instance must show, enumerated (M33.5).
//
// This list is the mechanism the milestone is about. `lctl demo` was written when
// a workspace and twenty links were the whole product, and it stayed that way
// through nine milestones of Phase 2 — not because anybody decided the demo
// should under-sell the phase, but because nothing failed when it did. A rule
// asking people to remember produced exactly one outcome, and this is the
// replacement.
//
// **A milestone that ships something a person can see extends the seeder and
// adds a row here.** That obligation is real and it is deliberate: the build
// fails otherwise. If it ever proves too heavy, the answer is to narrow what
// this list covers, in writing, with the reasoning recorded — never to delete
// the test. A demo nobody enforces rots back to twenty links, and the second
// time it happens nobody will notice for another nine milestones.
//
// Rows are ordered by the milestone they belong to, so the boundary at the
// bottom — what is deliberately *not* seeded yet — reads as the end of a list
// rather than as an omission.

// demoFeature is one thing the demo must show, and the query that proves it does.
type demoFeature struct {
	// Milestone is the number that shipped the feature, so a failure says which
	// page went empty rather than only which query returned zero.
	Milestone string
	// Feature is what somebody opening the demo would be looking at.
	Feature string
	// Query counts the seeded rows. $1 is the demo organization; $2, where the
	// query uses it, is its owner.
	Query string
	// Min is the count below which the feature is not being shown.
	Min int64
	// Max bounds it. Zero means unbounded, except where MaxIsZero says the
	// feature must produce no rows at all.
	Max int64
	// MaxIsZero marks a row whose whole claim is that nothing is seeded — the
	// boundary rows for milestones not yet built.
	MaxIsZero bool
	// Shows is what the demo loses when this row fails, in the words of somebody
	// looking at the instance rather than at the schema.
	Shows string
}

// demoWorkspaces is the scope every query below is written against: the demo
// organization's workspaces, which is what a person signed in as the owner can
// reach.
const demoWorkspaces = `SELECT id FROM workspaces WHERE organization_id = $1`

func demoCoverage() []demoFeature {
	return []demoFeature{
		{
			Milestone: "M8", Feature: "A catalogue of links",
			Query: `SELECT count(*) FROM links WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min:   20,
			Shows: "the link list, which is the first page anybody opens",
		},
		{
			Milestone: "M9", Feature: "Click history",
			Query: `SELECT count(*) FROM click_events WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min:   1000,
			Shows: "every chart on the analytics pages",
		},
		{
			Milestone: "M9", Feature: "Rolled-up analytics",
			Query: `SELECT count(*) FROM link_click_daily WHERE workspace_id IN (` + demoWorkspaces + `)`,
			Min:   50,
			Shows: "the daily series, which reads from the rollup rather than the events",
		},
		{
			Milestone: "M21", Feature: "An audit trail spanning several actions",
			Query: `SELECT count(DISTINCT action) FROM audit_logs WHERE organization_id = $1`,
			Min:   5,
			Shows: "the audit page as a trail rather than as one root-redirect change",
		},
		{
			Milestone: "M22", Feature: "Notifications, some unread",
			Query: `SELECT count(*) FROM notifications
			         WHERE user_id = $2 AND read_at IS NULL
			           AND (workspace_id IS NULL
			                OR workspace_id IN (` + demoWorkspaces + `))`,
			Min:   1,
			Shows: "the notification bell with a count on it",
		},
		{
			Milestone: "M22", Feature: "Notifications already read",
			Query: `SELECT count(*) FROM notifications
			         WHERE user_id = $2 AND read_at IS NOT NULL
			           AND (workspace_id IS NULL
			                OR workspace_id IN (` + demoWorkspaces + `))`,
			Min:   1,
			Shows: "that read and unread are two states, not one empty list",
		},
		{
			Milestone: "M25", Feature: "A second workspace",
			Query: `SELECT count(*) FROM workspaces
			         WHERE organization_id = $1 AND deleted_at IS NULL`,
			Min:   2,
			Shows: "the workspace switcher, which renders nothing when there is one",
		},
		{
			Milestone: "M25", Feature: "Links in every workspace",
			Query: `SELECT count(*) FROM workspaces w
			         WHERE w.organization_id = $1 AND w.deleted_at IS NULL
			           AND EXISTS (SELECT 1 FROM links l WHERE l.workspace_id = w.id)`,
			Min:   2,
			Shows: "that scoping is observable: switching changes what the list holds",
		},
		{
			Milestone: "M27", Feature: "An outstanding invitation",
			Query: `SELECT count(*) FROM invitations
			         WHERE organization_id = $1 AND redeemed_at IS NULL
			           AND revoked_at IS NULL AND expires_at > now()`,
			Min:   1,
			Shows: "the pending half of the invitations page",
		},
		{
			Milestone: "M27", Feature: "A redeemed invitation",
			Query: `SELECT count(*) FROM invitations
			         WHERE organization_id = $1 AND redeemed_at IS NOT NULL`,
			Min:   1,
			Shows: "the other half of the lifecycle, and how the members got there",
		},
		{
			Milestone: "M28", Feature: "More than one member",
			Query: `SELECT count(DISTINCT user_id) FROM memberships WHERE organization_id = $1`,
			Min:   3,
			Shows: "the members page as a list rather than as a single row",
		},
		{
			Milestone: "M28", Feature: "A member who is not an owner",
			Query: `SELECT count(*) FROM memberships m
			          JOIN roles r ON r.id = m.role_id
			         WHERE m.organization_id = $1 AND r.slug <> 'owner'`,
			Min:   2,
			Shows: "rank: manage only strictly below your own, with something to act on",
		},
		{
			Milestone: "M28", Feature: "A workspace-scoped membership",
			Query: `SELECT count(*) FROM memberships
			         WHERE organization_id = $1 AND workspace_id IS NOT NULL`,
			Min:   1,
			Shows: "that a grant adds and never narrows (D31)",
		},
		{
			Milestone: "M30", Feature: "Blocked destination attempts",
			Query: `SELECT count(*) FROM audit_logs
			         WHERE organization_id = $1 AND action = 'destination.blocked'`,
			Min:   3,
			Shows: "that refusals are recorded, which is what makes a tier tunable",
		},
		{
			Milestone: "M30", Feature: "The attempted URL stored defanged",
			Query: `SELECT count(*) FROM audit_logs
			         WHERE organization_id = $1 AND action = 'destination.blocked'
			           AND metadata->>'url_defanged' LIKE '%[:]%'
			           AND metadata->>'url_defanged' NOT LIKE '%://%'`,
			Min:   3,
			Shows: "that a hostile URL is inert in the column, not at display time",
		},
		{
			Milestone: "M31", Feature: "A dispute waiting for review",
			Query: `SELECT count(*) FROM destination_disputes WHERE status = 'open'`,
			Min:   1,
			Shows: "the review queue with something in it",
		},
		{
			Milestone: "M31", Feature: "A dispute that was allowed",
			Query: `SELECT count(*) FROM destination_disputes WHERE status = 'allowed'`,
			Min:   1,
			Shows: "a decision that removed an entry from the blocklist",
		},
		{
			Milestone: "M31", Feature: "A dispute that was upheld",
			Query: `SELECT count(*) FROM destination_disputes WHERE status = 'upheld'`,
			Min:   1,
			Shows: "that looking and saying no is a recorded decision too",
		},
		{
			Milestone: "M32", Feature: "No reputation feed verdict anywhere",
			Query: `SELECT count(*) FROM destination_disputes
			         WHERE reason_code = 'low_confidence.feed_reputation'`,
			MaxIsZero: true,
			Shows: "that the demo enabled no feed: a feed verdict here would mean " +
				"every seeded destination was sent to a third party",
		},
		{
			Milestone: "M32.5", Feature: "Bot blocking on for exactly one link",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND bot_blocking = 'on'`,
			Min: 1, Max: 1,
			Shows: "the setting, discoverable on a link that has it",
		},
		{
			Milestone: "M32.5", Feature: "Links that inherit the domain's answer",
			Query: `SELECT count(*) FROM links
			         WHERE workspace_id IN (` + demoWorkspaces + `) AND bot_blocking = 'inherit'`,
			Min:   10,
			Shows: "precedence, which is invisible when every link says the same thing",
		},

		// The boundary. Everything below is a milestone that has not been built,
		// so the demo showing nothing is correct — and these rows are what make
		// the next person add a feature row above instead of quietly seeding
		// something the list does not mention. Turn a row into a real one when its
		// milestone lands; do not delete it.
		{
			Milestone: "M34", Feature: "Routing rules (not built yet)",
			Query:     `SELECT count(*) FROM routing_rules`,
			MaxIsZero: true,
			Shows:     "nothing: M34 is unbuilt, and seeding it would be inventing a surface",
		},
		{
			Milestone: "M41", Feature: "QR codes and campaigns (not built yet)",
			Query:     `SELECT count(*) FROM qr_codes`,
			MaxIsZero: true,
			Shows:     "nothing: M41 is unbuilt",
		},
		{
			Milestone: "M42", Feature: "Webhooks (not built yet)",
			Query:     `SELECT count(*) FROM webhooks`,
			MaxIsZero: true,
			Shows:     "nothing: M42 is unbuilt",
		},
		{
			Milestone: "M43", Feature: "Automation rules (not built yet)",
			Query:     `SELECT count(*) FROM automation_rules`,
			MaxIsZero: true,
			Shows:     "nothing: M43 is unbuilt",
		},
	}
}

// TestDemoSeederShowsEveryFeatureItClaimsTo runs the seeder the way
// `make demo-update` runs it, then checks every row of the list above.
//
// It also runs it twice, because "idempotent under --reset" is a property the
// demo target depends on at every milestone boundary: the second run must
// produce the same demo as the first, or somebody's screenshot stops matching
// the instance.
func TestDemoSeederShowsEveryFeatureItClaimsTo(t *testing.T) {
	ctx := context.Background()
	pool := newDemoDB(t)
	cfg := demoTestConfig()

	owner := claimDemoInstance(t, pool, cfg)

	first := runDemoSeed(t, ctx, pool, cfg, owner.Email)
	t.Logf("seeder runtime, first run: %s", first.Round(time.Millisecond))

	orgID, ownerID := demoScope(t, pool, owner.Email)
	counts := checkDemoCoverage(t, pool, orgID, ownerID)

	// Second run, same flags. --reset is what `task demo` passes, and the whole
	// point of it is that running it again is safe.
	second := runDemoSeed(t, ctx, pool, cfg, owner.Email)
	t.Logf("seeder runtime, second run: %s", second.Round(time.Millisecond))

	// The organization is the same; the owner is the same account. The second
	// workspace and the seeded people are new rows with new ids, which is why the
	// comparison is on what the demo shows rather than on identifiers.
	orgID, ownerID = demoScope(t, pool, owner.Email)
	again := checkDemoCoverage(t, pool, orgID, ownerID)

	for i, feature := range demoCoverage() {
		if counts[i] != again[i] {
			t.Errorf("`lctl demo --reset` is not idempotent: %s (%s) counted %d "+
				"on the first run and %d on the second. Running demo-update twice "+
				"must produce the same demo; something the seeder writes has no "+
				"matching line in demoResetPhase2.",
				feature.Feature, feature.Milestone, counts[i], again[i])
		}
	}
}

// checkDemoCoverage evaluates every row and returns the counts, so the caller can
// compare two runs.
func checkDemoCoverage(t *testing.T, pool *pgxpool.Pool, orgID, ownerID uuid.UUID) []int64 {
	t.Helper()
	features := demoCoverage()
	counts := make([]int64, len(features))

	for i, f := range features {
		// Postgres refuses a parameter the statement does not mention, and the
		// boundary rows below name a whole table rather than a scope, so each
		// query is given exactly the placeholders it uses.
		var args []any
		if strings.Contains(f.Query, "$1") {
			args = append(args, orgID)
		}
		if strings.Contains(f.Query, "$2") {
			args = append(args, ownerID)
		}
		var n int64
		if err := pool.QueryRow(context.Background(), f.Query, args...).Scan(&n); err != nil {
			t.Fatalf("%s %s: %v\nquery: %s", f.Milestone, f.Feature, err, f.Query)
		}
		counts[i] = n

		switch {
		case f.MaxIsZero && n != 0:
			t.Errorf("%s — %s: %d rows, want none.\nThe demo shows %s.",
				f.Milestone, f.Feature, n, f.Shows)
		case f.MaxIsZero:
		case n < f.Min:
			t.Errorf("%s — %s: %d rows, want at least %d.\n"+
				"Without it the demo loses %s.\n"+
				"Extend cmd/lctl/demo_phase2.go so the demo instance shows the "+
				"feature instead of an empty page.",
				f.Milestone, f.Feature, n, f.Min, f.Shows)
		case f.Max > 0 && n > f.Max:
			t.Errorf("%s — %s: %d rows, want at most %d.\nThe demo shows %s.",
				f.Milestone, f.Feature, n, f.Max, f.Shows)
		}
	}
	return counts
}

// runDemoSeed runs the seeder with the flags `task demo` uses, and times it.
//
// The figure is logged rather than asserted. A threshold here would be a
// measurement of the machine the tests happen to be running on, and the property
// that matters — that `make demo-update` stays pleasant at a milestone
// boundary — is one a person judges from the number.
func runDemoSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cfg config.Config, email string) time.Duration {
	t.Helper()
	start := time.Now()
	if err := demoSeed(ctx, pool, cfg, demoOptions{
		user: email, days: 30, reset: true, volume: 4, prng: 1,
	}); err != nil {
		t.Fatalf("seed the demo: %v", err)
	}
	return time.Since(start)
}

// demoScope resolves the organization and owner every coverage query is written
// against.
func demoScope(t *testing.T, pool *pgxpool.Pool, email string) (orgID, userID uuid.UUID) {
	t.Helper()
	const q = `
		SELECT m.organization_id, u.id
		  FROM users u
		  JOIN memberships m ON m.user_id = u.id
		 WHERE u.email_lower = $1
		 ORDER BY m.created_at
		 LIMIT 1`
	if err := pool.QueryRow(context.Background(), q, strings.ToLower(email)).
		Scan(&orgID, &userID); err != nil {
		t.Fatalf("resolve the demo organization: %v", err)
	}
	return orgID, userID
}

// claimDemoInstance registers the first account, which is what somebody visiting
// a fresh instance does before anything can be seeded into it.
func claimDemoInstance(t *testing.T, pool *pgxpool.Pool, cfg config.Config) *auth.Identity {
	t.Helper()
	svc := auth.NewService(pool, auth.ServiceConfig{Params: auth.Params{
		MemoryKiB:   cfg.Auth.Argon2MemoryKiB,
		Iterations:  cfg.Auth.Argon2Iterations,
		Parallelism: cfg.Auth.Argon2Parallelism,
	}})
	id, err := svc.Register(context.Background(), auth.RegisterInput{
		Email: "demo-owner@example.com", Name: "Demo Owner",
		Password: "demo-owner-password", IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("claim the instance: %v", err)
	}
	return id
}

// demoTestConfig is the configuration an instance runs with, as far as the
// seeder reads it.
//
// The argon2 costs are the real defaults rather than cheap test values: this test
// is also where the seeder's runtime is measured, and hashing five passwords is
// part of what it costs.
func demoTestConfig() config.Config {
	var cfg config.Config
	cfg.BaseURL = "http://localhost:8080"
	cfg.Auth.Argon2MemoryKiB = auth.DefaultParams.MemoryKiB
	cfg.Auth.Argon2Iterations = auth.DefaultParams.Iterations
	cfg.Auth.Argon2Parallelism = auth.DefaultParams.Parallelism
	cfg.Auth.InviteTTL = 168 * time.Hour
	return cfg
}

// newDemoDB creates a migrated database of its own and drops it afterwards.
//
// Its own rather than the integration suite's template: the suite drops and
// recreates that template in its TestMain, and two package test binaries running
// concurrently would race for it. One database, migrated once, costs a couple of
// seconds against a seeder that takes longer than that anyway.
func newDemoDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, demoDSNFor("postgres"))
	if err != nil {
		t.Fatalf("connect to the maintenance database: %v\n\n"+
			"These tests need Postgres. Start it with `make up`, and run them with "+
			"`make test-integration` so TEST_DATABASE_URL is set.", err)
	}
	defer admin.Close()

	name := "t_demo_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")[:16]
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.New(context.Background(), demoDSNFor("postgres"))
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
	})

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, demoDSNFor(name), quiet); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	pool, err := pgxpool.New(ctx, demoDSNFor(name))
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := store.EnsurePartitions(ctx, pool, store.PartitionLookahead); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	return pool
}

// demoDSNFor rewrites the database name in TEST_DATABASE_URL.
const demoFallbackDSN = "postgres://linkctrl:devpassword@localhost:55432/linkctrl?sslmode=disable"

func demoDSNFor(name string) string {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = demoFallbackDSN
	}
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn
	}
	rest := ""
	if j := strings.Index(dsn[i:], "?"); j >= 0 {
		rest = dsn[i+j:]
	}
	return dsn[:i+1] + name + rest
}
