//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Destination blocking (M30), against a real database.
//
// The unit tests in internal/link hold the tier boundaries and the structural
// claims. What can only be asserted here is that the three surfaces that write a
// destination all go through the check, that the refusal reaches the audit table
// with the attempted URL as evidence, and that the runtime Postgres list behaves
// like a list an owner can change.

type blockingFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	links *link.Service
	audit *audit.Service
	owner *auth.Identity
	ctx   context.Context
}

func newBlocking(t *testing.T) *blockingFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	owner, err := authSvc.Register(context.Background(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	auditSvc := audit.NewService(pool)
	links := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://lnk.test",
		SplitHosts: true, Audit: auditSvc,
	})

	// A client address, so every record written here can be checked for the
	// prefix the privacy stance requires and the address it must never hold.
	ctx := auth.WithClientIP(t.Context(), netip.MustParseAddr("198.51.100.9"))

	return &blockingFixture{t: t, pool: pool, links: links, audit: auditSvc, owner: owner, ctx: ctx}
}

// blockHost adds a row the way M31's review queue will.
func (f *blockingFixture) blockHost(host, source string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO blocked_destinations (host, source, reason) VALUES ($1, $2, 'test')`,
		host, source); err != nil {
		f.t.Fatalf("block %q: %v", host, err)
	}
}

func (f *blockingFixture) blockedRows() map[string]string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT host, source FROM blocked_destinations`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var host, source string
		if err := rows.Scan(&host, &source); err != nil {
			f.t.Fatal(err)
		}
		out[host] = source
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// rowsExcludingSeed is blockedRows without what migration 01500 shipped.
//
// A test about reconciliation is counting the rows it changed, and hard-coding
// how many shortener hosts the migration seeds would make that test fail every
// time somebody adds one — which is a thing the migration is now explicitly for.
func (f *blockingFixture) rowsExcludingSeed() map[string]string {
	out := f.blockedRows()
	for host, source := range out {
		if source == link.SourceShortener {
			delete(out, host)
		}
	}
	return out
}

// blockEvents reads the destination.blocked records, newest last.
type blockEvent struct {
	IPPrefix *string
	Meta     map[string]any
}

func (f *blockingFixture) blockEvents() []blockEvent {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `
		SELECT ip_prefix, metadata FROM audit_logs
		 WHERE action = $1 ORDER BY occurred_at, id`, audit.ActionDestinationBlocked)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()

	var out []blockEvent
	for rows.Next() {
		var e blockEvent
		var raw []byte
		if err := rows.Scan(&e.IPPrefix, &raw); err != nil {
			f.t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &e.Meta); err != nil {
			f.t.Fatalf("audit metadata is not JSON: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got none")
	}
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || len(ve) == 0 {
		t.Fatalf("error %T is not a field-level refusal: %v", err, err)
	}
	return ve[0].Code
}

// The three surfaces that write a destination as of M30, each exercised against
// each tier. The bullet says the check runs at every destination-writing surface
// that exists when this ships — link create, link update and the root redirect —
// and this is what "kept by test" means for behaviour. The structural half, which
// catches a fourth surface being added later, is
// TestEveryDestinationSurfaceGoesThroughTheCheck in internal/link.
func TestEveryDestinationSurfaceRunsTheFullTierCheck(t *testing.T) {
	tiers := []struct {
		name, url, wantCode string
	}{
		{"unappealable", "http://169.254.169.254/latest/meta-data/", "unappealable.private_address"},
		{"high confidence", "https://metadata.google.internal/computeMetadata/", "high_confidence.embedded_host"},
		{"low confidence, operator list", "https://blocked.example/x", "low_confidence.operator_blocklist"},
		{"low confidence, heuristic", "https://paypal.com@evil.example/signin", "low_confidence.url_credentials"},
	}

	surfaces := map[string]func(f *blockingFixture, url string) error{
		"link.create": func(f *blockingFixture, url string) error {
			_, err := f.links.Create(f.ctx, f.owner, link.CreateInput{URL: url})
			return err
		},
		"link.update": func(f *blockingFixture, url string) error {
			created, err := f.links.Create(f.ctx, f.owner, link.CreateInput{URL: "https://good.example/"})
			if err != nil {
				f.t.Fatalf("create a link to edit: %v", err)
			}
			_, err = f.links.Update(f.ctx, f.owner, created.ID, link.UpdateInput{URL: &url})
			return err
		},
		"domain.root_redirect": func(f *blockingFixture, url string) error {
			_, err := f.links.SetRootRedirect(f.ctx, f.owner, url)
			return err
		},
	}

	for surface, run := range surfaces {
		for _, tier := range tiers {
			t.Run(surface+"/"+tier.name, func(t *testing.T) {
				f := newBlocking(t)
				f.blockHost("blocked.example", "review")

				err := run(f, tier.url)
				if got := codeOf(t, err); got != tier.wantCode {
					t.Fatalf("refusal code = %q, want %q", got, tier.wantCode)
				}

				// The root redirect reports against its own field, so the form
				// highlights the box the operator typed in rather than a "url"
				// input that is not on the page.
				var ve domain.ValidationErrors
				_ = errors.As(err, &ve)
				wantField := "url"
				if surface == "domain.root_redirect" {
					wantField = "root_redirect_url"
				}
				if ve[0].Field != wantField {
					t.Errorf("refusal field = %q, want %q", ve[0].Field, wantField)
				}

				events := f.blockEvents()
				if len(events) != 1 {
					t.Fatalf("%d destination.blocked records, want exactly 1", len(events))
				}
				e := events[0]
				if e.Meta["code"] != tier.wantCode {
					t.Errorf("audit code = %v, want %q", e.Meta["code"], tier.wantCode)
				}
				if e.Meta["surface"] != surface {
					t.Errorf("audit surface = %v, want %q", e.Meta["surface"], surface)
				}
				// The privacy stance, on the one action a stranger's traffic can
				// provoke most easily.
				if e.IPPrefix == nil || *e.IPPrefix != "198.51.100.0/24" {
					t.Errorf("ip_prefix = %v, want the /24 and never the address", e.IPPrefix)
				}
				evidence, _ := e.Meta["url_defanged"].(string)
				if evidence == "" {
					t.Fatal("the attempted URL was not stored as evidence")
				}
				if evidence != link.Defang(tier.url) {
					t.Errorf("evidence = %q, want the defanged attempt %q",
						evidence, link.Defang(tier.url))
				}
			})
		}
	}
}

// A trailing dot is the same host, and a refusal it provokes is recorded like
// any other (F26).
//
// The unit tests hold the tiers. What only a database shows is the consequence
// that made this worth reopening M30 for: the dotted attempt used to be accepted
// outright, so nothing was refused, and therefore **nothing was written** — the
// audit log said the instance had never been asked for the metadata endpoint,
// while a link pointing at it sat in the table waiting to be followed. The row
// is the evidence an operator would use to notice they are being probed, and its
// absence is what made the bypass silent as well as effective.
func TestATrailingDotIsRefusedAndRecorded(t *testing.T) {
	dotted := map[string]string{
		"http://169.254.169.254./latest/meta-data/":  "unappealable.private_address",
		"http://localhost./":                         "unappealable.private_address",
		"https://metadata.google.internal./compute/": "high_confidence.embedded_host",
	}

	for raw, wantCode := range dotted {
		for _, surface := range []string{"link.create", "link.update"} {
			t.Run(surface+"/"+raw, func(t *testing.T) {
				f := newBlocking(t)

				var err error
				if surface == "link.create" {
					_, err = f.links.Create(f.ctx, f.owner, link.CreateInput{URL: raw})
				} else {
					created, cerr := f.links.Create(f.ctx, f.owner,
						link.CreateInput{URL: "https://good.example/"})
					if cerr != nil {
						t.Fatalf("create a link to edit: %v", cerr)
					}
					_, err = f.links.Update(f.ctx, f.owner, created.ID, link.UpdateInput{URL: &raw})
				}
				if got := codeOf(t, err); got != wantCode {
					t.Fatalf("refusal code = %q, want %q", got, wantCode)
				}

				events := f.blockEvents()
				if len(events) != 1 {
					t.Fatalf("%d destination.blocked records, want exactly 1: a refusal "+
						"nobody records is a probe nobody can see", len(events))
				}
				e := events[0]
				if e.Meta["code"] != wantCode {
					t.Errorf("audit code = %v, want %q", e.Meta["code"], wantCode)
				}
				if e.Meta["surface"] != surface {
					t.Errorf("audit surface = %v, want %q", e.Meta["surface"], surface)
				}
				// The dot is in the evidence, because the evidence is what was
				// typed and not what the validator made of it.
				if got := e.Meta["url_defanged"]; got != link.Defang(raw) {
					t.Errorf("evidence = %v, want the defanged attempt %q", got, link.Defang(raw))
				}
			})
		}
	}

	// The accepted case, end to end, which is the other half of "canonicalized,
	// not rejected". A dotted host that nothing objects to is stored without its
	// dot — so the value the redirect hands a visitor verbatim is the same value
	// every tier judged, rather than a spelling they never saw.
	f := newBlocking(t)
	created, err := f.links.Create(f.ctx, f.owner, link.CreateInput{URL: "https://example.com./path"})
	if err != nil {
		t.Fatalf("refused https://example.com./path: %v. A trailing dot is a fully "+
			"qualified name, and this fix canonicalizes it rather than refusing it", err)
	}
	if created.URL != "https://example.com/path" {
		t.Errorf("stored %q, want %q", created.URL, "https://example.com/path")
	}
	if events := f.blockEvents(); len(events) != 0 {
		t.Errorf("%d blocked records for an accepted destination", len(events))
	}
}

// A destination nothing objects to is still accepted, and writes no record. The
// counterpart matters as much as the refusals: a check that refused everything
// would pass every test above.
func TestAnOrdinaryDestinationIsAcceptedAndNotRecorded(t *testing.T) {
	f := newBlocking(t)
	f.blockHost("blocked.example", "review")

	for _, url := range []string{
		"https://example.com/campaign",
		"https://notblocked.example/x",  // ends the same way, different label
		"https://blocked.example.org/x", // the blocked name is a prefix, not a suffix
		"https://xn--mller-kva.de/",     // müller.de, an ordinary name
		"https://93.184.216.34/",        // a public address literal
	} {
		if _, err := f.links.Create(f.ctx, f.owner, link.CreateInput{URL: url}); err != nil {
			t.Errorf("refused an ordinary destination %q: %v", url, err)
		}
	}
	// A child of a blocked host is refused, which is the label-boundary rule the
	// environment list has always had.
	if _, err := f.links.Create(f.ctx, f.owner,
		link.CreateInput{URL: "https://login.blocked.example/"}); err == nil {
		t.Error("accepted a child of a blocked host")
	}

	events := f.blockEvents()
	if len(events) != 1 {
		t.Errorf("%d blocked records, want 1: an accepted destination must not "+
			"write one, or the log fills with links that were fine", len(events))
	}
}

// LINKCTRL_DESTINATION_BLOCKLIST seeds the runtime list and keeps feeding it.
//
// Three properties, and the third is the one worth the test: the environment is
// reconciled rather than merely inserted, so an entry the operator removed is
// retired — and the reconciliation never touches what the owner decided in the
// review queue, because a restart that silently undid a moderation decision is
// the one failure this path must not have.
func TestTheEnvironmentBlocklistSeedsTheRuntimeList(t *testing.T) {
	f := newBlocking(t)

	if err := f.links.SeedBlocklist(f.ctx, []string{"a.example", " B.EXAMPLE ", ""}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows := f.rowsExcludingSeed()
	if rows["a.example"] != "env" || rows["b.example"] != "env" {
		t.Fatalf("rows after seeding = %v, want a.example and b.example from env "+
			"(case folded, whitespace trimmed, blanks skipped)", rows)
	}
	if len(rows) != 2 {
		t.Fatalf("rows after seeding = %v, want exactly 2", rows)
	}

	// The env list keeps working: an entry in it refuses, with a code that says
	// which tier it came from.
	_, err := f.links.Create(f.ctx, f.owner, link.CreateInput{URL: "https://a.example/x"})
	if got := codeOf(t, err); got != "low_confidence.operator_blocklist" {
		t.Errorf("refusal code = %q; the operator list is the low-confidence tier "+
			"and its refusals are appealable", got)
	}

	// Something the owner added, which a restart must leave alone.
	f.blockHost("owner.example", "review")

	if err := f.links.SeedBlocklist(f.ctx, []string{"a.example"}); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	rows = f.blockedRows()
	if _, still := rows["b.example"]; still {
		t.Error("b.example survived being removed from the environment; the " +
			"variable would be a one-way ratchet")
	}
	if rows["a.example"] != "env" {
		t.Error("a.example did not survive a reseed that still names it")
	}
	if rows["owner.example"] != "review" {
		t.Errorf("owner.example = %q after a reseed; a restart must never reverse "+
			"a decision the review queue made", rows["owner.example"])
	}
	// And the same protection covers the seeded shorteners, which is the reason
	// they have a source of their own instead of arriving as 'env' rows: the
	// environment names none of them, so an env-scoped retirement that could see
	// them would delete every one of them on the first boot.
	if rows["bit.ly"] != link.SourceShortener {
		t.Errorf("bit.ly = %q after a reseed, want %q: reconciliation is scoped to "+
			"one source so that it can only retire what it wrote",
			rows["bit.ly"], link.SourceShortener)
	}
}

// The shortener list is data, not a compiled file (D39).
//
// The unit test holds the structural half — nothing in the package embeds those
// hosts any more. What only a database can show is the half that made the
// decision worth taking: the same binary that refuses a short link accepts it
// once the row is gone, with no rebuild and no restart in between, because a
// match on this list was never an authoritative claim.
func TestTheSeededShortenerListIsRuntimeData(t *testing.T) {
	f := newBlocking(t)

	if got := f.blockedRows()["bit.ly"]; got != link.SourceShortener {
		t.Fatalf("bit.ly = %q, want a %q row seeded by migration 01500",
			got, link.SourceShortener)
	}

	// It refuses at the tier the owner can overrule, and reports what the row
	// actually claims rather than the generic operator-list rule — an operator
	// reading `low_confidence.operator_blocklist` would go looking for the
	// person who added it, and there is no such person.
	_, err := f.links.Create(f.ctx, f.owner, link.CreateInput{URL: "https://bit.ly/abc"})
	if got := codeOf(t, err); got != "low_confidence.shortener_chain" {
		t.Errorf("refusal code = %q, want low_confidence.shortener_chain", got)
	}
	// Case folding is the caller's, and a child of a listed shortener is covered
	// by the same label-boundary rule as every other row in this table.
	if _, err := f.links.Create(f.ctx, f.owner,
		link.CreateInput{URL: "https://links.TINYURL.com/x"}); err == nil {
		t.Error("accepted a child of a seeded shortener host")
	}

	// Overruling costs a row, not a release.
	if _, err := f.pool.Exec(f.ctx,
		`DELETE FROM blocked_destinations WHERE host = 'bit.ly'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.links.Create(f.ctx, f.owner,
		link.CreateInput{URL: "https://bit.ly/abc"}); err != nil {
		t.Errorf("still refused after the row was deleted: %v. A list that needs a "+
			"rebuild to overrule is the high-confidence tier, and this is not it.", err)
	}

	// And it is permanent. Running the boot path again — the only thing that
	// writes to this table without a person asking — does not bring the row
	// back, because the seed is a migration that has already run and not a file
	// re-read at every start. Seeding from an embedded file the way the
	// environment list is seeded would have made this overrule last exactly
	// until the next restart.
	if err := f.links.SeedBlocklist(f.ctx, nil); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if _, back := f.blockedRows()["bit.ly"]; back {
		t.Error("bit.ly came back; an overrule that survives only until the next " +
			"restart is the release cycle D39 removed, wearing a different hat")
	}
}

// The milestone's central claim, at the only place it can be checked end to end:
// no configuration, no list entry and no review path accepts a metadata or
// private address.
//
// The review path does not exist yet — M31 builds it — so what is asserted is
// the property that makes it safe when it arrives. The review queue's only verb
// on this table is DELETE, and the state it can reach is "the list is empty";
// that state is set up here explicitly, and the addresses are still refused,
// with a code that says the refusal cannot be appealed.
func TestNothingAcceptsAMetadataOrPrivateAddress(t *testing.T) {
	f := newBlocking(t)

	// Every row gone, as if the owner had approved everything anybody ever
	// disputed.
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM blocked_destinations`); err != nil {
		t.Fatal(err)
	}

	addresses := []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://0xa9fea9fe/",
		"http://10.0.0.1/admin",
		"http://127.0.0.1:8080/",
		"http://[::1]/",
		"http://localhost/",
	}
	for _, raw := range addresses {
		t.Run(raw, func(t *testing.T) {
			_, err := f.links.Create(f.ctx, f.owner, link.CreateInput{URL: raw})
			if got := codeOf(t, err); got != "unappealable.private_address" {
				t.Errorf("code = %q, want unappealable.private_address", got)
			}
		})
	}

	// And a list entry naming one changes nothing about which tier refuses it.
	// A reader who saw high_confidence.embedded_host here would reasonably
	// conclude that deleting the row makes the address acceptable.
	f.blockHost("169.254.169.254", "review")
	_, err := f.links.Create(f.ctx, f.owner, link.CreateInput{URL: "http://169.254.169.254/"})
	if got := codeOf(t, err); got != "unappealable.private_address" {
		t.Errorf("code = %q; adding an address to the list must not move which "+
			"tier refuses it", got)
	}
}

// The evidence is hostile input, and the audit read API hands it to whoever
// asks. Two plants, per the milestone: a javascript: URL and one carrying HTML.
// Both must come back inert — escaped, so nothing renders as markup, and
// defanged, so nothing is a followable link.
func TestBlockedAttemptEvidenceRendersInert(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Blocked first, so the two HTML-bearing plants are refused by a tier that
	// stores evidence rather than for being unparseable. The javascript: one
	// needs no help: it is refused by the unappealable tier on its scheme, and
	// its evidence is the one an operator is most likely to click.
	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO blocked_destinations (host, source, reason)
		 VALUES ('evil.example', 'review', 'test')`); err != nil {
		t.Fatal(err)
	}

	plants := []string{
		"javascript:alert(document.cookie)",
		"https://evil.example/<script>alert(1)</script>",
		`https://evil.example/?next="><img src=x onerror=alert(1)>`,
	}
	for _, raw := range plants {
		resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{"url": raw})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("POST %q = %d, want 422", raw, resp.StatusCode)
		}
		f.decode(resp, nil)
	}

	// The raw bytes a client receives, not a decoded structure: what matters is
	// what a browser or a console would render.
	resp := f.do(http.MethodGet, "/api/v1/audit?limit=200", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/audit = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)

	if !strings.Contains(page, "destination.blocked") {
		t.Fatal("no blocked-attempt records came back; the rest of this test would " +
			"pass by having nothing to find")
	}

	for _, live := range []string{
		"javascript:", // a scheme something would act on
		"://",         // anything auto-linkers turn into a link
		"<script",     // markup, unescaped
		"<img",
		`"><`,
	} {
		if strings.Contains(page, live) {
			t.Errorf("the audit response contains %q verbatim; the attempted URL "+
				"is hostile input and must be escaped and defanged everywhere it "+
				"is displayed", live)
		}
	}

	// And the evidence is still there to read, or defanging would just be
	// deletion with extra steps.
	for _, want := range []string{"javascript[:]alert", "evil[.]example"} {
		if !strings.Contains(page, want) {
			t.Errorf("the audit response does not contain %q; the attempted URL is "+
				"stored as evidence and must stay legible", want)
		}
	}
}

// Blocking runs on the management path and nowhere else.
//
// Re-checking links that were already accepted is a separate job and a separate
// decision (Plan.md), so a link created before its host was listed keeps
// redirecting. That is not an oversight to fix here: making the redirect tree
// consult a blocklist would put a database read on the hot path, which is the
// one thing every milestone in this phase inherits a rule against.
func TestBlockingDoesNotReachTheRedirectPath(t *testing.T) {
	f := newRedirect(t)
	f.setupOwner()

	alias := f.createLink(map[string]any{"url": "https://later-blocked.example/target"})

	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO blocked_destinations (host, source, reason)
		 VALUES ('later-blocked.example', 'review', 'test')`); err != nil {
		t.Fatal(err)
	}

	resp := f.get("/" + alias)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /%s = %d, want 302: an already-accepted link is not "+
			"re-checked, and the redirect tree does not read the blocklist",
			alias, resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://later-blocked.example/target" {
		t.Errorf("Location = %q", got)
	}

	// Creating the same destination now is refused, which is what shows the
	// difference is the path and not the data.
	create := f.do(http.MethodPost, "/api/v1/links",
		map[string]any{"url": "https://later-blocked.example/other"})
	defer func() { _ = create.Body.Close() }()
	if create.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("creating a blocked destination = %d, want 422", create.StatusCode)
	}
}

// A refusal whose audit record cannot be written is still a refusal. The
// alternative — failing the whole request — would turn a logging outage into a
// destination nobody can be refused for, which is exactly backwards.
func TestARefusedAuditWriteDoesNotAcceptTheDestination(t *testing.T) {
	pool := newDB(t)
	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	owner, err := authSvc.Register(context.Background(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	links := link.NewService(pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://lnk.test",
		Audit: failingRecorder{},
	})

	_, err = links.Create(t.Context(), owner, link.CreateInput{URL: "http://169.254.169.254/"})
	if got := codeOf(t, err); got != "unappealable.private_address" {
		t.Errorf("code = %q; a failed audit write must not change the verdict", got)
	}
}
