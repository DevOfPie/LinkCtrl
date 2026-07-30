// Command linkctrl is the LinkCtrl server.
//
// It is deliberately a single binary serving the redirect hot path, the REST
// API, the dashboard, and the background job scheduler. Plan.md lists these as
// separate logical services and the internal package boundaries follow those
// seams, but shipping eleven containers for an MVP would make `docker compose
// up` hostile to the self-hosters this project is for.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/DevOfPie/LinkCtrl/internal/build"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "linkctrl: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("linkctrl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		showVersion = fs.Bool("version", false, "print version information and exit")
		asJSON      = fs.Bool("json", false, "with --version, print as JSON")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: linkctrl [flags]\n\nFlags:\n")
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

	// The server is wired up in M1. Until then the binary exists so that the
	// build, the Dockerfile, and the version stamping can all be verified.
	return fmt.Errorf("server not implemented yet; run with --version")
}
