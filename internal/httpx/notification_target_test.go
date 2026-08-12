package httpx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// M48's claim about notifications, in the two halves it is made in: every
// declared kind has a destination, and each destination is the right one.

// notificationKindPrefix and notificationKindMark are how a kind constant is
// recognised in the source.
//
// **Both, and the second one is what stops the scan over-reaching.** The
// identifier prefix alone would pick up `MailKind`, `MailKindExists` and the
// four `RuleKind*` constants in internal/domain — none of which is a
// notification kind, and two of which are mail template names that happen to
// share a word. Requiring a dot in the *value* narrows it to the vocabulary this
// package maps: `audit.growth`, `dispute.filed`, and the five beside them are
// all `noun.verb`, and no non-notification constant beginning with `Kind` is.
//
// The prefix is a prefix rather than an exact match because the vocabulary is
// named `KindAuditGrowth`, `KindDomainFailing` and so on. `RuleKindMatch` does
// not begin with it, which is the whole reason the scan can be this simple.
const notificationKindPrefix = "Kind"
const notificationKindMark = "."

// declaredNotificationKinds reads the vocabulary out of the source.
//
// **Discovered, never listed.** m48.md asks that "a kind added in a later phase
// breaks the build instead of silently becoming unclickable", and a test naming
// the seven kinds that exist today would do the opposite: it would pass, wrongly
// and quietly, on the day an eighth arrives. So the enumeration is the source
// tree, and the map in notification_target.go is what is checked against it.
//
// It walks internal/ rather than the two packages that hold kinds today. The
// vocabulary is already split across internal/notify and internal/dispute, which
// is the shape that made the risk section of m48.md doubt this test could be
// written at all — a third package is a matter of time, and naming two here
// would be the list this function exists to avoid.
//
// The failure direction is safe. A constant that matches the shape and is not a
// notification kind makes this test demand a mapping for it, which is a person
// reading a name rather than a defect shipping.
func declaredNotificationKinds(t *testing.T) map[string]string {
	t.Helper()

	// The test's working directory is this package; internal/ is its parent.
	root := ".."
	out := map[string]string{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// A file this cannot parse is skipped rather than fatal: the package
		// under test compiles, so unparseable Go somewhere else in internal/ is
		// somebody else's build failing and not this assertion's news. The floor
		// below is what stops a tree of unparseable files passing silently.
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			//nolint:nilerr // skipping an unparseable file is the intent
			return nil
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) || !strings.HasPrefix(name.Name, notificationKindPrefix) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, uerr := strconv.Unquote(lit.Value)
					if uerr != nil || !strings.Contains(value, notificationKindMark) {
						continue
					}
					out[value] = path + " · " + name.Name
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/ for kind constants: %v", err)
	}

	// A scan that found nothing would pass every assertion below by testing
	// nothing at all, which is the one way this whole approach fails silently.
	if len(out) < 5 {
		t.Fatalf("the scan found %d notification kinds, which is too few for this to "+
			"be testing anything real. The vocabulary is exported constants named "+
			"%s* whose value contains a %q — if that stopped being true, this "+
			"function is what has to change, not the assertion below it.",
			len(out), notificationKindPrefix, notificationKindMark)
	}
	return out
}

// TestEveryNotificationKindHasADestination is the enumeration half.
//
// It is the reason notificationTargets is a map and not a switch: "has a
// mapping" has to be a question code can ask, and a `default:` arm answers it
// for every kind including the ones nobody thought about.
func TestEveryNotificationKindHasADestination(t *testing.T) {
	declared := declaredNotificationKinds(t)

	for kind, where := range declared {
		if _, ok := notificationTargets[kind]; !ok {
			t.Errorf("the notification kind %q (%s) has no entry in "+
				"notificationTargets, so clicking one lands on the list it is "+
				"already on. Add an entry — and if it genuinely leads nowhere, add "+
				"one that returns \"\", which is a different thing from an absence "+
				"and is what the audit-growth warning does.", kind, where)
		}
	}

	// And the other direction, which is what keeps the map from outliving the
	// vocabulary: an entry for a kind nothing declares is a destination for a
	// notification that can never arrive.
	for kind := range notificationTargets {
		if _, ok := declared[kind]; !ok {
			t.Errorf("notificationTargets maps %q, which no constant in internal/ "+
				"declares. Either the kind was renamed and this entry was left, or "+
				"it is spelled by hand here — and a kind spelled by hand is a kind "+
				"that stops matching the day the constant changes.", kind)
		}
	}
}

// TestWhereEachNotificationKindLeads is the per-kind half m48.md asks for.
//
// One row per declared kind, and the table is checked for completeness against
// the same scan — so this cannot be the test that quietly stops covering a kind
// somebody added.
func TestWhereEachNotificationKindLeads(t *testing.T) {
	const disputeID = "0198c9c5-0000-7000-8000-000000000030"

	cases := []struct {
		kind string
		data map[string]any
		want string
		why  string
	}{
		{
			kind: "audit.growth",
			data: map[string]any{"bytes": int64(5583457484)},
			want: "",
			why: "the audit log has no dashboard page, and what the recipient has " +
				"to do about it is an environment variable",
		},
		{
			kind: "invite.accepted",
			data: map[string]any{"invitation": "0198c9c5-0000-7000-8000-000000000080"},
			want: "/invites",
			why:  "the reader sent it, and this is where its lifecycle is",
		},
		{
			kind: "mfa.changed",
			data: map[string]any{"change": "recovery_code_used", "recovery_codes_remaining": int64(9)},
			want: "/account",
			why: "one kind for four events, because the reader's answer to every " +
				"one of them is the same: open the account page and look at the " +
				"second factor. The data says which happened; the destination " +
				"does not have to",
		},
		{
			kind: "update.available",
			data: map[string]any{"version": "0.4.0", "running": "0.3.0"},
			want: "",
			why: "there is no page in this product that upgrades it, and the " +
				"recipient's next act is at a shell rather than in a browser — " +
				"the same answer, for the same reason, as the audit-growth warning",
		},
		{
			kind: "automation.fired",
			data: map[string]any{"rule_id": "0198c9c5-0000-7000-8000-000000000081"},
			want: "/automation",
			why:  "the rule, with its last run beside it",
		},
		{
			kind: "domain.failing",
			data: map[string]any{"hostname": "go.example.com"},
			want: "/domains",
			why:  "where the verify button is",
		},
		{
			kind: "domain.unverified",
			data: map[string]any{"hostname": "go.example.com"},
			want: "/domains",
			why:  "the same page, and the same button",
		},
		{
			kind: "dispute.filed",
			data: map[string]any{"dispute_id": disputeID},
			want: "/disputes?all=1#dispute-" + disputeID,
			why: "the row that is waiting, not the queue it is in — the recipients " +
				"are reviewers, so the queue is a page they can open",
		},
		{
			kind: "dispute.filed",
			data: map[string]any{},
			want: "/disputes",
			why: "an unreadable id lands on the whole queue rather than on a " +
				"fragment that names nothing",
		},
		{
			kind: "dispute.decided",
			data: map[string]any{"dispute_id": disputeID, "status": "allowed"},
			want: "/links",
			why: "the reader filed it and is not a reviewer; what an allowed " +
				"dispute means for them is that they can create the link now",
		},
		{
			kind: "dispute.decided",
			data: map[string]any{"dispute_id": disputeID, "status": "upheld"},
			want: "",
			why: "no page shows a refusal that stands, and sending a filer to a " +
				"queue they cannot read would be sending them to a 403",
		},
	}

	for _, tc := range cases {
		if got := notificationTarget(tc.kind, tc.data); got != tc.want {
			t.Errorf("%s with %v leads to %q, want %q — %s",
				tc.kind, tc.data, got, tc.want, tc.why)
		}
	}

	// The table covers the vocabulary, checked against the scan rather than
	// against itself.
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.kind] = true
	}
	var missing []string
	for kind := range declaredNotificationKinds(t) {
		if !covered[kind] {
			missing = append(missing, kind)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("no case in this table names %s. m48.md asks for a test per kind, "+
			"and a table that is allowed to be short of one is a table that will be.",
			strings.Join(missing, ", "))
	}
}

// TestAnUnknownNotificationKindLandsOnTheList is the branch the two tests above
// exist to keep unreachable, asserted anyway.
//
// Rows outlive binaries. A kind written by a newer instance, or one renamed
// without a migration, reaches this function as a string nothing maps — and the
// answer has to be the list the reader is on rather than an empty redirect or a
// 500 on a page that was working.
func TestAnUnknownNotificationKindLandsOnTheList(t *testing.T) {
	if _, ok := notificationTargets["nothing.declares.this"]; ok {
		t.Fatal("the sentinel kind this test uses is mapped, so it is testing the " +
			"wrong branch")
	}
	if got := notificationTarget("nothing.declares.this", nil); got != "/notifications" {
		t.Errorf("an unknown kind leads to %q, want /notifications", got)
	}
}

// TestEveryPanelRouteIsMounted is the half internal/ui cannot check.
//
// TestEveryPanelIsAlsoACompletePage renders the routes' pages and asserts each
// is a complete page; it has no router, so it cannot know the route exists. The
// literals below are the hrefs that ui test compares against — the reviewer
// panel's `Open as a page` link, and the QR route the codes list selects a
// code through — so between the two tests the claim is closed: the href
// points at a route, and the route renders a page.
func TestEveryPanelRouteIsMounted(t *testing.T) {
	app := newAppMux()
	registerAppRoutes(maximalDeps(), app)

	for _, want := range []string{
		"GET /links/{id}/qr",
		"GET /disputes/reviewers",
	} {
		if !slices.Contains(app.patterns, want) {
			t.Errorf("%q is not registered. A panel whose contents are only "+
				"reachable inside the popup is a popup, and m48.md's requirement is "+
				"that it is a route first.", want)
		}
	}
}
