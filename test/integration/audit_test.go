//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// auditFixture is the audit service against a real database, plus a registered
// owner to act as. The read API gets its own fixture below; this one is for the
// writer, where the assertions are about what reached the column.
type auditFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	svc   *audit.Service
	owner *auth.Identity
}

func newAudit(t *testing.T) *auditFixture {
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

	return &auditFixture{t: t, pool: pool, svc: audit.NewService(pool), owner: owner}
}

// auditRow is one record read straight out of the table, bypassing the API, so
// the privacy assertions are about storage rather than about serialization.
type auditRow struct {
	Action     string
	ActorLabel string
	ActorID    *uuid.UUID
	OrgID      *uuid.UUID
	IPPrefix   *string
	Metadata   []byte
}

func (f *auditFixture) rows() []auditRow {
	f.t.Helper()
	rs, err := f.pool.Query(f.t.Context(), `
		SELECT action, actor_label, actor_user_id, organization_id, ip_prefix, metadata
		  FROM audit_logs ORDER BY occurred_at, id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rs.Close()

	var out []auditRow
	for rs.Next() {
		var r auditRow
		if err := rs.Scan(&r.Action, &r.ActorLabel, &r.ActorID, &r.OrgID, &r.IPPrefix, &r.Metadata); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// The privacy line, asserted where it actually matters: what is in the column.
//
// The writer is handed a full address through the context, exactly as a request
// would, and the record that lands must carry a network and nothing narrower.
// This is the test m21.md names — an address reaching this table is the failure
// the whole no-IP-column stance exists to prevent, and it would be invisible
// without an assertion on the stored value.
func TestAuditEventsRecordAPrefixAndNeverAnAddress(t *testing.T) {
	f := newAudit(t)

	const addr = "203.0.113.42"
	ctx := auth.WithClientIP(t.Context(), netip.MustParseAddr(addr))
	if err := f.svc.Record(ctx, f.owner, audit.Event{
		Action:   audit.ActionDomainRootRedirectChanged,
		Metadata: map[string]any{"from": "", "to": "https://example.com/home"},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows := f.rows()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(rows))
	}
	got := rows[0]

	if got.IPPrefix == nil {
		t.Fatal("ip_prefix is null; the address in the context was dropped entirely")
	}
	if *got.IPPrefix != "203.0.113.0/24" {
		t.Errorf("ip_prefix = %q, want 203.0.113.0/24", *got.IPPrefix)
	}
	// The whole point, stated as its own assertion: the host octet must be gone.
	// A /32, or the raw address, would both satisfy "there is something here".
	if strings.Contains(*got.IPPrefix, addr) {
		t.Errorf("ip_prefix %q contains the full address; only a network may be stored", *got.IPPrefix)
	}

	// And nowhere else in the row either — metadata is free-form jsonb and is
	// the obvious place for an address to reappear by accident later.
	if strings.Contains(string(got.Metadata), addr) {
		t.Errorf("metadata carries the client address: %s", got.Metadata)
	}
	if strings.Contains(got.ActorLabel, addr) {
		t.Errorf("actor_label carries the client address: %q", got.ActorLabel)
	}
}

// actor_label is a snapshot, and the reason it exists is that the account it
// names can be deleted. A label read through a join would vanish with the user;
// this one must survive.
func TestAuditActorLabelSurvivesTheUser(t *testing.T) {
	f := newAudit(t)

	if err := f.svc.Record(t.Context(), f.owner, audit.Event{
		Action: audit.ActionDomainRootRedirectChanged,
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.rows()[0].ActorLabel; got != "owner@example.com" {
		t.Fatalf("actor_label = %q, want the actor's email snapshotted at write time", got)
	}

	// Delete the user the record names. audit_logs.actor_user_id deliberately
	// has no foreign key, so this must not cascade and must not fail.
	if _, err := f.pool.Exec(t.Context(), `DELETE FROM users WHERE id = $1`, f.owner.UserID); err != nil {
		t.Fatalf("delete the acting user: %v", err)
	}

	rows := f.rows()
	if len(rows) != 1 {
		t.Fatalf("%d records survive the user's deletion, want 1: an audit record "+
			"must outlive the account it names", len(rows))
	}
	if rows[0].ActorLabel != "owner@example.com" {
		t.Errorf("actor_label = %q after the user was deleted, want the snapshot intact",
			rows[0].ActorLabel)
	}
}

// M20 promised that changing the root redirect becomes an audit event once the
// audit log had behavior. This is that promise, discharged end to end through
// the service the dashboard and the API both call.
func TestChangingTheRootRedirectIsAudited(t *testing.T) {
	f := newAudit(t)

	rootCache := &countingRootInvalidator{}
	links := link.NewService(f.pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://lnk.test",
		SplitHosts: true, RootCache: rootCache,
		Audit: f.svc,
	})

	// The instance default's settings are the principal's since D100, and this
	// fixture registers an ordinary account rather than claiming the instance
	// through setup. Granting the one permission is what makes the actor
	// plausible; the subject of this test is the audit record, not the guard.
	grantInstanceScope(t, f.pool, f.owner.UserID, auth.PermDomainsWriteInstance)
	owner, err := newService(f.pool).IdentityForEmail(t.Context(), "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}

	ctx := auth.WithClientIP(t.Context(), netip.MustParseAddr("198.51.100.9"))
	if _, err := links.SetRootRedirect(ctx, owner, "https://example.com/first"); err != nil {
		t.Fatalf("SetRootRedirect: %v", err)
	}
	// A second change, so the "from" of the second record proves the previous
	// value was captured before the write rather than after it.
	if _, err := links.SetRootRedirect(ctx, owner, "https://example.com/second"); err != nil {
		t.Fatalf("second SetRootRedirect: %v", err)
	}

	rows := f.rows()
	if len(rows) != 2 {
		t.Fatalf("%d audit records, want one per change", len(rows))
	}
	for i, r := range rows {
		if r.Action != audit.ActionDomainRootRedirectChanged {
			t.Errorf("record %d action = %q", i, r.Action)
		}
		// Scoped to no organization since M45 (F36). The setter targets the
		// instance default domain — `WHERE is_default`, on a row whose own
		// organization_id is NULL — so this redirects every stray visitor to
		// every workspace on the box. Filing it under whichever organization the
		// actor was standing in named a tenant with no claim to it and hid it
		// from every tenant it changed.
		if r.OrgID != nil {
			t.Errorf("record %d is filed under organization %s; an instance-wide "+
				"act belongs to no tenant", i, *r.OrgID)
		}
		if r.IPPrefix == nil || *r.IPPrefix != "198.51.100.0/24" {
			t.Errorf("record %d ip_prefix = %v, want 198.51.100.0/24", i, r.IPPrefix)
		}
	}

	var second map[string]any
	if err := json.Unmarshal(rows[1].Metadata, &second); err != nil {
		t.Fatalf("metadata is not jsonb-decodable: %v", err)
	}
	if second["from"] != "https://example.com/first" {
		t.Errorf(`metadata["from"] = %v, want the value being replaced; without it a `+
			`reader cannot tell a change from a no-op`, second["from"])
	}
	if second["to"] != "https://example.com/second" {
		t.Errorf(`metadata["to"] = %v`, second["to"])
	}
}

// A failed audit write must not fail the change the operator asked for. The
// setting is what they wanted; losing the record of it is the lesser harm, and
// the alternative — refusing the change — is the greater one.
func TestARefusedAuditWriteDoesNotFailTheChange(t *testing.T) {
	f := newAudit(t)

	links := link.NewService(f.pool, link.Config{
		Policy: link.DefaultDestinationPolicy(), BaseURL: "http://lnk.test",
		SplitHosts: true,
		Audit:      failingRecorder{},
	})

	// The instance permission, for D100's reason — see the audited test above.
	grantInstanceScope(t, f.pool, f.owner.UserID, auth.PermDomainsWriteInstance)
	owner, err := newService(f.pool).IdentityForEmail(t.Context(), "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}

	settings, err := links.SetRootRedirect(t.Context(), owner, "https://example.com/home")
	if err != nil {
		t.Fatalf("SetRootRedirect failed because its audit record could not be "+
			"written: %v", err)
	}
	if settings.RootRedirectURL != "https://example.com/home" {
		t.Errorf("root redirect = %q, want the change to have taken effect",
			settings.RootRedirectURL)
	}
}

type failingRecorder struct{}

func (failingRecorder) Record(context.Context, *auth.Identity, audit.Event) error {
	return context.DeadlineExceeded
}

type countingRootInvalidator struct{ n int }

func (c *countingRootInvalidator) InvalidateRoot() { c.n++ }

// Keyset pagination over the read API, driven through HTTP.
//
// The property that matters is not "a page comes back" but that paging the
// whole log yields every record exactly once. An off-by-one in the cursor
// comparison silently skips or repeats a record, and on an audit log a skipped
// record reads as something that never happened.
func TestAuditListPaginatesWithoutSkippingOrRepeating(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	const total = 25
	seedAuditEvents(t, f.pool, total)

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		path := "/api/v1/audit?limit=10"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		resp := f.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		var page struct {
			Items []struct {
				ID         string `json:"id"`
				Action     string `json:"action"`
				OccurredAt string `json:"occurred_at"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
			HasMore    bool   `json:"has_more"`
		}
		f.decode(resp, &page)

		for _, it := range page.Items {
			seen[it.ID]++
		}
		pages++
		if pages > 10 {
			t.Fatal("pagination did not terminate; the cursor is not advancing")
		}
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Error("next_cursor is set on the last page")
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatal("has_more is true but no cursor was returned; the caller cannot continue")
		}
		cursor = page.NextCursor
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct records across %d pages, want %d", len(seen), pages, total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("record %s appeared %d times; paging must not repeat", id, n)
		}
	}
}

// Newest first is the contract, and it is the ordering the cursor is built
// against — if the sort flipped, the cursor comparison would silently return
// one page and then nothing.
func TestAuditListIsNewestFirst(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	seedAuditEvents(t, f.pool, 5)

	resp := f.do(http.MethodGet, "/api/v1/audit", nil)
	var page struct {
		Items []struct {
			OccurredAt time.Time `json:"occurred_at"`
		} `json:"items"`
	}
	f.decode(resp, &page)

	if len(page.Items) != 5 {
		t.Fatalf("%d items, want 5", len(page.Items))
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i].OccurredAt.After(page.Items[i-1].OccurredAt) {
			t.Fatalf("item %d is newer than item %d; the log is not newest-first", i, i-1)
		}
	}
}

// The permission gate. An editor can change links all day and still must not be
// able to read who else changed what.
func TestAuditListRequiresThePermission(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Strip audit.read from every role this instance has, which is the closest
	// a test can get to "a member who was never granted it" without M28's
	// member management existing yet.
	if _, err := f.pool.Exec(t.Context(), `
		DELETE FROM role_permissions
		 WHERE permission_id = (SELECT id FROM permissions WHERE slug = 'audit.read')`); err != nil {
		t.Fatal(err)
	}

	resp := f.do(http.MethodGet, "/api/v1/audit", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /api/v1/audit without audit.read = %d, want 403", resp.StatusCode)
	}
	f.decode(resp, nil)
}

// D18's first limb, enforced. An API key must not be mintable with audit.read,
// and NonDelegableScopes is the only thing enforcing it — so this is the test
// that would notice if that entry were removed by a merge.
func TestAuditReadCannotBeGrantedToAnAPIKey(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	resp := f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "log shipper", "scopes": []string{"links.read", "audit.read"},
	})
	var problem struct {
		Status int `json:"status"`
		Errors []struct {
			Field, Code string
		} `json:"errors"`
	}
	f.decode(resp, &problem)

	if problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: audit.read must not be grantable to a key", problem.Status)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Code != "not_delegable" {
		t.Errorf("errors = %+v, want one not_delegable on scopes", problem.Errors)
	}
}

// seedAuditEvents writes n records for the instance's only organization,
// spaced so the ordering is unambiguous.
// The workspace an action happened in comes back with it.
//
// The column has been stored, indexed, selected and scanned since M21 and was
// dropped on the way out (F110) — a second choice riding on a documented first
// one, since the query explains at length why the read is not *filtered* by
// workspace and says nothing about not returning it. Without the field a reader
// cannot tell which workspace a link-scoped action came from: those actions name
// the link and nothing else, where `workspace.*` actions carry it as target_id
// and were readable all along.
//
// The organization-level case is asserted beside it, because the fix must not
// turn an absent workspace into a zero uuid — that would read as a real
// workspace nobody can look up.
func TestTheAuditLogSaysWhichWorkspaceAnActionHappenedIn(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	var orgID, wsID uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM organizations LIMIT 1`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM workspaces WHERE organization_id = $1 LIMIT 1`, orgID).Scan(&wsID); err != nil {
		t.Fatal(err)
	}

	// One action inside a workspace, one that belongs to the organization.
	for _, row := range []struct {
		action string
		ws     *uuid.UUID
	}{
		{"link.bot_blocking_changed", &wsID},
		{"invitation.created", nil},
	} {
		if _, err := f.pool.Exec(t.Context(), `
			INSERT INTO audit_logs (id, occurred_at, organization_id, workspace_id,
			                        actor_label, action, metadata)
			VALUES ($1, now(), $2, $3, 'seed@example.com', $4, '{}'::jsonb)`,
			uuid.Must(uuid.NewV7()), orgID, row.ws, row.action); err != nil {
			t.Fatalf("seed %s: %v", row.action, err)
		}
	}

	resp := f.do(http.MethodGet, "/api/v1/audit?limit=50", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/audit = %d", resp.StatusCode)
	}
	var page struct {
		Items []struct {
			Action      string `json:"action"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"items"`
	}
	f.decode(resp, &page)

	got := map[string]string{}
	for _, it := range page.Items {
		got[it.Action] = it.WorkspaceID
	}

	if got["link.bot_blocking_changed"] != wsID.String() {
		t.Errorf("a link-scoped action reported workspace_id %q, want %q — the column "+
			"is stored and selected, and the reader cannot otherwise tell which "+
			"workspace the link was in",
			got["link.bot_blocking_changed"], wsID)
	}
	if w := got["invitation.created"]; w != "" {
		t.Errorf("an organization-level action reported workspace_id %q, want it "+
			"absent; a zero uuid reads as a workspace that cannot be looked up", w)
	}
}

func seedAuditEvents(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	var orgID uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM organizations LIMIT 1`).Scan(&orgID); err != nil {
		t.Fatalf("find the organization: %v", err)
	}

	base := time.Now().UTC().Add(-time.Duration(n) * time.Minute)
	for i := range n {
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO audit_logs (id, occurred_at, organization_id, actor_label, action, metadata)
			VALUES ($1, $2, $3, 'seed@example.com', 'domain.root_redirect_changed', '{}'::jsonb)`,
			uuid.Must(uuid.NewV7()), base.Add(time.Duration(i)*time.Minute), orgID); err != nil {
			t.Fatalf("seed audit record %d: %v", i, err)
		}
	}
}
