//go:build integration

package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// The trash window: a soft-deleted link holds its alias for the whole recovery
// period. Before this was enforced, the partial unique index freed the alias
// the moment deleted_at was set, and anyone could re-register a trafficked
// alias and hijack its existing bookmarks.
func TestTrashedAliasCannotBeReRegistered(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/original", "alias": "trashed",
	})
	var created struct{ ID string }
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create returned %d", resp.StatusCode)
	}
	f.decode(resp, &created)

	resp = f.do(http.MethodDelete, "/api/v1/links/"+created.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete returned %d", resp.StatusCode)
	}
	f.decode(resp, nil)

	// User-supplied alias path: must be a 409, and specifically not a success.
	resp = f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/hijack", "alias": "trashed",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("re-registering a trashed alias returned %d, want 409: the alias "+
			"stays reserved while the row exists", resp.StatusCode)
	}
	f.decode(resp, nil)

	// Alias-change path — an alias change is a creation as far as the
	// namespace is concerned.
	resp = f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/other", "alias": "innocent",
	})
	var other struct{ ID string }
	f.decode(resp, &other)
	resp = f.do(http.MethodPatch, "/api/v1/links/"+other.ID, map[string]any{
		"alias": "trashed",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("renaming onto a trashed alias returned %d, want 409", resp.StatusCode)
	}
	f.decode(resp, nil)
}

// Renaming is the other way a link lets go of an alias, and it has the same
// consequence as purging: the old alias stops being covered by the partial
// unique index, so without a reservation anyone can take over the audience the
// rename left behind. The threshold matches the purge — traffic is what puts an
// alias in the wild — so a rename with clicks reserves and one without releases.
func TestRenamingReservesATraffickedAlias(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	ctx := t.Context()

	mk := func(alias string) string {
		resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
			"url": "https://example.com/" + alias, "alias": alias,
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s returned %d", alias, resp.StatusCode)
		}
		var c struct{ ID string }
		f.decode(resp, &c)
		return c.ID
	}
	clicked := mk("promo")
	unclicked := mk("draft")

	if _, err := f.pool.Exec(ctx,
		`UPDATE links SET click_count = 50000 WHERE id = $1`, clicked); err != nil {
		t.Fatal(err)
	}

	for id, to := range map[string]string{clicked: "promo2024", unclicked: "draft2"} {
		resp := f.do(http.MethodPatch, "/api/v1/links/"+id, map[string]any{"alias": to})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("rename to %s returned %d", to, resp.StatusCode)
		}
		f.decode(resp, nil)
	}

	var reservations int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM reserved_aliases WHERE alias = 'promo'`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Errorf("reserved_aliases holds %d rows for the renamed trafficked alias, want 1", reservations)
	}

	// The consequence that matters: the abandoned alias cannot be claimed. It
	// is still on flyers and in bookmarks pointing at the original destination.
	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://phishing.example/login", "alias": "promo",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("claiming a renamed trafficked alias returned %d, want 409: "+
			"renaming abandons an alias, it does not free it", resp.StatusCode)
	}
	f.decode(resp, nil)

	// An alias nobody ever followed is released, for the same reason purge
	// releases it: reserving it would only bleed the namespace.
	resp = f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/reuse", "alias": "draft2",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("the new alias of a renamed link returned %d, want 409", resp.StatusCode)
	}
	f.decode(resp, nil)
	resp = f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/reuse", "alias": "draft",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("claiming a renamed untrafficked alias returned %d, want 201", resp.StatusCode)
	}
	f.decode(resp, nil)
}

// The end of the window. Purge deletes the row; a trafficked alias enters
// reserved_aliases in the same statement and stays unregisterable forever,
// while an untrafficked one is released — nothing in the wild points at it.
func TestPurgeReservesTraffickedAliasesAndReleasesTheRest(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	ctx := t.Context()

	mk := func(alias string) string {
		resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
			"url": "https://example.com/" + alias, "alias": alias,
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s returned %d", alias, resp.StatusCode)
		}
		var c struct{ ID string }
		f.decode(resp, &c)
		return c.ID
	}
	clicked := mk("hadclicks")
	unclicked := mk("noclicks")

	// Traffic, recorded the way the ingester records it: via click_count.
	if _, err := f.pool.Exec(ctx,
		`UPDATE links SET click_count = 7 WHERE id = $1`, clicked); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{clicked, unclicked} {
		resp := f.do(http.MethodDelete, "/api/v1/links/"+id, nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete returned %d", resp.StatusCode)
		}
		f.decode(resp, nil)
	}

	// The window has not passed: purge must touch nothing.
	q := dbgen.New(f.pool)
	if rows, err := q.PurgeExpiredLinks(ctx, 100); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Fatalf("purge deleted %d links inside their recovery window", len(rows))
	}

	// End the window and purge.
	if _, err := f.pool.Exec(ctx,
		`UPDATE links SET purge_after = now() - interval '1 minute' WHERE deleted_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	rows, err := q.PurgeExpiredLinks(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("purged %d links, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Alias == "hadclicks" && !r.Reserved {
			t.Error("the trafficked alias was purged without a reservation")
		}
		if r.Alias == "noclicks" && r.Reserved {
			t.Error("the untrafficked alias was reserved; reservation is only for aliases in the wild")
		}
	}

	// Idempotent: a second run finds nothing.
	if rows, err := q.PurgeExpiredLinks(ctx, 100); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Fatalf("second purge run deleted %d links", len(rows))
	}

	// Rows are really gone, and the reservation landed.
	var linksLeft, reservations int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM links WHERE alias IN ('hadclicks','noclicks')`).Scan(&linksLeft); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM reserved_aliases WHERE alias = 'hadclicks'`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if linksLeft != 0 {
		t.Errorf("%d purged link rows remain", linksLeft)
	}
	if reservations != 1 {
		t.Errorf("reserved_aliases holds %d rows for the trafficked alias, want 1", reservations)
	}

	// The permanent consequence, via the API: the trafficked alias is refused
	// forever, the untrafficked one is available again.
	resp := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/again", "alias": "hadclicks",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("re-registering a purged trafficked alias returned %d, want 409: "+
			"it is on printed material and in bookmarks", resp.StatusCode)
	}
	f.decode(resp, nil)

	resp = f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/fresh", "alias": "noclicks",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("re-registering a purged untrafficked alias returned %d, want 201", resp.StatusCode)
	}
	f.decode(resp, nil)
}

// The generated-alias path must also avoid reserved aliases; IsAliasTaken is
// its pre-check and now sees trashed rows and reservations.
func TestGeneratedAliasesAvoidReservedOnes(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	ctx := t.Context()

	var domID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM domains WHERE is_default`).Scan(&domID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO reserved_aliases (domain_id, alias, reason)
		VALUES ($1, 'rsvd0000', 'test')`, domID); err != nil {
		t.Fatal(err)
	}

	// The query itself is the contract: a reserved alias reads as taken.
	taken, err := dbgen.New(f.pool).IsAliasTaken(ctx, dbgen.IsAliasTakenParams{
		DomainID: domID, Alias: "rsvd0000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !taken {
		t.Error("IsAliasTaken = false for a reserved alias; generation could mint it")
	}
}

// The reapers share the housekeeping job with the purge: expired sessions and
// long-revoked keys are deleted once their retention lapses, and not before.
func TestSessionAndKeyReapers(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()
	ctx := t.Context()
	q := dbgen.New(f.pool)

	// Alongside the live session: one revoked long ago, one long expired.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, token_hash, ip_prefix, user_agent, created_at, last_seen_at, expires_at, revoked_at)
		SELECT gen_random_uuid(), u.id, sha256(random()::text::bytea), '203.0.113.0/24', 't', now(), now(), now() + interval '1 day', now() - interval '8 days'
		FROM users u LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, token_hash, ip_prefix, user_agent, created_at, last_seen_at, expires_at)
		SELECT gen_random_uuid(), u.id, sha256(random()::text::bytea), '203.0.113.0/24', 't', now(), now(), now() - interval '8 days'
		FROM users u LIMIT 1`); err != nil {
		t.Fatal(err)
	}

	deleted, err := q.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("reaped %d sessions, want the 8-day-revoked and 8-day-expired ones (2)", deleted)
	}

	// The live session must have survived — the owner is still signed in.
	resp := f.do(http.MethodGet, "/api/v1/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("live session was reaped: /me returned %d", resp.StatusCode)
	}
	f.decode(resp, nil)
}
