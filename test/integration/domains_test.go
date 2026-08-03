//go:build integration

package integration

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// M39. Per-domain ownership — who owns a hostname, who may change it, and the
// fact that owning one buys nothing on the wire until M40 verifies it.
//
// Two claims are load-bearing here and they pull in opposite directions:
//
//   - `domains.write` is now an **ownership** check. It was minted for one
//     instance-wide setting with nothing to own; a workspace admin now manages
//     their own hostnames and gets a 403 on anybody else's.
//   - A registered hostname is **not a routing target**. Nothing is served on
//     it, and the host router refuses it exactly as it refuses a name nobody
//     registered — which hosts_test.go asserts, unchanged.

// newDomainFixture is the team fixture with an audit recorder on its link
// service.
//
// The recorder is the point rather than a detail: domain create, rename and
// delete are audit events, and a fixture without one would let the tests that
// read those records pass by never producing any.
func newDomainFixture(t *testing.T) *teamFixture {
	t.Helper()
	f := newTeamFixture(t)
	f.links = link.NewService(f.pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://links.test",
		Audit: audit.NewService(f.pool),
	})
	return f
}

// registeredIn registers a hostname as the given actor and returns it.
func (f *teamFixture) registeredIn(t *testing.T, actor *auth.Identity, hostname string) *link.Domain {
	t.Helper()
	d, err := f.links.RegisterDomain(t.Context(), actor, hostname)
	if err != nil {
		t.Fatalf("register %s: %v", hostname, err)
	}
	return d
}

// **The test M39 exists for.** Two workspaces, an admin in each, and neither
// one's `domains.write` reaches the other's hostname.
//
// Both admins are organization-wide, which is the arrangement that makes this
// worth asserting: they hold exactly the same permissions, so nothing about the
// permission set distinguishes them. What distinguishes them is which workspace
// the domain belongs to, and that is the check this milestone added.
func TestWorkspaceAdminManagesOnlyItsOwnDomains(t *testing.T) {
	f := newDomainFixture(t)

	marketing, err := f.team.CreateWorkspace(t.Context(), f.owner, "Marketing")
	if err != nil {
		t.Fatalf("create Marketing: %v", err)
	}
	support, err := f.team.CreateWorkspace(t.Context(), f.owner, "Support")
	if err != nil {
		t.Fatalf("create Support: %v", err)
	}

	f.member(t, "alice@example.com", "admin")
	f.member(t, "bob@example.com", "admin")
	alice := f.identityIn(t, "alice@example.com", marketing.ID)
	bob := f.identityIn(t, "bob@example.com", support.ID)
	for _, who := range []*auth.Identity{alice, bob} {
		if !who.Can(link.PermDomainsWrite) {
			t.Fatalf("%s does not hold %s; this test would prove nothing",
				who.Email, link.PermDomainsWrite)
		}
	}

	mine := f.registeredIn(t, alice, "go.marketing.example")
	if mine.Scope != link.ScopeWorkspace {
		t.Errorf("a registered hostname has scope %q, want %q", mine.Scope, link.ScopeWorkspace)
	}

	// Bob holds domains.write and it reaches nothing of Alice's. Forbidden
	// rather than not-found: he is being told the hostname is not his, which is
	// the true answer and the one that does not invite guessing at ids.
	if _, err := f.links.RenameDomain(t.Context(), bob, mine.ID, "stolen.example"); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("another workspace's admin renamed the domain: %v, want forbidden", err)
	}
	if err := f.links.DeleteDomain(t.Context(), bob, mine.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("another workspace's admin deleted the domain: %v, want forbidden", err)
	}
	if got := f.count(t,
		`SELECT count(*) FROM domains WHERE id = $1 AND hostname = $2 AND deleted_at IS NULL`,
		mine.ID, "go.marketing.example"); got != 1 {
		t.Fatal("a refused write changed the domain anyway")
	}

	// And it is absent from his list, not merely unmanageable in it. The
	// registration is the only thing there is to disclose at this milestone.
	for _, d := range f.domainsFor(t, bob) {
		if d.ID == mine.ID {
			t.Errorf("another workspace's hostname %q is listed for %s", d.Hostname, bob.Email)
		}
	}

	// The owning workspace's admin can do both.
	renamed, err := f.links.RenameDomain(t.Context(), alice, mine.ID, "links.marketing.example")
	if err != nil {
		t.Fatalf("the owning workspace's admin could not rename: %v", err)
	}
	if renamed.Hostname != "links.marketing.example" {
		t.Errorf("hostname = %q after rename", renamed.Hostname)
	}
	if err := f.links.DeleteDomain(t.Context(), alice, mine.ID); err != nil {
		t.Fatalf("the owning workspace's admin could not delete: %v", err)
	}
}

// domainsFor lists what an actor may see.
func (f *teamFixture) domainsFor(t *testing.T, actor *auth.Identity) []link.Domain {
	t.Helper()
	out, err := f.links.Domains(t.Context(), actor)
	if err != nil {
		t.Fatalf("list domains for %s: %v", actor.Email, err)
	}
	return out
}

// A registration is stored unverified, and the instance default is left alone.
func TestRegisteredHostnameIsStoredUnverified(t *testing.T) {
	f := newDomainFixture(t)

	d := f.registeredIn(t, f.owner, "go.example.test")
	if d.Verified || d.VerifiedAt != nil {
		t.Errorf("a freshly registered hostname reports verified=%v at %v; nothing has "+
			"proved control of the name", d.Verified, d.VerifiedAt)
	}
	if d.IsDefault {
		t.Error("a registered hostname claimed to be the instance default")
	}
	var (
		verifiedAt *string
		ssl        string
		wsID       *uuid.UUID
		orgID      *uuid.UUID
	)
	if err := f.pool.QueryRow(t.Context(),
		`SELECT verified_at::text, ssl_status, workspace_id, organization_id
		   FROM domains WHERE id = $1`, d.ID).
		Scan(&verifiedAt, &ssl, &wsID, &orgID); err != nil {
		t.Fatalf("read the registered row: %v", err)
	}
	if verifiedAt != nil {
		t.Errorf("verified_at = %v in the database, want NULL", *verifiedAt)
	}
	if ssl != "none" {
		t.Errorf("ssl_status = %q, want none", ssl)
	}
	// D68's third legal state: both columns set, the workspace implying its
	// organization. A row with a workspace and no organization is the one
	// combination the CHECK forbids, and the service must not be the thing that
	// tries to write it.
	if wsID == nil || *wsID != f.owner.WorkspaceID {
		t.Errorf("workspace_id = %v, want %v", wsID, f.owner.WorkspaceID)
	}
	if orgID == nil || *orgID != f.owner.OrgID {
		t.Errorf("organization_id = %v, want %v", orgID, f.owner.OrgID)
	}
}

// The three legal ownership states are the only ones the table accepts.
//
// Written against the constraint rather than through the service, because the
// service is what the service tests cover: this asserts that a future writer
// which forgets the pair is stopped by the schema and not by a code path it
// might not go through.
func TestDomainOwnershipStatesAreConstrained(t *testing.T) {
	f := newDomainFixture(t)

	_, err := f.pool.Exec(t.Context(),
		`INSERT INTO domains (id, organization_id, workspace_id, hostname)
		 VALUES ($1, NULL, $2, 'orphan.example')`,
		uuid.Must(uuid.NewV7()), f.owner.WorkspaceID)
	if err == nil {
		t.Fatal("a workspace-owned domain with no organization was accepted; the " +
			"CHECK that enumerates D68's three states is not doing anything")
	}
	// The three that must be accepted, one row each.
	for _, tc := range []struct {
		name string
		org  *uuid.UUID
		ws   *uuid.UUID
		host string
	}{
		{"instance default", nil, nil, "instance.example"},
		{"organization-owned", &f.owner.OrgID, nil, "org.example"},
		{"workspace-owned", &f.owner.OrgID, &f.owner.WorkspaceID, "ws.example"},
	} {
		if _, err := f.pool.Exec(t.Context(),
			`INSERT INTO domains (id, organization_id, workspace_id, hostname)
			 VALUES ($1, $2, $3, $4)`,
			uuid.Must(uuid.NewV7()), tc.org, tc.ws, tc.host); err != nil {
			t.Errorf("the %s state was refused: %v", tc.name, err)
		}
	}
}

// The instance default is not administered through this surface, whoever asks.
//
// It is the hostname every workspace's links are on, its name is a placeholder
// the resolver never reads — matching on `is_default` instead — and its settings
// are the `/domain` endpoint's. Renaming it here would change a name nothing
// consults; deleting it would take the hostname out from under every link on the
// instance.
func TestInstanceDefaultDomainIsNotRegisteredHere(t *testing.T) {
	f := newDomainFixture(t)
	def := f.defaultDomain(t)

	for _, tc := range []struct {
		what string
		err  error
	}{
		{"rename", func() error {
			_, err := f.links.RenameDomain(t.Context(), f.owner, def, "renamed.example")
			return err
		}()},
		{"delete", f.links.DeleteDomain(t.Context(), f.owner, def)},
	} {
		var ve domain.ValidationErrors
		if !errors.As(tc.err, &ve) || ve[0].Code != "instance_default" {
			t.Errorf("%s of the instance default answered %v, want instance_default", tc.what, tc.err)
		}
	}
	if got := f.count(t,
		`SELECT count(*) FROM domains WHERE id = $1 AND is_default AND deleted_at IS NULL`,
		def); got != 1 {
		t.Fatal("the instance default was changed by a refused write")
	}

	// It is still listed, because every link is on it and a page that hid it
	// would be a page that never mentions the hostname links are served on.
	// Listed and not manageable.
	var seen bool
	for _, d := range f.domainsFor(t, f.owner) {
		if d.ID != def {
			continue
		}
		seen = true
		if d.Scope != link.ScopeInstance {
			t.Errorf("the default domain has scope %q, want %q", d.Scope, link.ScopeInstance)
		}
		if d.Manageable {
			t.Error("the default domain is offered as manageable from the domains page")
		}
	}
	if !seen {
		t.Error("the instance default is missing from the list")
	}
}

// An editor holds no domains.write, so registration is refused before ownership
// is ever consulted.
func TestRegisteringADomainRequiresDomainsWrite(t *testing.T) {
	f := newDomainFixture(t)
	editor := f.member(t, "editor@example.com", "editor")
	if editor.Can(link.PermDomainsWrite) {
		t.Fatalf("an invited editor holds %s; this test would prove nothing", link.PermDomainsWrite)
	}

	if _, err := f.links.RegisterDomain(t.Context(), editor, "editor.example"); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("an editor registered a hostname: %v, want forbidden", err)
	}
	// Reading is links.read, so the list is not refused — the editor sees the
	// hostname their links are served on and is offered nothing to do to it.
	for _, d := range f.domainsFor(t, editor) {
		if d.Manageable {
			t.Errorf("%q is offered to an editor as manageable", d.Hostname)
		}
	}
}

// One hostname, one workspace. The instance-wide uniqueness is what makes the
// alias namespace unambiguous, which is the argument D68 turns on.
func TestAHostnameBelongsToExactlyOneWorkspace(t *testing.T) {
	f := newDomainFixture(t)

	other, err := f.team.CreateWorkspace(t.Context(), f.owner, "Other")
	if err != nil {
		t.Fatalf("create Other: %v", err)
	}
	f.member(t, "carol@example.com", "admin")
	carol := f.identityIn(t, "carol@example.com", other.ID)

	f.registeredIn(t, f.owner, "shared.example")

	_, err = f.links.RegisterDomain(t.Context(), carol, "shared.example")
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || ve[0].Code != "conflict" {
		t.Fatalf("a second workspace registered the same hostname: %v", err)
	}
	// The message must not say whose it is. Registration is open to every
	// workspace on the instance, so a message naming the owner would turn this
	// into a way of asking what a neighbour has registered.
	if !strings.Contains(ve[0].Message, "shared.example") {
		t.Errorf("the conflict message does not name the hostname: %q", ve[0].Message)
	}
	if strings.Contains(ve[0].Message, f.owner.Email) {
		t.Errorf("the conflict message discloses the owner: %q", ve[0].Message)
	}

	// Case is not a difference. `domains_hostname_key` is on lower(hostname).
	if _, err := f.links.RegisterDomain(t.Context(), carol, "SHARED.example"); !errors.As(err, &ve) {
		t.Errorf("a case variant of a registered hostname was accepted: %v", err)
	}

	// Removing it frees the name, because the unique index is partial on
	// deleted_at.
	var id uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM domains WHERE hostname = 'shared.example' AND deleted_at IS NULL`).
		Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := f.links.DeleteDomain(t.Context(), f.owner, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := f.links.RegisterDomain(t.Context(), carol, "shared.example"); err != nil {
		t.Errorf("a removed hostname could not be registered again: %v", err)
	}
}

// Create, rename and delete each write a record (M21).
//
// These are the phase's unowned administrative changes: every other one happens
// inside an organization and is answerable by asking somebody in it, while a
// hostname is a public name and "who put this on the instance" had no answer at
// all before this milestone.
func TestDomainLifecycleIsAudited(t *testing.T) {
	f := newDomainFixture(t)

	d := f.registeredIn(t, f.owner, "audited.example")
	if _, err := f.links.RenameDomain(t.Context(), f.owner, d.ID, "renamed.example"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := f.links.DeleteDomain(t.Context(), f.owner, d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	rows, err := f.pool.Query(t.Context(),
		`SELECT action, target_type, target_id, metadata::text
		   FROM audit_logs
		  WHERE organization_id = $1 AND action LIKE 'domain.%'
		  ORDER BY occurred_at, action`, f.owner.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var action, targetType, metadata string
		var targetID *uuid.UUID
		if err := rows.Scan(&action, &targetType, &targetID, &metadata); err != nil {
			t.Fatal(err)
		}
		if targetType != "domain" {
			t.Errorf("%s has target_type %q, want domain", action, targetType)
		}
		if targetID == nil || *targetID != d.ID {
			t.Errorf("%s names target %v, want %v", action, targetID, d.ID)
		}
		got = append(got, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// Chronological, which is also the order they happened in. The three are
	// asserted as a sequence rather than as a set because "created then
	// renamed then deleted" is the story an operator reads off the log, and a
	// set assertion would pass on a log that recorded them in any order.
	want := []string{
		audit.ActionDomainCreated, audit.ActionDomainRenamed, audit.ActionDomainDeleted,
	}
	if len(got) != len(want) {
		t.Fatalf("domain audit actions = %v, want the three of %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("audit action %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Hostname syntax, at the edges people actually reach.
func TestHostnameSyntaxIsRefusedBeforeAnythingIsStored(t *testing.T) {
	f := newDomainFixture(t)

	for _, tc := range []struct{ name, hostname, code string }{
		{"a whole URL", "https://go.example.com/path", "not_a_hostname"},
		{"an IP address", "203.0.113.7", "not_a_hostname"},
		{"a single label", "localhost", "not_a_hostname"},
		{"a numeric TLD", "example.123", "malformed"},
		{"a leading hyphen", "-go.example.com", "malformed"},
		{"empty", "   ", "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.links.RegisterDomain(t.Context(), f.owner, tc.hostname)
			var ve domain.ValidationErrors
			if !errors.As(err, &ve) {
				t.Fatalf("registering %q answered %v, want a validation error", tc.hostname, err)
			}
			if ve[0].Code != tc.code {
				t.Errorf("code = %q, want %q (%s)", ve[0].Code, tc.code, ve[0].Message)
			}
		})
	}

	// Normalization, not refusal: the fully-qualified form and a shouted one are
	// the same name, and storing either verbatim would make the unique index
	// treat them as two.
	d := f.registeredIn(t, f.owner, "  GO.Example.COM.  ")
	if d.Hostname != "go.example.com" {
		t.Errorf("stored hostname = %q, want go.example.com", d.Hostname)
	}
	if n := f.count(t, `SELECT count(*) FROM domains WHERE hostname = 'go.example.com'`); n != 1 {
		t.Errorf("the normalized hostname is not what was stored")
	}
}

// **A registered hostname is not a routing target.**
//
// The split fixture is the whole product on two hostnames, exactly as a reverse
// proxy presents it. A third name is registered here and stays a name the router
// does not know: no alias resolves on it, no dashboard answers on it, and the
// operational endpoints answer as they do for any unrecognized host.
//
// This does not replace TestUnknownHostServesNoLinksAndNoDashboard, which
// asserts the same refusal for a host nobody registered. Both must hold, and the
// point of M39 is that registering makes no difference to either.
func TestRegisteredHostnameServesNothingUntilItIsVerified(t *testing.T) {
	f := newSplit(t)
	const custom = "go.custom.test"

	f.createLinkForRouting("registered-route", "https://example.com/target")
	d, err := f.links.RegisterDomain(f.t.Context(), f.owner, custom)
	if err != nil {
		t.Fatalf("register %s: %v", custom, err)
	}
	if d.Verified {
		t.Fatal("the registration verified itself; the rest of this test proves nothing")
	}

	for _, tc := range []struct{ name, path string }{
		{"an alias that exists on the link host", "/registered-route"},
		{"an alias that exists nowhere", "/nosuchalias"},
		{"the dashboard", "/dashboard"},
		{"the API", "/api/v1/openapi.json"},
		{"the root", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.get(custom, tc.path)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s%s = %d, want 404; a registered hostname is not a "+
					"routing target until it is verified", custom, tc.path, resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); loc != "" {
				t.Errorf("the registered host redirected to %q", loc)
			}
		})
	}

	// The operational endpoints answer on every host, registered or not — a
	// probe does not know the operator's hostnames.
	if resp := f.get(custom, "/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s/healthz = %d, want 200", custom, resp.StatusCode)
	}
}

// The dashboard half, with scripting switched off.
//
// Every write is an ordinary form POST answered with a 303, so the page works
// without htmx and a reload never offers to resubmit. And the page has to say
// the thing the milestone is careful about: a registered hostname serves
// nothing. A domains page that looked like a working custom domain would be the
// product promising what M40 has not built.
func TestTheDomainsPageRegistersAHostnameWithoutJavaScript(t *testing.T) {
	f := newRules(t)
	f.claim()

	page := f.getHTML("/domains")
	if !strings.Contains(page, `action="/domains"`) {
		t.Fatalf("the domains page offers no register form:\n%s", page)
	}
	if !strings.Contains(page, "Nothing is served on it yet") {
		t.Error("the page does not say that a registered hostname serves nothing")
	}

	f.postDomainForm(t, "/domains", url.Values{"hostname": {"go.dashboard.example"}})
	page = f.getHTML("/domains")
	if !strings.Contains(page, "go.dashboard.example") {
		t.Fatalf("the registered hostname is not on the page:\n%s", page)
	}
	if !strings.Contains(page, "Not verified") {
		t.Error("the row does not say the hostname is unverified")
	}

	domains, err := f.links.Domains(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, d := range domains {
		if d.Hostname == "go.dashboard.example" {
			id = d.ID.String()
		}
	}
	if id == "" {
		t.Fatal("the form did not register a hostname")
	}

	f.postDomainForm(t, "/domains/"+id, url.Values{"hostname": {"links.dashboard.example"}})
	if page = f.getHTML("/domains"); !strings.Contains(page, "links.dashboard.example") {
		t.Error("the rename form did not change the hostname")
	}
	f.postDomainForm(t, "/domains/"+id+"/delete", url.Values{})
	if page = f.getHTML("/domains"); strings.Contains(page, "links.dashboard.example") {
		t.Errorf("the removed hostname is still on the page:\n%s", page)
	}
}

func (f *ruleFixture) postDomainForm(t *testing.T, path string, vals url.Values) {
	t.Helper()
	resp := f.postForm(path, vals)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body := make([]byte, 2048)
		n, _ := resp.Body.Read(body)
		t.Fatalf("POST %s = %d, want 303 (a form post has to redirect, or a "+
			"reload offers to resubmit it)\n%s", path, resp.StatusCode, body[:n])
	}
}
