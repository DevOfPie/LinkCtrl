package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
	"github.com/DevOfPie/LinkCtrl/internal/platform/postgres"
)

// instanceCmd implements `lctl instance`.
//
// **This is the one subcommand that does not act as a named user**, and the
// difference is the whole of why it exists (F140). `lctl apikey` resolves an
// identity from `--user` and calls the same service methods the API does, so an
// operator with database access cannot mint a key that user's role does not
// grant. There is no equivalent trick available here: the operation is *appoint
// somebody who administers this instance*, and D98 puts that outside the
// permission system on purpose — `instance.admin` is not in
// `auth.InstanceGrantable`, so no in-product surface confers it and no holder can
// mint another holder. Asking for a `--user` to act as would be asking the
// operator which identity to pretend to be.
//
// What authorizes it instead is the shell. Whoever runs `lctl` has the database
// URL, the configuration and the deploy, which is the same claim to the box that
// `POST /api/v1/auth/setup` already rests on — `internal/auth`'s `Register` says
// so in as many words, and setup is where the principal is conferred in the first
// place. So this widens nothing: it reaches a state an operator could already
// reach with `psql`, and reaching it through the service is what makes the audit
// record exist.
func instanceCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lctl instance principal show|move")
	}
	switch args[0] {
	case "principal":
		return principalCmd(args[1:])
	default:
		return fmt.Errorf("unknown instance subcommand %q", args[0])
	}
}

func principalCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lctl instance principal show|move")
	}
	switch args[0] {
	case "show":
		return principalShow(args[1:])
	case "move":
		return principalMove(args[1:])
	default:
		return fmt.Errorf("unknown principal subcommand %q", args[0])
	}
}

// withInstanceService opens the pools and builds the service.
//
// The auditor is wired for the reason `lctl apikey` wires one: the write it
// performs is administrative and instance-wide, and a CLI that quietly skipped
// the record would make the shell the one way to change who administers a box
// without leaving a trail — which is the state F140 is about, arriving by a new
// route. The logger is discarded so a stray line cannot corrupt output being
// piped somewhere; the service only logs a failed audit write, which this command
// would rather report itself.
func withInstanceService(ctx context.Context, fn func(context.Context, *instance.Service) error) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	pools, err := postgres.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pools.Close()

	return fn(ctx, instance.NewService(pools.App, instance.Config{
		Audit: audit.NewService(pools.App),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
}

func principalShow(args []string) error {
	fs := flag.NewFlagSet("instance principal show", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: lctl instance principal show

Prints the account that administers this instance: the one that appoints and
removes the people who work the destination-dispute queue, and the only one that
reads the audit records of acts belonging to no organization.

There is normally exactly one. Two means somebody has written to instance_grants
directly, and `+"`move`"+` will refuse until that is resolved.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	return withInstanceService(context.Background(), func(ctx context.Context, svc *instance.Service) error {
		held, err := svc.Principals(ctx)
		if err != nil {
			return err
		}
		if len(held) == 0 {
			// Not an error. A database migrated before anybody claimed it is in
			// exactly this state, and saying so is more use than a non-zero exit.
			fmt.Println("this instance has no principal; the account that completes " +
				"setup becomes one")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tEMAIL\tNAME")
		for _, p := range held {
			fmt.Fprintf(w, "%s\t%s\t%s\n", p.UserID, p.Email, p.Name)
		}
		return w.Flush()
	})
}

func principalMove(args []string) error {
	fs := flag.NewFlagSet("instance principal move", flag.ContinueOnError)
	var (
		email = fs.String("to", "", "email address of the account to make the instance principal")
		// The same guard `lctl seed` and `lctl demo` carry, for the same reason
		// and against the same mistake: this is not something anybody should be
		// able to do to a live instance by pressing up-arrow in the wrong
		// terminal. It is not a claim that the operation is wrong in production —
		// production is where it is needed — which is why it asks rather than
		// refuses, and why the flag is spelled the way the other two spell it.
		force = fs.Bool("force", false, "allow the move when APP_ENV=production")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: lctl instance principal move --to <email> [--force]

Moves the instance principal onto an existing account, and off every account that
holds it. Use it when the account that claimed this instance can no longer be
reached: a forgotten password with no mailer configured, or a colleague who has
left. There is no account recovery in this product, so losing that password and
losing the principal are the same event.

It is a move and never an addition. Exactly one account holds the principal
afterwards, checked before the change commits — an operation that could create a
second one would defeat the bound that makes delegating dispute review safe, and
that bound is the reason this is a command rather than a page.

The account must already exist. This does not create one, does not change a
password, and does not touch anybody appointed to review disputes: they keep what
they were given.

The change is written to the instance-wide audit log with the actor recorded as
`+"`system`"+`, because nobody signed in to make it.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*email) == "" {
		return fmt.Errorf("--to is required (the email address of the new principal)")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.AppEnv.IsProduction() && !*force {
		return fmt.Errorf("refusing to move the instance principal with APP_ENV=production; " +
			"pass --force if you mean it")
	}

	return withInstanceService(context.Background(), func(ctx context.Context, svc *instance.Service) error {
		moved, err := svc.MovePrincipal(ctx, *email)
		if err != nil {
			return err
		}
		fmt.Printf("the instance principal is %s\n", moved.To.Email)
		// Named rather than counted, because "who lost it" is the question
		// afterwards and an operator running this has usually just been told a
		// name by somebody. An empty set is reported too: it means the account
		// already held it, and silence there reads like a failure.
		if len(moved.From) == 0 {
			fmt.Println("no other account held it")
			return nil
		}
		for _, p := range moved.From {
			fmt.Printf("taken from %s\n", p.Email)
		}
		return nil
	})
}
