//go:build integration

package integration

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// M44: a key replaces itself, into something identical or narrower, and the key
// it replaced stops working when its window closes.
//
// The whole feature exists inside a rule it must not break — `apikeys.*` is not
// delegable, so no key can mint a key. Rotation is the exception that is not an
// exception: nothing is minted *for* anybody, a credential is replaced by a
// weaker or equal copy of itself, and `TestNonDelegableScopesCoverKeyManagement`
// in internal/auth still passes unmodified.

type rotatedKey struct {
	createdKey
	OrgWide        bool    `json:"org_wide"`
	ExpiresAt      *string `json:"expires_at"`
	GraceExpiresAt *string `json:"grace_expires_at"`
	SuccessorID    *string `json:"successor_id"`
	Predecessor    struct {
		ID             string `json:"id"`
		Prefix         string `json:"prefix"`
		StopsWorkingAt string `json:"stops_working_at"`
	} `json:"predecessor"`
}

// rotate sends the rotation as the key itself, which is the only way to send it.
func (f *apiFixture) rotate(token string, body any, wantStatus int) rotatedKey {
	f.t.Helper()
	resp := f.doWithKey(token, http.MethodPost, "/api/v1/api-keys/rotate", body)
	if resp.StatusCode != wantStatus {
		var problem map[string]any
		f.decode(resp, &problem)
		f.t.Fatalf("rotate returned %d, want %d: %v", resp.StatusCode, wantStatus, problem)
	}
	var out rotatedKey
	if wantStatus == http.StatusCreated {
		f.decode(resp, &out)
	} else {
		f.decode(resp, nil)
	}
	return out
}

func TestAPIKeyRotatesItselfIntoASuccessor(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	original := f.createKey("ci-deploy", "links.read", "links.create")
	successor := f.rotate(original.Key, nil, http.StatusCreated)

	if successor.Key == original.Key || successor.Prefix == original.Prefix {
		t.Fatal("the successor is the same credential; rotation must produce a new secret")
	}
	if successor.Predecessor.ID != original.ID || successor.Predecessor.Prefix != original.Prefix {
		t.Errorf("the response names predecessor %+v, want the key that made the request (%s / %s)",
			successor.Predecessor, original.ID, original.Prefix)
	}
	if len(successor.Scopes) != 2 {
		t.Errorf("successor scopes = %v, want the predecessor's two", successor.Scopes)
	}

	// Both verify while the window is open. This is the property the grace
	// window is *for*: a deployment mid-rollout has consumers holding either.
	for name, token := range map[string]string{
		"the successor":       successor.Key,
		"the key it replaced": original.Key,
	} {
		resp := f.doWithKey(token, http.MethodGet, "/api/v1/links", nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s returned %d during the grace window, want 200", name, resp.StatusCode)
		}
	}

	// The predecessor's row carries the deadline, and the chain points forwards.
	var graceAt *time.Time
	var successorID *uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT grace_expires_at, successor_id FROM api_keys WHERE id = $1`,
		original.ID).Scan(&graceAt, &successorID); err != nil {
		t.Fatal(err)
	}
	if graceAt == nil || successorID == nil {
		t.Fatalf("the predecessor row has grace_expires_at=%v successor_id=%v; both are set together", graceAt, successorID)
	}
	if successorID.String() != successor.ID {
		t.Errorf("predecessor points at %s, want the successor %s", successorID, successor.ID)
	}
	if want := time.Hour; time.Until(*graceAt) > want+time.Minute {
		t.Errorf("the default grace window is %s, want about %s", time.Until(*graceAt), want)
	}

	// Rotation is audited. It is the one administrative change to a credential
	// that happens with nobody signed in, so the record is the only trace.
	var action, predPrefix, succPrefix string
	if err := f.pool.QueryRow(t.Context(), `
		SELECT action, metadata->>'prefix', metadata->>'successor_prefix'
		  FROM audit_logs
		 WHERE action = 'apikey.rotated'
		 ORDER BY occurred_at DESC LIMIT 1`).Scan(&action, &predPrefix, &succPrefix); err != nil {
		t.Fatalf("no apikey.rotated record was written: %v", err)
	}
	if predPrefix != original.Prefix || succPrefix != successor.Prefix {
		t.Errorf("the record says %s -> %s, want %s -> %s",
			predPrefix, succPrefix, original.Prefix, successor.Prefix)
	}

	// And it names the key that acted, which for a rotation is the key itself.
	var actorKey *uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT actor_api_key_id FROM audit_logs WHERE action = 'apikey.rotated' ORDER BY occurred_at DESC LIMIT 1`,
	).Scan(&actorKey); err != nil {
		t.Fatal(err)
	}
	if actorKey == nil || actorKey.String() != original.ID {
		t.Errorf("the record's actor key is %v, want the predecessor %s", actorKey, original.ID)
	}
}

// The claim that makes the grace window bounded rather than decorative.
//
// Written so that it **exercises the old secret after the window closes**, which
// is the only way to tell this apart from a test that merely asserts a column
// was written. The window has a five-minute floor, so the clock is moved by
// ageing the row rather than by sleeping — the alternative is a test that takes
// five minutes and still proves nothing about what happens at the boundary.
func TestRotatedPredecessorStopsVerifyingWhenItsGraceCloses(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	original := f.createKey("expiring", "links.read")
	successor := f.rotate(original.Key, nil, http.StatusCreated)

	// Before: the old secret works. Asserted rather than assumed, so a later
	// failure cannot be a key that never worked in the first place.
	resp := f.doWithKey(original.Key, http.MethodGet, "/api/v1/links", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the predecessor returned %d inside its window, want 200", resp.StatusCode)
	}

	// The window closes. Only grace_expires_at moves — revoked_at is deliberately
	// left NULL, so this proves the refusal comes from the grace column and not
	// from the revocation the housekeeping job writes later.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE api_keys SET grace_expires_at = now() - interval '1 second' WHERE id = $1`,
		original.ID); err != nil {
		t.Fatal(err)
	}

	resp = f.doWithKey(original.Key, http.MethodGet, "/api/v1/links", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the predecessor returned %d after its grace window closed, want 401. "+
			"A grace window that does not end is a second live credential, and D9's "+
			"accepted trade rests on this being finite", resp.StatusCode)
	}

	// The successor is untouched by any of it.
	resp = f.doWithKey(successor.Key, http.MethodGet, "/api/v1/links", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the successor returned %d, want 200: closing the predecessor's window "+
			"must not touch the key that replaced it", resp.StatusCode)
	}

	// Housekeeping then makes the key list agree with that behaviour. It is
	// bookkeeping, not enforcement — the 401 above happened without it — and the
	// timestamp it writes is when the window closed, not when the job ran.
	q := dbgen.New(f.pool)
	if _, err := q.RevokeLapsedAPIKeyGraces(t.Context()); err != nil {
		t.Fatal(err)
	}
	var revokedAt, graceAt *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT revoked_at, grace_expires_at FROM api_keys WHERE id = $1`,
		original.ID).Scan(&revokedAt, &graceAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt == nil {
		t.Fatal("the sweep left a lapsed predecessor unrevoked; the key list would show it as active " +
			"while it authenticates nothing")
	}
	if !revokedAt.Equal(*graceAt) {
		t.Errorf("revoked_at = %s, want the moment the window closed (%s) rather than when the job ran",
			revokedAt, graceAt)
	}

	// And the successor is not swept, which is the half of the sweep that would
	// be silently destructive if the predicate were wrong.
	var successorRevoked *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT revoked_at FROM api_keys WHERE id = $1`, successor.ID).Scan(&successorRevoked); err != nil {
		t.Fatal(err)
	}
	if successorRevoked != nil {
		t.Errorf("the sweep revoked the successor at %s; it has no grace window of its own", successorRevoked)
	}
}

// The other half of "identical or narrower", over the wire.
func TestRotationRefusesToWidenOverTheAPI(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Minted by an owner who holds everything, but granted one scope. The key
	// must not be able to reach the rest of its owner's role through a rotation.
	original := f.createKey("read-only", "links.read")

	resp := f.doWithKey(original.Key, http.MethodPost, "/api/v1/api-keys/rotate",
		map[string]any{"scopes": []string{"links.read", "links.delete"}})
	var problem struct {
		Status int                            `json:"status"`
		Errors []struct{ Field, Code string } `json:"errors"`
	}
	f.decode(resp, &problem)

	if problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("widening a key through rotation returned %d, want 422", problem.Status)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Field != "scopes" ||
		problem.Errors[0].Code != "not_held" {
		t.Errorf("errors = %+v, want one not_held on scopes", problem.Errors)
	}

	// apikeys.write is refused for a second reason, and the reason matters: it is
	// not that the key lacks it, it is that no key may ever hold it.
	resp = f.doWithKey(original.Key, http.MethodPost, "/api/v1/api-keys/rotate",
		map[string]any{"scopes": []string{"apikeys.write"}})
	f.decode(resp, &problem)
	if problem.Status != http.StatusUnprocessableEntity {
		t.Errorf("rotating into apikeys.write returned %d, want 422; a key that could mint "+
			"keys makes revocation meaningless", problem.Status)
	}

	// Narrowing works, and the successor really is narrower.
	original2 := f.createKey("two-scopes", "links.read", "links.create")
	narrowed := f.rotate(original2.Key, map[string]any{"scopes": []string{"links.read"}},
		http.StatusCreated)
	if len(narrowed.Scopes) != 1 || narrowed.Scopes[0] != "links.read" {
		t.Fatalf("narrowed successor holds %v, want [links.read]", narrowed.Scopes)
	}
	resp = f.doWithKey(narrowed.Key, http.MethodPost, "/api/v1/links",
		map[string]any{"url": "https://example.com/should-be-refused"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("the narrowed successor created a link (%d); the scope it dropped must be gone",
			resp.StatusCode)
	}
}

// A key minted before a scope became non-delegable is the one holder that rule
// never met: the mint path refuses the scope today, so such a key exists only
// as history, and rotation is where its scope would otherwise ride forward
// forever. The refusal must land on what the successor would hold — inheriting
// carries the scope, so it refuses; naming the scope carries it just the same,
// so it refuses identically; omitting it is the way forward the refusal itself
// names, and it must keep the grace overlap that makes rotation worth having
// over revoke-and-re-mint.
func TestRotationDropsANowNonDelegableScopeButWillNotCarryIt(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	original := f.createKey("pre-m43", "links.read")
	// Create refuses webhooks.write, so the historical key is written the way it
	// actually came to exist: the row predates the rule.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE api_keys SET scopes = ARRAY['links.read','webhooks.write'] WHERE id = $1`,
		original.ID); err != nil {
		t.Fatal(err)
	}

	var problem struct {
		Status int                            `json:"status"`
		Errors []struct{ Field, Code string } `json:"errors"`
	}

	// Inheriting (no scopes named) would carry the scope into the successor.
	resp := f.doWithKey(original.Key, http.MethodPost, "/api/v1/api-keys/rotate", nil)
	f.decode(resp, &problem)
	if problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("inheriting a now-non-delegable scope returned %d, want 422", problem.Status)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Field != "scopes" ||
		problem.Errors[0].Code != "not_delegable" {
		t.Errorf("errors = %+v, want one not_delegable on scopes", problem.Errors)
	}

	// Naming it is the same carry-forward, spelled out.
	resp = f.doWithKey(original.Key, http.MethodPost, "/api/v1/api-keys/rotate",
		map[string]any{"scopes": []string{"links.read", "webhooks.write"}})
	f.decode(resp, &problem)
	if problem.Status != http.StatusUnprocessableEntity ||
		len(problem.Errors) != 1 || problem.Errors[0].Code != "not_delegable" {
		t.Errorf("re-requesting the scope gave %d %+v, want 422 with one not_delegable",
			problem.Status, problem.Errors)
	}

	// Omitting it succeeds, and the predecessor still verifies inside its grace
	// window — the overlap a forced revoke-and-re-mint would not have.
	successor := f.rotate(original.Key, map[string]any{"scopes": []string{"links.read"}},
		http.StatusCreated)
	if len(successor.Scopes) != 1 || successor.Scopes[0] != "links.read" {
		t.Errorf("successor holds %v, want [links.read]", successor.Scopes)
	}
	for name, token := range map[string]string{
		"the successor":   successor.Key,
		"the predecessor": original.Key,
	} {
		r := f.doWithKey(token, http.MethodGet, "/api/v1/links", nil)
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("%s returned %d during the grace window, want 200", name, r.StatusCode)
		}
	}
}

func TestASessionCannotRotateAndAKeyRotatesOnce(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// A session is signed in as an owner holding apikeys.write, and still cannot
	// rotate: rotation replaces the credential that made the request.
	resp := f.do(http.MethodPost, "/api/v1/api-keys/rotate", map[string]any{})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a session rotated a key (%d), want 403", resp.StatusCode)
	}

	original := f.createKey("once", "links.read")
	f.rotate(original.Key, nil, http.StatusCreated)

	// The second attempt is a 409 and not a 401: the caller is holding a
	// perfectly valid credential that has simply already been replaced, and
	// answering "invalid" would send an automated rotation into a retry loop.
	f.rotate(original.Key, nil, http.StatusConflict)

	var successors int64
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM api_keys WHERE id != $1 AND user_id = (SELECT user_id FROM api_keys WHERE id = $1)`,
		original.ID).Scan(&successors); err != nil {
		t.Fatal(err)
	}
	if successors != 1 {
		t.Errorf("the key has %d successors, want 1; a key that fans out turns D9's chain "+
			"into a tree and revocation into a search", successors)
	}
}

func TestRotationKeepsTheWorkspaceBindingAndTheLifetime(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// An expiry, so the lifetime rule has something to preserve.
	resp := f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "bound", "scopes": []string{"links.read"},
		"expires_at": time.Now().Add(720 * time.Hour).UTC().Format(time.RFC3339),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create returned %d", resp.StatusCode)
	}
	var original rotatedKey
	f.decode(resp, &original)

	successor := f.rotate(original.Key, nil, http.StatusCreated)

	var predWS, succWS *uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT (SELECT workspace_id FROM api_keys WHERE id = $1),
		        (SELECT workspace_id FROM api_keys WHERE id = $2)`,
		original.ID, successor.ID).Scan(&predWS, &succWS); err != nil {
		t.Fatal(err)
	}
	if predWS == nil || succWS == nil || *predWS != *succWS {
		t.Errorf("workspace binding moved: predecessor %v, successor %v. A rotation copies "+
			"it verbatim; changing reach is the creator's choice and it is made once",
			predWS, succWS)
	}

	if successor.ExpiresAt == nil {
		t.Fatal("a key with an expiry rotated into one with none")
	}
	at, err := time.Parse(time.RFC3339, *successor.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(at); d < 719*time.Hour || d > 721*time.Hour {
		t.Errorf("successor expires in %s, want about the predecessor's 720-hour lifetime "+
			"measured from now", d)
	}
}

// The workspace choice the 00500 column comment promised and nothing ever made
// (M44). Two halves: the default is unchanged, and the wider option is gated on
// a membership that is itself organization-wide.
func TestOrgWideKeyNeedsAnOrganizationWideMembership(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// The default. Every key issued before M44 was bound to one workspace, and
	// leaving the field alone must still produce exactly that.
	def := f.createKey("default", "links.read")
	var defWS *uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT workspace_id FROM api_keys WHERE id = $1`, def.ID).Scan(&defWS); err != nil {
		t.Fatal(err)
	}
	if defWS == nil {
		t.Fatal("a key created without asking is organization-wide; the default must be the " +
			"single-workspace behaviour it has always had")
	}

	// Asked for, by an owner whose membership covers the organization.
	resp := f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "everywhere", "scopes": []string{"links.read"}, "org_wide": true,
	})
	if resp.StatusCode != http.StatusCreated {
		var problem map[string]any
		f.decode(resp, &problem)
		t.Fatalf("an organization-wide owner was refused an organization-wide key: %d %v",
			resp.StatusCode, problem)
	}
	var wide rotatedKey
	f.decode(resp, &wide)
	if !wide.OrgWide {
		t.Error("the response does not report the key as organization-wide")
	}
	var wideWS *uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT workspace_id FROM api_keys WHERE id = $1`, wide.ID).Scan(&wideWS); err != nil {
		t.Fatal(err)
	}
	if wideWS != nil {
		t.Errorf("the organization-wide key is bound to workspace %s", wideWS)
	}
	// It still authenticates: a NULL workspace resolves the way a login does.
	authed := f.doWithKey(wide.Key, http.MethodGet, "/api/v1/links", nil)
	_ = authed.Body.Close()
	if authed.StatusCode != http.StatusOK {
		t.Errorf("the organization-wide key returned %d, want 200", authed.StatusCode)
	}

	// A **pinned** key resolves inside its own organization, and not wherever its
	// owner happens to have been last.
	//
	// The precedence that answers "which workspace is this request in" filters on
	// membership alone, and a pinned default is a property of the person rather
	// than of the tenancy — so an owner who belongs to a second organization and
	// pinned a workspace there would, without a bound, have this key resolve into
	// that tenant carrying its scopes with it. Nothing could reach that before
	// M44, because nothing had ever written a NULL workspace_id.
	//
	// **The key asserted on is pinned explicitly since M54**, and that one word is
	// the whole of what changed. This test used `wide` above, which was an
	// organization-wide key and is now an account-wide one: M54 made the unpinned
	// default reach the organizations its owner belongs to, so `wide` following
	// its owner into Elsewhere is now correct rather than the leak this paragraph
	// describes. The bound itself is untouched and still exactly what M44 built —
	// what changed is which keys carry it, and a key that carries it is one whose
	// creator asked for it by naming an organization.
	var ownerID, sessionOrg uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM users WHERE email_lower = 'owner@example.com'`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(),
		`SELECT organization_id FROM api_keys WHERE id = $1`, def.ID).Scan(&sessionOrg); err != nil {
		t.Fatal(err)
	}
	pinnedResp := f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "pinned-here", "scopes": []string{"links.read"},
		"org_wide": true, "organization_id": sessionOrg.String(),
	})
	if pinnedResp.StatusCode != http.StatusCreated {
		var problem map[string]any
		f.decode(pinnedResp, &problem)
		t.Fatalf("pinning a key to the organization being acted in was refused: %d %v",
			pinnedResp.StatusCode, problem)
	}
	var pinned rotatedKey
	f.decode(pinnedResp, &pinned)

	other := addOrgWithWorkspace(t, f.pool, ownerID, "Elsewhere", "Their Space")
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE users SET default_workspace_id = $1, last_workspace_id = $1 WHERE id = $2`,
		other, ownerID); err != nil {
		t.Fatal(err)
	}

	meResp := f.doWithKey(pinned.Key, http.MethodGet, "/api/v1/me", nil)
	var me struct {
		WorkspaceID string `json:"workspace_id"`
	}
	f.decode(meResp, &me)
	if me.WorkspaceID == other.String() {
		t.Fatalf("a pinned key issued in one organization resolved into "+
			"workspace %s, which belongs to another one its owner also joined. "+
			"Pinned means every workspace in *this* organization",
			me.WorkspaceID)
	}
	var resolvedOrg uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT organization_id FROM workspaces WHERE id = $1`, me.WorkspaceID).Scan(&resolvedOrg); err != nil {
		t.Fatal(err)
	}
	var keyOrg uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT organization_id FROM api_keys WHERE id = $1`, pinned.ID).Scan(&keyOrg); err != nil {
		t.Fatal(err)
	}
	if resolvedOrg != keyOrg {
		t.Errorf("the key resolved into organization %s, want its own %s", resolvedOrg, keyOrg)
	}

	// And the key that named no organization does the opposite, deliberately.
	// Stated here rather than left implied, because the two rows differ by one
	// field and reading either one alone would suggest the bound is universal.
	wideMe := f.doWithKey(wide.Key, http.MethodGet, "/api/v1/me", nil)
	var wideWhere struct {
		WorkspaceID string `json:"workspace_id"`
	}
	f.decode(wideMe, &wideWhere)
	if wideWhere.WorkspaceID != other.String() {
		t.Errorf("an account-wide key resolved into %s, want the organization its "+
			"owner moved to (%s). Account-wide is the reach a key created without "+
			"an organization now has", wideWhere.WorkspaceID, other)
	}

	// Now the same owner, holding the same role, through a membership scoped to
	// one workspace. `Can(apikeys.write)` still answers yes — the union covers
	// the workspace being acted in — and that is exactly why the check is
	// `In(nil)` and not `Can`. This is F27's shape: a workspace-scoped role must
	// not issue a credential that reaches the whole organization.
	if _, err := f.pool.Exec(t.Context(), `
		UPDATE memberships
		   SET workspace_id = (SELECT id FROM workspaces
		                        WHERE organization_id = memberships.organization_id
		                        ORDER BY created_at LIMIT 1)`); err != nil {
		t.Fatal(err)
	}

	resp = f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "overreach", "scopes": []string{"links.read"}, "org_wide": true,
	})
	var problem struct {
		Status int                            `json:"status"`
		Errors []struct{ Field, Code string } `json:"errors"`
	}
	f.decode(resp, &problem)
	if problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("a workspace-scoped role minted an organization-wide key (%d, want 422). "+
			"D44 authorizes a write against the membership whose scope covers its target, "+
			"and this target is the organization", problem.Status)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Field != "org_wide" {
		t.Errorf("errors = %+v, want one on org_wide", problem.Errors)
	}

	// The narrow key is still issuable, which is the point of refusing only the
	// wide one.
	resp = f.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "still fine", "scopes": []string{"links.read"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("a workspace-scoped role was refused a workspace key (%d)", resp.StatusCode)
	}
}

// The removal that lands between authenticating and rotating.
//
// Authentication now refuses a key whose owner holds no membership, so the
// ordinary path is closed — but a rotation authenticated a moment earlier is
// still in flight, and the identity it carries was resolved while the membership
// stood. This is the same reason revocation is re-checked under the lock: a
// removal that loses this race would mint the one credential it could not
// reach, because the successor is a new row nobody is watching and it can rotate
// again.
//
// Driven at the service, which is the only way to hold an identity across the
// removal: over HTTP the two are one request and authentication answers first.
func TestRotationRefusesAnIdentityWhoseMembershipWentAwayMidFlight(t *testing.T) {
	f := newTeamFixture(t)

	created, err := f.keys.Create(t.Context(), f.owner, auth.CreateAPIKeyInput{
		Name: "mid-flight", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatalf("mint a key: %v", err)
	}
	key, err := f.keys.Authenticate(t.Context(), created.Key)
	if err != nil {
		t.Fatalf("authenticate the key: %v", err)
	}

	if _, err := f.pool.Exec(t.Context(),
		`DELETE FROM memberships WHERE user_id = $1`, f.owner.UserID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.keys.Rotate(t.Context(), key, auth.RotateAPIKeyInput{}); !errors.Is(err, auth.ErrAPIKeyInvalid) {
		t.Errorf("a key rotated on an identity whose membership had gone: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM api_keys WHERE successor_id IS NOT NULL`); n != 0 {
		t.Error("a refused rotation wrote a successor anyway")
	}

	// The same for a deactivated account, which the lock re-reads beside the
	// membership.
	f2 := newTeamFixture(t)
	created2, err := f2.keys.Create(t.Context(), f2.owner, auth.CreateAPIKeyInput{
		Name: "deactivated", Scopes: []string{"links.read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	key2, err := f2.keys.Authenticate(t.Context(), created2.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.pool.Exec(t.Context(),
		`UPDATE users SET status = 'suspended' WHERE id = $1`, f2.owner.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.keys.Rotate(t.Context(), key2, auth.RotateAPIKeyInput{}); !errors.Is(err, auth.ErrAPIKeyInvalid) {
		t.Errorf("a key rotated on an identity whose account had been suspended: %v", err)
	}
}
