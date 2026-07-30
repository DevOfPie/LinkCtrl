// Command lctl is the LinkCtrl operator CLI.
//
// It shares the server's configuration loading and database layer, so anything
// it reports is what the server would see. Subcommands arrive with the
// milestones that need them: migrate in M2, user in M4, apikey in M10, seed in
// M8.
package main

import (
	"fmt"
	"os"

	"github.com/DevOfPie/LinkCtrl/internal/build"
	"github.com/DevOfPie/LinkCtrl/internal/config"
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
  config check    Validate configuration and report every problem at once
  version         Print version information
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
		if _, err := config.Load(); err != nil {
			fmt.Fprintf(os.Stderr, "configuration is invalid:\n\n%v\n", err)
			return fmt.Errorf("invalid configuration")
		}
		fmt.Println("configuration OK")
		return nil

	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}
