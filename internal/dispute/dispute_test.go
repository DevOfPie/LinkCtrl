package dispute

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// codeOf returns the reason code of a refusal to file, or "" when it accepted.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("refusal is not a validation error: %T %v", err, err)
	}
	if len(ve) == 0 {
		t.Fatal("refusal carries no field errors")
	}
	if ve[0].Field != "url" {
		t.Errorf("refusal is against field %q, want url", ve[0].Field)
	}
	return ve[0].Code
}

// TestOnlyALowConfidenceRefusalCanBeDisputed is the milestone's first bullet.
//
// The two tiers with no dispute path are the two that protect somebody other
// than the person appealing. Phase 1's refusals keep this instance from being
// used as an SSRF proxy — an owner who could approve 169.254.169.254 on request
// would have turned the queue into exactly that — and the embedded list is meant
// to cost a rebuild to overrule, which a click would not.
func TestOnlyALowConfidenceRefusalCanBeDisputed(t *testing.T) {
	lowConfidence := func(rule string) link.Verdict {
		return link.Verdict{
			Host:  "evil.example",
			Block: &link.Block{Tier: link.TierLowConfidence, Rule: rule, Detail: "listed"},
			Errs:  domain.ValidationErrors{{Code: link.TierLowConfidence.Code(rule)}},
		}
	}

	cases := []struct {
		name    string
		verdict link.Verdict
		want    string // "" means the dispute is accepted
	}{
		{
			name: "private address is unappealable",
			verdict: link.Verdict{
				Block: &link.Block{
					Tier: link.TierUnappealable, Rule: link.RulePrivateAddress,
					Detail: "destination must not be a private, loopback or link-local address",
				},
				Errs: domain.ValidationErrors{{
					Code: link.TierUnappealable.Code(link.RulePrivateAddress),
				}},
			},
			want: CodeNotDisputable,
		},
		{
			name: "forbidden scheme is unappealable",
			verdict: link.Verdict{
				Block: &link.Block{
					Tier: link.TierUnappealable, Rule: link.RuleSchemeForbidden, Detail: "no",
				},
				Errs: domain.ValidationErrors{{
					Code: link.TierUnappealable.Code(link.RuleSchemeForbidden),
				}},
			},
			want: CodeNotDisputable,
		},
		{
			name: "the embedded list is not appealable either",
			verdict: link.Verdict{
				Host: "known-bad.example",
				Block: &link.Block{
					Tier: link.TierHighConfidence, Rule: link.RuleEmbeddedHost, Detail: "no",
				},
				Errs: domain.ValidationErrors{{
					Code: link.TierHighConfidence.Code(link.RuleEmbeddedHost),
				}},
			},
			want: CodeNotDisputable,
		},
		{
			name:    "a destination nothing refuses has nothing to dispute",
			verdict: link.Verdict{Normalized: "https://example.com/", Host: "example.com"},
			want:    CodeNotBlocked,
		},
		{
			name: "a typo is not an appeal",
			verdict: link.Verdict{Errs: domain.ValidationErrors{{
				Code: "no_scheme", Message: "destination must start with http:// or https://",
			}}},
			want: "no_scheme",
		},
		{name: "operator blocklist", verdict: lowConfidence(link.RuleOperatorBlocklist)},
		{name: "shortener chain", verdict: lowConfidence(link.RuleShortenerChain)},
		{name: "punycode homograph", verdict: lowConfidence(link.RulePunycodeHomograph)},
		{name: "credentials in the URL", verdict: lowConfidence(link.RuleURLCredentials)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codeOf(t, disputable(tc.verdict))
			if got != tc.want {
				t.Errorf("disputable = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOnlyListedRulesCanBeLifted pins what an allow is able to do.
//
// An allow deletes a row from blocked_destinations and nothing else. Two of the
// four low-confidence rules are computed from the destination every time it is
// judged, so there is no row to delete and — per 01500, which has no allow
// column on purpose — none anybody may add. Reporting those as liftable would
// put a button in front of an owner that could only ever fail.
func TestOnlyListedRulesCanBeLifted(t *testing.T) {
	listed := map[string]bool{
		link.RuleOperatorBlocklist: true,
		link.RuleShortenerChain:    true,
		link.RulePunycodeHomograph: false,
		link.RuleURLCredentials:    false,
	}
	for rule, want := range listed {
		if got := liftableRules[link.TierLowConfidence.Code(rule)]; got != want {
			t.Errorf("%s liftable = %v, want %v", rule, got, want)
		}
	}
	// A tier that is not appealable must never be liftable either, whatever
	// somebody later adds to the map.
	for _, tier := range []link.Tier{link.TierUnappealable, link.TierHighConfidence} {
		for code := range liftableRules {
			if strings.HasPrefix(code, string(tier)+".") {
				t.Errorf("%s is marked liftable; only the low-confidence tier may be lifted", code)
			}
		}
	}
}

// --- the grep gate -----------------------------------------------------------

// outboundHTTP matches a symbol that would make this feature fetch something.
//
// Deliberately blunt and deliberately over-broad. A false positive here is a
// line somebody has to justify in a comment; a false negative is the SSRF the
// destination validator exists to refuse, arriving as a convenience feature.
//
// net/http is not banned outright, because the handlers are HTTP handlers and
// live in this scan. What is banned is its *client* half, plus every other way
// out of the process this code could plausibly reach for: a raw dial, a name
// lookup, a reverse proxy, a subprocess.
var outboundHTTP = regexp.MustCompile(
	`\b(?:` +
		`http\.(?:Get|Head|Post|PostForm|Do|NewRequest|NewRequestWithContext|Client|DefaultClient|` +
		`DefaultTransport|Transport|ReadResponse|ProxyFromEnvironment)` +
		`|httputil\.` +
		`|net\.(?:Dial|DialTimeout|DialIP|DialTCP|DialUDP|Dialer|Resolver|LookupHost|LookupIP|LookupAddr|LookupCNAME)` +
		`|exec\.(?:Command|CommandContext)` +
		`|smtp\.|ftp\.|websocket\.` +
		`)`)

// queueSources are every file the review queue is made of.
//
// Named rather than globbed over internal/httpx, because that package is the
// HTTP server and legitimately contains client-shaped code elsewhere. A file
// added to this feature and not listed here is caught by the companion check
// below, which fails when a *dispute* file exists that this list does not name.
var queueSources = []string{
	"internal/dispute",
	"internal/httpx/api_disputes.go",
	"internal/httpx/web_disputes.go",
	"internal/ui/templates/pages/disputes.html",
	"internal/store/query/disputes.sql",
	"internal/store/migrations/01600_destination_disputes.sql",
}

// TestTheQueueFetchesNothing is the milestone's third bullet, as a gate.
//
// No code path fetches, previews or screenshots a submitted URL. The queue hands
// an instance owner a URL a stranger chose; fetching it would make the server
// the one that visits, from inside the network the validator's private-address
// refusals exist to protect — and it would arrive looking like a feature
// request, which is why this is a test rather than a note.
func TestTheQueueFetchesNothing(t *testing.T) {
	const root = "../.."

	for _, src := range queueSources {
		for _, path := range filesUnder(t, filepath.Join(root, src)) {
			// Test files are exempt, and only test files. They ship in no binary
			// and serve no request, and this file itself has to name the banned
			// symbols in order to ban them.
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			b, err := os.ReadFile(path) //nolint:gosec // G304: paths come from queueSources
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if m := outboundHTTP.FindString(line); m != "" {
					t.Errorf("%s:%d uses %s. Nothing behind the review queue may fetch "+
						"a submitted URL — a preview, a screenshot, a favicon or a "+
						"liveness check is exactly the SSRF the destination validator "+
						"refuses, arriving as a convenience feature.",
						filepath.ToSlash(path), i+1, m)
				}
			}
			// A template pulling in an external asset is the same failure wearing
			// HTML: the *browser* does the fetching, from the owner's machine.
			if strings.HasSuffix(path, ".html") &&
				(strings.Contains(string(b), "http://") || strings.Contains(string(b), "https://")) {
				t.Errorf("%s references an absolute URL; the queue's page is built from "+
					"local assets only", filepath.ToSlash(path))
			}
		}
	}
}

// TestEveryQueueFileIsScanned closes the gap the list above would otherwise
// leave: a second handler file that nothing scans.
func TestEveryQueueFileIsScanned(t *testing.T) {
	const root = "../.."

	scanned := map[string]bool{}
	for _, src := range queueSources {
		for _, p := range filesUnder(t, filepath.Join(root, src)) {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				t.Fatal(err)
			}
			scanned[filepath.ToSlash(rel)] = true
		}
	}

	var missed []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "node_modules", "dbgen", "test":
				return filepath.SkipDir
			}
			return nil
		}
		name := strings.ToLower(d.Name())
		if !strings.Contains(name, "dispute") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if slash := filepath.ToSlash(rel); !scanned[slash] {
			missed = append(missed, slash)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	sort.Strings(missed)
	if len(missed) > 0 {
		t.Errorf("these files belong to the dispute feature but no outbound-HTTP scan "+
			"covers them: %v. Add them to queueSources.", missed)
	}
}

// filesUnder returns path itself when it is a file, or every file beneath it.
func filesUnder(t *testing.T, path string) []string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.IsDir() {
		return []string{path}
	}
	var out []string
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", path, err)
	}
	return out
}

// TestTheServiceHasNoFieldThatCouldFetch is the structural half of the gate
// above.
//
// The scan catches a call. This catches the dependency being *held*: a Service
// that grew an http.Client field, or a Config that accepted one, would pass a
// grep until the day something called it. Nothing in this package may hold a
// type from a package that reaches the network.
func TestTheServiceHasNoFieldThatCouldFetch(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	forbidden := map[string]bool{"http": true, "net": true, "httputil": true, "exec": true, "smtp": true}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, f := range st.Fields.List {
				sel, ok := unwrap(f.Type).(*ast.SelectorExpr)
				if !ok {
					continue
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || !forbidden[id.Name] {
					continue
				}
				t.Errorf("%s: a struct field holds %s.%s. Nothing in this package "+
					"may hold something that can reach the network.",
					name, id.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

// unwrap strips pointers and slices, so *http.Client is seen for what it is.
func unwrap(e ast.Expr) ast.Expr {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ArrayType:
			e = t.Elt
		default:
			return e
		}
	}
}
