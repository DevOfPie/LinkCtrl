//go:build integration

// Package integration exercises the real database.
//
// Run with: go test -tags=integration ./test/integration/...
// Needs TEST_DATABASE_URL, or falls back to the compose dev port.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// newDB gives each test its own database, created from a migrated template.
//
// CREATE DATABASE ... TEMPLATE costs about 30ms against roughly 2s to
// re-migrate, and unlike wrapping each test in a transaction it works with
// code that opens its own transactions — which Register does.

// fastParams keep argon2 at the RFC floor so the suite stays quick. The cost
// policy itself is covered by unit tests.
var fastParams = auth.Params{MemoryKiB: 19 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

// grantInstanceScope confers one instance-level permission on an account.
//
// Fixtures register their owner with auth.Register rather than claiming the
// instance through /auth/setup, so they get an ordinary organization owner and
// not the instance principal. Since D100 that distinction reaches the instance
// default domain's settings, so a test whose subject is something else — the
// audit record, the destination tiers — says so here rather than being rewritten
// around a guard it is not about.
func grantInstanceScope(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, scope string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO instance_grants (user_id, permission_id)
		 SELECT $1, id FROM permissions WHERE slug = $2
		 ON CONFLICT DO NOTHING`, userID, scope); err != nil {
		t.Fatalf("grant %s: %v", scope, err)
	}
}

func newService(pool *pgxpool.Pool) *auth.Service {
	return auth.NewService(pool, auth.ServiceConfig{
		Params:  fastParams,
		TTL:     auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
		Lockout: auth.LockoutPolicy{Threshold: 5, Window: 15 * time.Minute},
	})
}

func TestMigrationsProduceExpectedSchema(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()

	var tables int
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p')
		  AND c.relispartition = false AND c.relname <> 'goose_db_version'`).Scan(&tables)
	if err != nil {
		t.Fatal(err)
	}
	// 31 was the Phase 1 count: all 20 Plan.md entities, plus the join and
	// support tables they need. Seven Phase 2 tables have been added since, and
	// none of them is an entity: mail_outbox is the delivery queue decision D23
	// chose over an in-memory retry, invitations is the grant M27 issues,
	// pending_registrations is M29's waiting room — a self-serve registration
	// that has not proven its address yet, which is why no account exists for
	// it — blocked_destinations is M30's low-confidence blocklist, the one tier
	// of three that is meant to change without a rebuild,
	// destination_disputes is M31's appeal against exactly that list, and
	// link_click_budget is M35's durable click counter — a table rather than a
	// column on `links` because `links.click_count` is approximate by design and
	// gating on it would make a lossy asynchronous counter into an
	// authorization boundary. And instance_grants is M45's instance-level
	// principal (D98) — a permission held by a person over the whole instance
	// rather than through a membership, which is the one thing this schema had no
	// way to express and the reason F15, F31 and F36 all bottomed out at D38.
	// Each is live and typed rather than dormant jsonb, because the feature that
	// reads it arrived in the same commit. The number moves and the sentence says
	// why, rather than the count silently growing whenever somebody adds a table.
	if tables != 38 {
		t.Errorf("got %d tables, want 38 (all 20 Plan.md entities, plus mail_outbox, "+
			"invitations, pending_registrations, blocked_destinations, "+
			"destination_disputes, link_click_budget and instance_grants)", tables)
	}

	// The UTC guarantee the partition scheme depends on.
	var tz string
	if err := pool.QueryRow(ctx, "SHOW timezone").Scan(&tz); err != nil {
		t.Fatal(err)
	}
	if tz != "UTC" {
		t.Errorf("session timezone = %q, want UTC; partition bounds would be created at the wrong offset", tz)
	}
}

func TestRegisterProvisionsTenancyAtomically(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	needs, err := svc.NeedsSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !needs {
		t.Fatal("a fresh instance should report NeedsSetup")
	}

	id, err := svc.Register(ctx, auth.RegisterInput{
		Email: "Owner@Example.COM", Name: "Owner", Password: "a-sufficiently-long-password",
		IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if id.Email != "owner@example.com" {
		t.Errorf("email = %q, want it normalized to lowercase", id.Email)
	}
	if id.WorkspaceID == uuid.Nil || id.OrgID == uuid.Nil {
		t.Fatal("registration did not provision an organization and workspace")
	}
	if id.Role != "owner" {
		t.Errorf("role = %q, want owner", id.Role)
	}

	// An owner holds every permission, including the one only they should have.
	for _, p := range []string{"links.create", "links.delete", "apikeys.write", "org.delete"} {
		if !id.Can(p) {
			t.Errorf("owner cannot %q", p)
		}
	}

	if needs, _ := svc.NeedsSetup(ctx); needs {
		t.Error("NeedsSetup still true after the first user was created")
	}
}

func TestRegisterRejectsDuplicateEmailCaseInsensitively(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	if _, err := svc.Register(ctx, auth.RegisterInput{
		Email: "dup@example.com", Password: "a-sufficiently-long-password",
	}); err != nil {
		t.Fatal(err)
	}

	// Different casing must collide, or two accounts exist for one address and
	// login becomes ambiguous.
	_, err := svc.Register(ctx, auth.RegisterInput{
		Email: "DUP@Example.com", Password: "another-long-enough-password",
	})
	if !errors.Is(err, auth.ErrEmailTaken) {
		t.Errorf("second registration = %v, want ErrEmailTaken", err)
	}
}

// TestRegisterRollsBackEverythingOnFailure proves the transaction boundary. A
// user without a workspace is a state no other code path expects.
func TestRegisterIsAtomic(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	// Force a failure after the user insert by removing the owner role, which
	// Register looks up partway through.
	if _, err := pool.Exec(ctx, "DELETE FROM role_permissions"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM roles WHERE slug = 'owner'"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Register(ctx, auth.RegisterInput{
		Email: "rollback@example.com", Password: "a-sufficiently-long-password",
	}); err == nil {
		t.Fatal("Register succeeded with no owner role present")
	}

	var users, orgs int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users)
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM organizations").Scan(&orgs)
	if users != 0 || orgs != 0 {
		t.Errorf("after a failed registration: %d users and %d organizations remain; "+
			"the transaction did not roll back", users, orgs)
	}
}

func TestLoginAndSessionLifecycle(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	const email, password = "user@example.com", "a-sufficiently-long-password"
	if _, err := svc.Register(ctx, auth.RegisterInput{Email: email, Password: password}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Login(ctx, auth.LoginInput{
		Email: email, Password: password,
		IP:        netip.MustParseAddr("203.0.113.42"),
		UserAgent: "Mozilla/5.0 test",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.Token == "" || res.Identity.SessionID == uuid.Nil {
		t.Fatal("login returned no session")
	}

	// The raw token must not be recoverable from the database.
	var stored []byte
	if err := pool.QueryRow(ctx, "SELECT token_hash FROM sessions WHERE id = $1",
		res.Identity.SessionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) == res.Token {
		t.Error("the session token is stored verbatim; a database leak would hand over live sessions")
	}

	// The IP must be stored anonymized.
	var ipPrefix *string
	_ = pool.QueryRow(ctx, "SELECT ip_prefix FROM sessions WHERE id = $1",
		res.Identity.SessionID).Scan(&ipPrefix)
	if ipPrefix == nil || *ipPrefix != "203.0.113.0/24" {
		t.Errorf("ip_prefix = %v, want the anonymized 203.0.113.0/24", ipPrefix)
	}

	// The token authenticates.
	id, err := svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.UserID != res.Identity.UserID {
		t.Error("Authenticate resolved to a different user")
	}

	// And stops working after logout.
	if err := svc.Logout(ctx, res.Identity.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, res.Token); err == nil {
		t.Error("a revoked session still authenticates")
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	const email, password = "real@example.com", "a-sufficiently-long-password"
	if _, err := svc.Register(ctx, auth.RegisterInput{Email: email, Password: password}); err != nil {
		t.Fatal(err)
	}

	// Unknown account and wrong password must be reported identically;
	// distinguishing them is an account-enumeration oracle.
	_, errUnknown := svc.Login(ctx, auth.LoginInput{Email: "nobody@example.com", Password: password})
	_, errWrong := svc.Login(ctx, auth.LoginInput{Email: email, Password: "wrong-but-long-enough"})

	if !errors.Is(errUnknown, auth.ErrInvalidCredentials) {
		t.Errorf("unknown account = %v, want ErrInvalidCredentials", errUnknown)
	}
	if !errors.Is(errWrong, auth.ErrInvalidCredentials) {
		t.Errorf("wrong password = %v, want ErrInvalidCredentials", errWrong)
	}
	if errUnknown.Error() != errWrong.Error() {
		t.Errorf("the two failures produce different messages (%q vs %q), which reveals "+
			"whether an address is registered", errUnknown, errWrong)
	}
}

func TestAccountLocksAfterRepeatedFailures(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	const email, password = "lock@example.com", "a-sufficiently-long-password"
	if _, err := svc.Register(ctx, auth.RegisterInput{Email: email, Password: password}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		_, _ = svc.Login(ctx, auth.LoginInput{Email: email, Password: "wrong-but-long-enough"})
	}

	// Locked out even with the correct password.
	_, err := svc.Login(ctx, auth.LoginInput{Email: email, Password: password})
	if !errors.Is(err, auth.ErrAccountLocked) {
		t.Errorf("after 5 failures = %v, want ErrAccountLocked", err)
	}

	// The lock must be time-bounded, not permanent: otherwise anyone can lock
	// a victim out on demand by failing five times.
	var lockedUntil *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT locked_until FROM users WHERE email_lower = $1", email).Scan(&lockedUntil); err != nil {
		t.Fatal(err)
	}
	if lockedUntil == nil {
		t.Fatal("locked_until not set")
	}
	if d := time.Until(*lockedUntil); d > 20*time.Minute {
		t.Errorf("lockout lasts %s, which is long enough to be a denial of service", d)
	}
}

// ─── Finding F92: a lockout must not be a registration oracle ────────────────

// signInRefusal is everything a caller can see of a refused sign-in.
type signInRefusal struct {
	Status int    `json:"-"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// signInAPI posts one credential pair to the JSON endpoint and returns the
// refusal whole, so a difference in any visible field fails rather than only the
// field somebody remembered to compare.
func (f *webFixture) signInAPI(email, password string) signInRefusal {
	f.t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		f.t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost,
		f.server.URL+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := signInRefusal{Status: resp.StatusCode}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		f.t.Fatalf("decode problem document: %v", err)
	}
	return out
}

// TestSignInAnswersALockedAccountLikeAnUnregisteredOne is finding F92.
//
// Sign-in used to tell an unauthenticated stranger whether an address had an
// account here, and the price of asking was five wrong passwords. The fifth
// against a registered address came back `429 …/problems/account-locked`; five
// against an unregistered one came back `401 …/problems/invalid-credentials`.
// Five fits inside LOGIN_RATE_PER_MIN, so the per-address limiter never masked
// it, and the limiter's own refusal carries a third type — so all three states
// were tellable apart with no credential at all, on the shipped `closed` default
// where the registration endpoint refuses before it ever looks an address up.
// Asking also cost the target a lockout, which made it a cheap denial of service
// against a known address as well as an oracle.
//
// Both surfaces are asserted here because fixing one is the trap the row named:
// mapping the error to a 401 closes the API and leaves the form saying *the
// account is locked briefly* in prose, and whoever wants to know only has to ask
// the surface that still answers.
func TestSignInAnswersALockedAccountLikeAnUnregisteredOne(t *testing.T) {
	f := newWeb(t)
	f.claim()
	// Signed out first: claim() leaves a session in the jar, and a sign-in
	// attempt carrying one is not the state a stranger is in.
	f.body(f.postForm("/logout", url.Values{}, nil))

	const known, unknown = "owner@example.com", "nobody@example.com"
	const wrong = "wrong-but-long-enough"

	// The threshold, attempt by attempt, so the comparison covers the fifth —
	// which is the one that used to change its answer — and not only the total.
	for attempt := 1; attempt <= 5; attempt++ {
		gotKnown := f.signInAPI(known, wrong)
		gotUnknown := f.signInAPI(unknown, wrong)
		if gotKnown != gotUnknown {
			t.Errorf("attempt %d: a registered address answers %+v where an "+
				"unregistered one answers %+v — the difference is the answer to "+
				"\"does this address have an account here?\"",
				attempt, gotKnown, gotUnknown)
		}
		if gotKnown.Status != http.StatusUnauthorized {
			t.Errorf("attempt %d returned %d, want 401: a sign-in refusal is a 401 "+
				"whatever caused it, and a 429 here is both the oracle and a "+
				"client being told to back off for somebody else's guessing",
				attempt, gotKnown.Status)
		}
	}

	// Now locked. The strongest case: the *correct* password against an account
	// that is locked out must be indistinguishable from the correct password
	// against nothing at all.
	if got, want := f.signInAPI(known, webPassword), f.signInAPI(unknown, webPassword); got != want {
		t.Errorf("with the right password, a locked account answers %+v and an "+
			"unregistered address answers %+v", got, want)
	}

	// And the form, which is where the prose lives.
	page := func(email string) string {
		resp := f.postForm("/login", url.Values{"email": {email}, "password": {wrong}}, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			f.t.Fatalf("the sign-in form returned %d for %s, want 401", resp.StatusCode, email)
		}
		body := f.body(resp)
		// The only thing that legitimately differs is the address echoed back
		// into the form's value attribute, so it is normalized away rather than
		// excluded from the comparison — anything else that differs is a leak.
		return strings.ReplaceAll(body, email, "<the address that was submitted>")
	}
	if lockedPage, unknownPage := page(known), page(unknown); lockedPage != unknownPage {
		t.Errorf("the sign-in form renders a different page for a locked account "+
			"than for an unregistered address; the API answering identically is "+
			"worth nothing while the prose still says which is which\n"+
			"locked page length %d, unknown page length %d",
			len(lockedPage), len(unknownPage))
	}
}

// TestALockedAccountCostsTheSameAsAnUnregisteredOne is F92's second leak, and
// the reason equalising the status alone would not have closed it.
//
// The locked branch returned before any verification, so it answered in one
// database round trip where the unknown-address path pays a dummy argon2 — the
// same question, asked with a stopwatch instead of a status code.
//
// Shaped like TestDummyVerifyCostsTheSameAsARealOne and bounded as generously,
// for its reason: this is a timing test on a shared machine, and what it has to
// catch is an order-of-magnitude gap rather than a few percent.
func TestALockedAccountCostsTheSameAsAnUnregisteredOne(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	const email, password = "timing@example.com", "a-sufficiently-long-password"
	if _, err := svc.Register(ctx, auth.RegisterInput{Email: email, Password: password}); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		_, _ = svc.Login(ctx, auth.LoginInput{Email: email, Password: "wrong-but-long-enough"})
	}
	if _, err := svc.Login(ctx, auth.LoginInput{Email: email, Password: password}); !errors.Is(err, auth.ErrAccountLocked) {
		t.Fatalf("the account under test is not locked: %v", err)
	}

	// Warm up, so the first allocation does not skew the comparison.
	_, _ = svc.Login(ctx, auth.LoginInput{Email: "nobody@example.com", Password: password})
	_, _ = svc.Login(ctx, auth.LoginInput{Email: email, Password: password})

	const runs = 5
	var locked, unknown time.Duration
	for range runs {
		start := time.Now()
		_, _ = svc.Login(ctx, auth.LoginInput{Email: "nobody@example.com", Password: password})
		unknown += time.Since(start)

		start = time.Now()
		_, _ = svc.Login(ctx, auth.LoginInput{Email: email, Password: password})
		locked += time.Since(start)
	}
	locked /= runs
	unknown /= runs

	if ratio := float64(locked) / float64(unknown); ratio < 0.25 || ratio > 4.0 {
		t.Errorf("a locked account refuses in %s where an unregistered address "+
			"takes %s (ratio %.2f); the gap is the same account-enumeration oracle "+
			"the status code used to be, answerable with a stopwatch",
			locked, unknown, ratio)
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	const email, password = "change@example.com", "a-sufficiently-long-password"
	if _, err := svc.Register(ctx, auth.RegisterInput{Email: email, Password: password}); err != nil {
		t.Fatal(err)
	}

	// Two sessions: a "current browser" and another device.
	current, err := svc.Login(ctx, auth.LoginInput{Email: email, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.Login(ctx, auth.LoginInput{Email: email, Password: password})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.ChangePassword(ctx, current.Identity.UserID, current.Identity.SessionID,
		password, "a-brand-new-long-password"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// The other session must be gone; whoever knew the old password loses access.
	if _, err := svc.Authenticate(ctx, other.Token); err == nil {
		t.Error("a sibling session survived a password change")
	}
	// The current one survives, so the user is not logged out of the browser
	// they just used.
	if _, err := svc.Authenticate(ctx, current.Token); err != nil {
		t.Errorf("the current session was revoked by its own password change: %v", err)
	}

	// The old password no longer works, the new one does.
	if _, err := svc.Login(ctx, auth.LoginInput{Email: email, Password: password}); err == nil {
		t.Error("the old password still works")
	}
	if _, err := svc.Login(ctx, auth.LoginInput{Email: email, Password: "a-brand-new-long-password"}); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

// TestRBACMatrix is the M5 gate: every seeded role against every permission.
func TestRBACMatrix(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	// Register an owner, then re-point their membership at each role in turn.
	id, err := svc.Register(ctx, auth.RegisterInput{
		Email: "matrix@example.com", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]map[string]bool{
		"owner":  {"links.read": true, "links.create": true, "links.delete": true, "apikeys.write": true, "members.write": true, "org.delete": true},
		"admin":  {"links.read": true, "links.create": true, "links.delete": true, "apikeys.write": true, "members.write": true, "org.delete": false},
		"editor": {"links.read": true, "links.create": true, "links.delete": true, "apikeys.write": false, "members.write": false, "org.delete": false},
		"viewer": {"links.read": true, "links.create": false, "links.delete": false, "apikeys.write": false, "members.write": false, "org.delete": false},
	}

	for role, expectations := range want {
		t.Run(role, func(t *testing.T) {
			if _, err := pool.Exec(ctx,
				`UPDATE memberships SET role_id = (SELECT id FROM roles WHERE slug = $1 AND organization_id IS NULL)
				 WHERE user_id = $2`, role, id.UserID); err != nil {
				t.Fatal(err)
			}

			res, err := svc.Login(ctx, auth.LoginInput{
				Email: "matrix@example.com", Password: "a-sufficiently-long-password",
			})
			if err != nil {
				t.Fatal(err)
			}

			for perm, allowed := range expectations {
				if got := res.Identity.Can(perm); got != allowed {
					t.Errorf("%s.Can(%q) = %v, want %v", role, perm, got, allowed)
				}
			}
		})
	}
}

// TestEditorCannotMintAPIKeys is called out separately because it is a
// privilege-escalation path, not just a permission row: an editor who can
// create API keys can grant themselves scopes beyond their own role.
func TestEditorCannotMintAPIKeys(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	id, err := svc.Register(ctx, auth.RegisterInput{
		Email: "editor@example.com", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE memberships SET role_id = (SELECT id FROM roles WHERE slug='editor' AND organization_id IS NULL)
		 WHERE user_id = $1`, id.UserID); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Login(ctx, auth.LoginInput{
		Email: "editor@example.com", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Identity.Can("apikeys.write") {
		t.Error("an editor can mint API keys, which lets them grant themselves scopes " +
			"beyond their own role")
	}
}

func TestCrossWorkspaceAccessIsDenied(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	alice, err := svc.Register(ctx, auth.RegisterInput{
		Email: "alice@example.com", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := svc.Register(ctx, auth.RegisterInput{
		Email: "bob@example.com", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatal(err)
	}

	if alice.WorkspaceID == bob.WorkspaceID {
		t.Fatal("two users share a workspace; personal workspaces must be distinct")
	}

	// Alice holds no permissions in Bob's workspace.
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT p.slug)
		FROM memberships m
		JOIN workspaces w ON w.organization_id = m.organization_id
		JOIN role_permissions rp ON rp.role_id = m.role_id
		JOIN permissions p ON p.id = rp.permission_id
		WHERE m.user_id = $1 AND w.id = $2`, alice.UserID, bob.WorkspaceID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("alice holds %d permissions in bob's workspace, want 0", count)
	}
}

func TestConcurrentRegistrationsOfTheSameEmail(t *testing.T) {
	pool := newDB(t)
	svc := newService(pool)
	ctx := context.Background()

	const workers = 8
	var wg sync.WaitGroup
	results := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = svc.Register(ctx, auth.RegisterInput{
				Email: "race@example.com", Password: "a-sufficiently-long-password",
			})
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("%d of %d concurrent registrations succeeded, want exactly 1", succeeded, workers)
	}

	var users int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users)
	if users != 1 {
		t.Errorf("%d user rows exist, want 1", users)
	}
}

func TestPartitionsExistAndAreUTCAligned(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()

	if _, err := store.EnsurePartitions(ctx, pool, 2); err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		FROM pg_class c
		JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'click_events' AND c.relname <> 'click_events_default'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var name, bounds string
		if err := rows.Scan(&name, &bounds); err != nil {
			t.Fatal(err)
		}
		n++
		// Bounds are resolved against the session timezone at DDL time. Any
		// offset other than +00 means a gap or overlap between months.
		if !strings.Contains(bounds, "00:00:00+00") {
			t.Errorf("%s has bounds %q, which are not UTC-midnight aligned", name, bounds)
		}
	}
	if n < 3 {
		t.Errorf("got %d month partitions, want at least 3 (current plus two ahead)", n)
	}
}
