// Command lctl is the LinkCtrl operator CLI.
//
// It shares the server's configuration loading and database layer, so anything
// it reports is what the server would see. Subcommands arrive with the
// milestones that need them: user in M4, apikey in M10, seed in M8.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/DevOfPie/LinkCtrl/internal/build"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/platform/postgres"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "lctl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: lctl <command>

Commands:
  config check       Validate configuration, reporting every problem at once
  migrate up         Apply pending migrations, then ensure partitions exist
  migrate down       Roll back the most recent migration
  migrate status     Show applied and pending migrations
  partitions ensure  Create partitions for the current and next months
  apikey create      Issue an API key   --user --name --scopes [--expires-in]
  apikey list        List a user's API keys                          --user
  apikey revoke      Revoke an API key                         --user --id
  version            Print version information
`)
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("no command given")
	}

	switch args[0] {
	case "version":
		fmt.Println(build.Get())
		return nil

	case "config":
		if len(args) < 2 || args[1] != "check" {
			usage()
			return fmt.Errorf("unknown config subcommand")
		}
		if _, err := loadConfig(); err != nil {
			return err
		}
		// Warnings go to stderr and do not change the exit status: a stale line in
		// a .env is worth saying out loud and is not a reason to fail a deploy
		// check.
		for _, w := range config.RemovedInUse() {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
		fmt.Println("configuration OK")
		return nil

	case "migrate":
		return migrate(args[1:])

	case "apikey":
		return apikeyCmd(args[1:])

	case "partitions":
		if len(args) < 2 || args[1] != "ensure" {
			usage()
			return fmt.Errorf("unknown partitions subcommand")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return ensurePartitions(context.Background(), cfg)

	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// loadConfig prints aggregated validation errors as a list rather than
// collapsing them into one line, which is the whole point of collecting them.
func loadConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration is invalid:\n\n%v\n\n"+
			"See .env.example for the full reference.\n", err)
		return cfg, fmt.Errorf("invalid configuration")
	}
	return cfg, nil
}

func migrate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lctl migrate up|down|status")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch args[0] {
	case "up":
		log := observability.NewLogger(cfg, os.Stdout)
		if err := store.Migrate(ctx, cfg.DB.URL.Reveal(), log); err != nil {
			return err
		}
		// Partitions are created here rather than inside a migration, because
		// the same code must also run on a schedule as months roll over.
		return ensurePartitions(ctx, cfg)

	case "down":
		return store.Down(ctx, cfg.DB.URL.Reveal())

	case "status":
		lines, err := store.Status(ctx, cfg.DB.URL.Reveal())
		if err != nil {
			return err
		}
		fmt.Printf("%-6s %-8s %-40s %s\n", "VER", "STATE", "NAME", "APPLIED")
		for _, l := range lines {
			fmt.Println(l)
		}
		return nil

	default:
		return fmt.Errorf("unknown migrate subcommand %q", args[0])
	}
}

func ensurePartitions(ctx context.Context, cfg config.Config) error {
	pools, err := postgres.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pools.Close()

	created, err := store.EnsurePartitions(ctx, pools.App, store.PartitionLookahead)
	if err != nil {
		return err
	}
	fmt.Printf("partitions ensured (%d created)\n", created)
	return nil
}
