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
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/platform/postgres"
)

// apikeyCmd implements `lctl apikey`.
//
// The CLI acts as a named user rather than as root: every subcommand resolves
// an identity from --user and then calls the same service methods the API
// does. So `lctl apikey create` cannot mint a key with scopes that user's role
// does not grant — the one place an operator with database access could
// trivially bypass RBAC is the one place it would be least obvious.
//
// It exists because the first key on a fresh instance has to come from
// somewhere. Creating one through the API needs a session, and a headless
// deployment has no browser.
func apikeyCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lctl apikey create|list|revoke")
	}
	switch args[0] {
	case "create":
		return apikeyCreate(args[1:])
	case "list":
		return apikeyList(args[1:])
	case "revoke":
		return apikeyRevoke(args[1:])
	default:
		return fmt.Errorf("unknown apikey subcommand %q", args[0])
	}
}

// withKeyService opens the pools and builds the key service and an identity for
// the named user.
func withKeyService(ctx context.Context, email string,
	fn func(context.Context, *auth.APIKeyService, *auth.Identity) error,
) error {
	if strings.TrimSpace(email) == "" {
		return fmt.Errorf("--user is required (the email address the key acts as)")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	pools, err := postgres.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pools.Close()

	authSvc := auth.NewService(pools.App, auth.ServiceConfig{
		Params: auth.Params{
			MemoryKiB:   cfg.Auth.Argon2MemoryKiB,
			Iterations:  cfg.Auth.Argon2Iterations,
			Parallelism: cfg.Auth.Argon2Parallelism,
		},
	})

	// Discard the log: the service only logs background usage flushing, which
	// the CLI never runs, and a stray record would corrupt output being piped
	// into something.
	// The auditor is wired for the reason main.go wires it: revoking somebody
	// else's key writes a record, and a CLI that quietly skipped it would make
	// the shell the one way to stop a credential without leaving a trail.
	keys, err := auth.NewAPIKeyService(pools.App, authSvc, auth.APIKeyConfig{
		Pepper:  []byte(cfg.APIKeyPepper.Reveal()),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auditor: audit.NewService(pools.App),
	})
	if err != nil {
		return err
	}

	identity, err := authSvc.IdentityForEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("resolve user %s: %w", email, err)
	}

	return fn(ctx, keys, identity)
}

func apikeyCreate(args []string) error {
	fs := flag.NewFlagSet("apikey create", flag.ContinueOnError)
	var (
		email  = fs.String("user", "", "email address of the user the key acts as")
		name   = fs.String("name", "", "human-readable name, e.g. \"ci-deploy\"")
		scopes = fs.String("scopes", "", "comma-separated permission slugs, e.g. links.read,links.create")
		expiry = fs.Duration("expires-in", 0, "lifetime, e.g. 720h; omit for a key that never expires")
		// Off by default here for the reason it is off by default everywhere:
		// reaching every workspace is a choice somebody makes, not one they get
		// by leaving a flag alone. The service refuses it unless the named user's
		// own membership is organization-wide, so this flag asks rather than
		// grants — which is the point of the CLI acting as a named user at all.
		orgWide = fs.Bool("org-wide", false,
			"not pinned to one workspace: each request resolves one the way a sign-in does, "+
				"within the organization the key is issued in")
		// M54's axis, and it only means anything with --org-wide. Off by default
		// for the same reason --org-wide is off by default, one tier up: an
		// unpinned key is account-wide unless somebody says otherwise, and saying
		// otherwise is what pins it. The flag is the *narrowing* one, so leaving it
		// alone never produces a credential narrower than the operator expected —
		// it produces the model the product now has, which is the thing the help
		// text has to make legible at the moment of minting.
		pin = fs.Bool("pin", false,
			"with --org-wide: pin the key to this organization instead of letting it "+
				"reach every organization its owner belongs to")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	in := auth.CreateAPIKeyInput{Name: *name, Scopes: splitScopes(*scopes), OrgWide: *orgWide}
	if *expiry > 0 {
		at := time.Now().Add(*expiry)
		in.ExpiresAt = &at
	}

	return withKeyService(context.Background(), *email,
		func(ctx context.Context, keys *auth.APIKeyService, actor *auth.Identity) error {
			// Resolved here rather than at flag-parse time because the
			// organization is a property of the identity, and the identity does
			// not exist until the pools are open.
			if *pin {
				org := actor.OrgID
				in.OrganizationID = &org
			}
			created, err := keys.Create(ctx, actor, in)
			if err != nil {
				return err
			}

			// The token goes to stdout on its own line and everything else to
			// stderr, so `lctl apikey create ... > key.txt` captures the key and
			// nothing else.
			fmt.Fprintf(os.Stderr, "created %s (%s) for %s\nreach: %s\nscopes: %s\n",
				created.Name, created.Prefix, actor.Email, keyReach(created.APIKeyInfo),
				strings.Join(created.Scopes, ", "))
			fmt.Fprintln(os.Stderr, "this is the only time the key is shown:")
			fmt.Println(created.Key)
			return nil
		})
}

func apikeyList(args []string) error {
	fs := flag.NewFlagSet("apikey list", flag.ContinueOnError)
	email := fs.String("user", "", "email address whose keys to list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	return withKeyService(context.Background(), *email,
		func(ctx context.Context, keys *auth.APIKeyService, actor *auth.Identity) error {
			list, err := keys.List(ctx, actor)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("no API keys")
				return nil
			}

			// The id is shown because revoke takes it; the prefix is shown
			// because that is what appears in a caller's configuration.
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tPREFIX\tNAME\tREACH\tSTATE\tLAST USED\tSCOPES")
			for _, k := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					k.ID, k.Prefix, k.Name, keyReach(k), keyState(k), stamp(k.LastUsedAt),
					strings.Join(k.Scopes, ","))
			}
			return w.Flush()
		})
}

func apikeyRevoke(args []string) error {
	fs := flag.NewFlagSet("apikey revoke", flag.ContinueOnError)
	var (
		email = fs.String("user", "", "email address to act as: the key's owner, or an administrator holding apikeys.write across the organization")
		id    = fs.String("id", "", "key id, as shown by lctl apikey list")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	keyID, err := uuid.Parse(strings.TrimSpace(*id))
	if err != nil {
		return fmt.Errorf("--id must be the key's uuid: %w", err)
	}

	return withKeyService(context.Background(), *email,
		func(ctx context.Context, keys *auth.APIKeyService, actor *auth.Identity) error {
			if err := keys.Revoke(ctx, actor, keyID); err != nil {
				return err
			}
			fmt.Printf("revoked %s\n", keyID)
			return nil
		})
}

func keyState(k auth.APIKeyInfo) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case k.ExpiresAt != nil && !k.ExpiresAt.After(time.Now()):
		return "expired"
	// A rotated predecessor still authenticates until its window closes, so it
	// is neither "active" nor "revoked" and calling it either would be a lie an
	// operator acts on. The deadline is in the word.
	case k.GraceExpiresAt != nil && k.GraceExpiresAt.After(time.Now()):
		return "rotated, until " + stamp(k.GraceExpiresAt)
	default:
		return "active"
	}
}

// keyReach names the reach chosen at creation. Spelled out rather than left to
// a blank column, because "bound to one workspace", "valid across one
// organization" and "valid across the account" are three different credentials
// and the difference is invisible otherwise.
//
// Two columns in the row, three answers here: OrgWide is the workspace tier and
// OrganizationID is the tenancy tier, and only the second gained a state in M54.
func keyReach(k auth.APIKeyInfo) string {
	switch {
	case !k.OrgWide:
		return "workspace"
	case k.OrganizationID != nil:
		return "organization"
	default:
		return "account"
	}
}

func stamp(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func splitScopes(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
