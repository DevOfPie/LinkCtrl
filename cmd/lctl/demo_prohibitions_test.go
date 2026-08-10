package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/config"
)

// The three things the demo seeder must never do (M33.5).
//
// Each is asserted twice, and the pair is deliberate. The first assertion reads
// the configuration the seeder actually builds, which is the truth about this
// run. The second scans the package's source for the machinery that would be
// needed to do the forbidden thing at all, which is the truth about the next
// person to edit it: a milestone that seeds a new feature reaches for the
// service it needs, finds it wants a mailer or a feed client, and wires one in
// without ever seeing this file. The scan is what puts a test in front of them.

// A feed sends every destination it judges to a third party (M32). A demo
// instance quietly doing that would be the worst available violation of the
// promise that feature was built to qualify — the whole point of M32 is that
// egress is opt-in, disclosed, and off by default.
func TestDemoSeederNeverEnablesAFeed(t *testing.T) {
	cfg := config.Config{}
	cfg.BaseURL = "http://localhost:8080"

	if got := demoLinkConfig(cfg, nil, nil, nil); got.Feed != nil {
		t.Errorf("demoLinkConfig sets Feed = %#v, want nil. The seeder judges "+
			"destinations, and a link service with a feed sends every one of them "+
			"to a third party.", got.Feed)
	}

	forbidden(t, map[string]string{
		"github.com/DevOfPie/LinkCtrl/internal/feed": "the demo seeder must construct no feed client",
	}, map[string]string{
		"Feed":        "link.Config.Feed must stay unset in the demo seeder",
		"FeedMetrics": "a feed the demo does not enable needs no metrics",
	})
}

// The seeder must run on a default instance, where the mailer is off (D1) and
// the copyable invitation link is the whole delivery mechanism (D27). A demo
// that needed a relay would not run for most people, and would email whoever
// happens to own the seeded addresses on an instance that has one.
func TestDemoSeederNeedsNoMailer(t *testing.T) {
	cfg := config.Config{}
	cfg.BaseURL = "http://localhost:8080"

	if got := demoInviteConfig(cfg, nil, nil, nil); got.Mail != nil {
		t.Errorf("demoInviteConfig sets Mail = %#v, want nil. In-app delivery is "+
			"the baseline and mail is the addition; the demo takes only the baseline.",
			got.Mail)
	}

	forbidden(t, map[string]string{
		"github.com/DevOfPie/LinkCtrl/internal/mail": "the demo seeder must queue no mail",
	}, map[string]string{
		"Mail": "invite.Config.Mail must stay nil in the demo seeder",
	})
}

// D7: under `closed` the environment ceiling is absolute and no invitation
// admits an account that does not exist. A seeder that raised the mode, or that
// passed NewAccounts through from it, would be making D7 false to save itself a
// query — the extra accounts are written directly instead.
func TestDemoSeederLeavesSignupModeAlone(t *testing.T) {
	cfg := config.Config{}
	cfg.BaseURL = "http://localhost:8080"
	cfg.Auth.SignupMode = config.SignupOpen

	if got := demoInviteConfig(cfg, nil, nil, nil); got.NewAccounts {
		t.Error("demoInviteConfig sets NewAccounts = true. It must be false " +
			"whatever the instance's signup mode says: redemption in the demo only " +
			"ever adds a membership to an account the seeder already wrote.")
	}

	forbidden(t, map[string]string{
		"github.com/DevOfPie/LinkCtrl/internal/signup": "the demo seeder must not consult the signup mode",
	}, map[string]string{
		"SignupMode": "the demo seeder must neither read nor set LINKCTRL_SIGNUP_MODE",
	})
}

// The demo instance must not re-verify the hostname the seeder verified through
// a stub (F162).
//
// The two halves are asserted together because neither is wrong on its own and
// the pair is fatal. `lctl demo` verifies exactly one hostname, through
// link.Service.VerifyDomain against demoChallengeResolver — the real check, and
// a resolver that lives in the seeder process and dies with it. The name it
// verifies is an RFC 2606 `.example` one, which is right: a demo must not claim
// a name somebody could own. The consequence is that the long-running server,
// which wires the real resolver, fails that check every hour and calls
// UnverifyDomain one grace window later. The demo lost its only verified custom
// hostname 24 hours after every reseed, and `demoCoverage()`'s row asserting
// exactly one verified domain could not catch it: newDemoDB seeds a throwaway
// database and asserts in the same instant, so it measures the seed and never
// the instance the job runs against.
//
// This is the assertion that can. It reads the generator rather than
// `.env.demo`, because that file is untracked, written once, and never rewritten
// — a change made only there survives nothing.
//
// The pair, stated as a rule: **while the demo verifies a hostname no resolver
// can answer for, the demo instance must have the re-verification pass off.**
// Break either half and this fails. Give the seeder a hostname a real resolver
// satisfies — a name in a zone somebody owns, with the TXT record published —
// and the honest move is to delete the setting and this test with it.
func TestTheDemoInstanceDoesNotReverifyWhatTheSeederStubbed(t *testing.T) {
	var verified []string
	for _, d := range demoDomains() {
		if d.verify {
			verified = append(verified, d.hostname)
		}
	}
	if len(verified) == 0 {
		t.Skip("the seeder verifies no hostname, so there is nothing for the hourly " +
			"pass to take away; delete this test and the setting it guards")
	}
	for _, h := range verified {
		if !strings.HasSuffix(h, ".example") {
			t.Fatalf("the demo seeder verifies %s, which is not an RFC 2606 .example "+
				"name. If a real resolver can now answer for it, re-verification is no "+
				"longer a threat to the demo and LINKCTRL_DOMAIN_VERIFY_INTERVAL=0 "+
				"should go; if it cannot, the seeder is claiming a name somebody else "+
				"may own.", h)
		}
	}

	const generator = "../../scripts/instance.sh"
	src, err := os.ReadFile(generator)
	if err != nil {
		t.Fatalf("read %s: %v", generator, err)
	}
	// The demo branch only, so the setting cannot be satisfied by the test
	// instance's half of the same file.
	body := string(src)
	demoBranch := body[strings.Index(body, `if [ "$inst" = demo ]; then`):]
	if i := strings.Index(demoBranch, "\n\telse\n"); i > 0 {
		demoBranch = demoBranch[:i]
	}
	if !strings.Contains(demoBranch, "LINKCTRL_DOMAIN_VERIFY_INTERVAL=0") {
		t.Errorf("%s does not set LINKCTRL_DOMAIN_VERIFY_INTERVAL=0 for the demo "+
			"instance, and the seeder verifies %v through a stub resolver. The hourly "+
			"pass will fail that check against the real resolver from the moment the "+
			"seed finishes and unverify the hostname one DOMAIN_VERIFY_GRACE later, "+
			"so the demo stops showing the feature M40 built a day after every "+
			"reseed — with nothing failing and no coverage row able to see it.",
			generator, verified)
	}
}

// forbidden scans this package's non-test sources for an import path or a struct
// field key that must not appear.
//
// Field keys are matched on composite literals only, which is what wiring a
// service looks like in this tree. That is narrower than searching the text and
// deliberately so: a comment naming the thing being avoided is exactly what these
// files should contain.
//
// An explicit `nil` value is allowed, because writing the field down as nil is
// how the seeder states the prohibition at the place it applies. Anything else
// is a wire-up.
func forbidden(t *testing.T, imports, fields map[string]string) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Failing rather than skipping: an unparseable file in this package is a
		// broken tree, and skipping it would let the wiring hide in the one file
		// the scan gave up on.
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}

		for _, imp := range file.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if why, bad := imports[path]; bad {
				t.Errorf("%s imports %s: %s", name, path, why)
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				why, bad := fields[key.Name]
				if !bad || isNilIdent(kv.Value) {
					continue
				}
				t.Errorf("%s sets a %s field at %s: %s",
					name, key.Name, fset.Position(kv.Pos()), why)
			}
			return true
		})
	}
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}
