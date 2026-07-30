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
	keys, err := auth.NewAPIKeyService(pools.App, authSvc, auth.APIKeyConfig{
		Pepper: []byte(cfg.APIKeyPepper.Reveal()),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	in := auth.CreateAPIKeyInput{Name: *name, Scopes: splitScopes(*scopes)}
	if *expiry > 0 {
		at := time.Now().Add(*expiry)
		in.ExpiresAt = &at
	}

	return withKeyService(context.Background(), *email,
		func(ctx context.Context, keys *auth.APIKeyService, actor *auth.Identity) error {
			created, err := keys.Create(ctx, actor, in)
			if err != nil {
				return err
			}

			// The token goes to stdout on its own line and everything else to
			// stderr, so `lctl apikey create ... > key.txt` captures the key and
			// nothing else.
			fmt.Fprintf(os.Stderr, "created %s (%s) for %s\nscopes: %s\n",
				created.Name, created.Prefix, actor.Email, strings.Join(created.Scopes, ", "))
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
			fmt.Fprintln(w, "ID\tPREFIX\tNAME\tSTATE\tLAST USED\tSCOPES")
			for _, k := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					k.ID, k.Prefix, k.Name, keyState(k), stamp(k.LastUsedAt),
					strings.Join(k.Scopes, ","))
			}
			return w.Flush()
		})
}

func apikeyRevoke(args []string) error {
	fs := flag.NewFlagSet("apikey revoke", flag.ContinueOnError)
	var (
		email = fs.String("user", "", "email address that owns the key")
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
	default:
		return "active"
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
