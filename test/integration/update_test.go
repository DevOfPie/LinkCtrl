//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
	"github.com/DevOfPie/LinkCtrl/internal/update"
)

// The update check (M55), end to end against a real database and a stub for the
// one thing that is not this instance's.
//
// Three parts, and they are separate on purpose. The *pass* — who is asked, how
// often, and who is told — runs on the scheduler where there is no request, so a
// test that drove it through a browser would prove nothing about it. The
// *first-run prompt* is the surface D149 obliged the milestone to build, on the
// form that claims a fresh instance. The *upgrade prompt* is D164's: the same
// question, put to an administrator on the dashboard, on an instance that had no
// first run left to be asked at.

// releaseStub is GitHub, as this product sees it: one endpoint, a counter, and a
// body the test chooses.
type releaseStub struct {
	server *httptest.Server
	calls  atomic.Int64
	// agents records the User-Agent of every request, so a test can assert what
	// left the machine rather than only how often.
	agents chan string
}

func newReleaseStub(t *testing.T, body func() string) *releaseStub {
	t.Helper()
	s := &releaseStub{agents: make(chan string, 16)}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		select {
		case s.agents <- r.UserAgent():
		default:
		}
		_, _ = io.WriteString(w, body())
	}))
	t.Cleanup(s.server.Close)
	return s
}

func release(tag string) func() string {
	return func() string { return `{"tag_name":"` + tag + `"}` }
}

// newUpdateService wires the pass the way main.go wires it, except for the
// endpoint.
func newUpdateService(
	t *testing.T, pool *pgxpool.Pool, version, endpoint string, announce update.Announcer,
) *update.Service {
	t.Helper()
	return update.NewService(pool, update.Config{
		Version:  version,
		Endpoint: endpoint,
		Announce: announce,
		Log:      slog.New(slog.DiscardHandler),
	})
}

// principalOn puts an account on the instance holding instance.admin, which is
// the only recipient a release announcement has.
func principalOn(t *testing.T, pool *pgxpool.Pool, email string) *auth.Identity {
	t.Helper()
	id, err := newService(pool).Register(context.Background(), auth.RegisterInput{
		Email: email, Name: "Operator", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	grantInstanceScope(t, pool, id.UserID, auth.PermInstanceAdmin)
	return id
}

// updateCheckSetting reads the column with all three of its states intact
// (D164): nil is *nobody has been asked*, which is neither on nor off and is
// what an instance upgrading into 0.3.0 arrives in.
//
// A `bool` here would be the bug this milestone was rejected for, written into
// the test as well: it cannot tell an unanswered instance from one whose
// operator said no, so every assertion below would pass for the wrong reason.
func updateCheckSetting(t *testing.T, pool *pgxpool.Pool) *bool {
	t.Helper()
	var enabled *bool
	if err := pool.QueryRow(t.Context(),
		`SELECT update_check_enabled FROM instance_settings WHERE id`).Scan(&enabled); err != nil {
		t.Fatalf("read instance_settings: %v", err)
	}
	return enabled
}

func wantSetting(t *testing.T, pool *pgxpool.Pool, want *bool) {
	t.Helper()
	got := updateCheckSetting(t, pool)
	show := func(v *bool) string {
		if v == nil {
			return "unanswered"
		}
		if *v {
			return "on"
		}
		return "off"
	}
	if (got == nil) != (want == nil) || (got != nil && *got != *want) {
		t.Errorf("update_check_enabled is %s, want %s", show(got), show(want))
	}
}

func answered(v bool) *bool { return &v }

// answerTheQuestion is the operator saying yes or no.
//
// Written straight to the row, because every test that calls it is about the
// *pass* — who is asked, how often, who is told — and driving the surface first
// would make each of them depend on a prompt they are not testing. The surface
// that records the answer has its own tests further down, and they assert this
// column rather than trusting it.
func answerTheQuestion(t *testing.T, pool *pgxpool.Pool, enabled bool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE instance_settings SET update_check_enabled = $1 WHERE id`, enabled); err != nil {
		t.Fatalf("answer the update-check question: %v", err)
	}
}

func countUpdateNotifications(t *testing.T, pool *pgxpool.Pool, version string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM notifications WHERE kind = $1 AND data->>'version' = $2`,
		notify.KindUpdateAvailable, version).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// backdateTheCheck moves the recorded check into the past, which is the only way
// to make a second pass eligible without waiting a day.
func backdateTheCheck(t *testing.T, pool *pgxpool.Pool, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE instance_settings SET update_checked_at = now() - $1::interval WHERE id`,
		d.String()); err != nil {
		t.Fatalf("backdate the check: %v", err)
	}
}

// TestTheMigrationAnswersNothingForAnInstanceThatAlreadyExists.
//
// **D164, asserted rather than described.** An instance upgrading into 0.3.0 has
// no first run left to be asked at, so the migration leaves the question open
// and the check off until somebody answers it. D149 bought *the operator decides
// knowingly*, not *on*, and a `NOT NULL DEFAULT true` here would be this file
// answering on their behalf for the whole population the first-run prompt cannot
// reach.
//
// The second half is the important one: unanswered is not merely a distinct
// value, it is a value the pass declines to act on.
func TestTheMigrationAnswersNothingForAnInstanceThatAlreadyExists(t *testing.T) {
	pool := newDB(t)

	if got := updateCheckSetting(t, pool); got != nil {
		t.Errorf("a migrated instance answers %v to the update-check question. "+
			"D164 is that it answers nothing: the column has three states and an "+
			"upgraded instance is in the third one until an administrator signs in "+
			"and is asked. A default here decides for every instance that already "+
			"exists, which is the larger half of them.", *got)
	}

	// One row, and the primary key is what guarantees it. A settings table with
	// two rows has no settings.
	var rows int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM instance_settings`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("instance_settings holds %d rows, want exactly 1", rows)
	}

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO instance_settings (id) VALUES (true)`); err == nil {
		t.Error("a second settings row was accepted. The singleton is the primary " +
			"key's job, and without it 'the settings' means whichever row a query " +
			"happened to read first.")
	}
}

// TestAnInstanceNobodyHasAnsweredForAsksNothing.
//
// The behavioural half of D164, and the one that would still be missing if the
// column were merely made nullable. *Off while unanswered* is enforced in
// ClaimUpdateCheck, in the same statement as the daily bound, so there is no
// window in which a pass reads "not answered" and asks anyway.
//
// It also asserts the row is not marked as checked, for the reason the declined
// case does: an instance that recorded a check it never made would sit out the
// first day after somebody finally answers.
func TestAnInstanceNobodyHasAnsweredForAsksNothing(t *testing.T) {
	pool := newDB(t)
	stub := newReleaseStub(t, release("v9.9.9"))
	svc := newUpdateService(t, pool, "0.3.0", stub.server.URL, nil)

	for range 3 {
		if err := svc.Run(t.Context()); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if got := stub.calls.Load(); got != 0 {
		t.Errorf("an instance whose operator has not been asked made %d requests, "+
			"want none. The upgrade is the case D164 is about: the check is off "+
			"until somebody answers, and an instance nobody signs into stays quiet "+
			"forever — which docs/deployment.md states rather than leaves to be "+
			"found.", got)
	}

	var checkedAt *time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT update_checked_at FROM instance_settings WHERE id`).Scan(&checkedAt); err != nil {
		t.Fatal(err)
	}
	if checkedAt != nil {
		t.Errorf("an unanswered instance recorded a check at %s, so the day after "+
			"somebody answers would be sat out", checkedAt)
	}

	// And the answer is what unblocks it, or the assertion above would pass on an
	// instance where the check is simply broken.
	answerTheQuestion(t, pool, true)
	if err := svc.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Errorf("after the operator said yes the instance made %d requests in "+
			"total, want 1", got)
	}
}

// TestTheCheckAsksOnceADayAndNotOncePerTick.
//
// The scheduler ticks hourly (a job whose ticker *is* its period never runs on
// an instance redeployed most days), so the daily bound has to be the row. This
// drives the pass three times in a row and asserts one request — and then
// backdates the row and asserts a second, so the test cannot pass by the check
// simply being broken.
func TestTheCheckAsksOnceADayAndNotOncePerTick(t *testing.T) {
	pool := newDB(t)
	answerTheQuestion(t, pool, true)
	stub := newReleaseStub(t, release("v0.0.1"))
	svc := newUpdateService(t, pool, "0.3.0", stub.server.URL, nil)

	for range 3 {
		if err := svc.Run(t.Context()); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("three passes made %d requests, want 1. The bound is the row "+
			"ClaimUpdateCheck updates, not the ticker; without it a replica "+
			"restarted every ten minutes asks GitHub every ten minutes.", got)
	}

	backdateTheCheck(t, pool, 25*time.Hour)
	if err := svc.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stub.calls.Load(); got != 2 {
		t.Fatalf("after a day had passed the check made %d requests in total, want 2", got)
	}
}

// TestAFailedCheckIsNotRetriedUntilTheNextDay.
//
// m55.md forbids a retry storm, and the mechanism is that the timestamp is
// written *before* the request: a failure consumes the day exactly as a success
// does. A stub that answers 500 every time would otherwise be asked on every
// tick forever.
func TestAFailedCheckIsNotRetriedUntilTheNextDay(t *testing.T) {
	pool := newDB(t)
	answerTheQuestion(t, pool, true)

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	svc := newUpdateService(t, pool, "0.3.0", srv.URL, nil)
	for range 5 {
		if err := svc.Run(t.Context()); err != nil {
			t.Fatalf("a failed check returned an error to the scheduler: %v.\n"+
				"A check that cannot complete is never fatal and never surfaces; "+
				"returning an error here would log it at Error on every tick.", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("five passes against a failing endpoint made %d requests, want 1", got)
	}
}

// TestAnOperatorWhoDeclinedIsNeverAsked.
//
// The first-run answer is a row, and it gates the claim rather than the
// construction — so turning it off after boot takes effect on the next pass
// without a restart. That is the half of D160 the environment variable cannot
// cover.
func TestAnOperatorWhoDeclinedIsNeverAsked(t *testing.T) {
	pool := newDB(t)
	stub := newReleaseStub(t, release("v9.9.9"))
	svc := newUpdateService(t, pool, "0.3.0", stub.server.URL, nil)

	answerTheQuestion(t, pool, false)

	for range 3 {
		if err := svc.Run(t.Context()); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if got := stub.calls.Load(); got != 0 {
		t.Errorf("an instance whose operator declined the check made %d requests, "+
			"want none. The answer they gave at first run is the whole of what "+
			"makes D149's default defensible.", got)
	}

	// And the row is not silently marked as checked either, or turning it back on
	// would leave the instance thinking it had already asked today.
	var checkedAt *time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT update_checked_at FROM instance_settings WHERE id`).Scan(&checkedAt); err != nil {
		t.Fatal(err)
	}
	if checkedAt != nil {
		t.Errorf("a declined instance recorded a check at %s; the claim and the "+
			"enabled test are one statement precisely so this cannot happen", checkedAt)
	}
}

// TestANewerReleaseTellsThePrincipalExactlyOnce.
//
// The milestone's *it is not repeated for a version already notified. Asserted
// by test.* The second pass is a real second pass — the row is backdated, so the
// daily bound is not what suppresses it — which is the only way to tell the
// version guard from the day guard.
func TestANewerReleaseTellsThePrincipalExactlyOnce(t *testing.T) {
	pool := newDB(t)
	answerTheQuestion(t, pool, true)
	principalOn(t, pool, "operator@example.com")

	stub := newReleaseStub(t, release("v0.4.0"))
	svc := newUpdateService(t, pool, "0.3.0", stub.server.URL, notify.NewService(pool))

	if err := svc.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := countUpdateNotifications(t, pool, "0.4.0"); n != 1 {
		t.Fatalf("after one pass the principal has %d notifications about 0.4.0, want 1", n)
	}

	backdateTheCheck(t, pool, 25*time.Hour)
	if err := svc.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stub.calls.Load(); got != 2 {
		t.Fatalf("the second pass did not reach the endpoint (%d calls), so the "+
			"assertion below would pass for the wrong reason", got)
	}
	if n := countUpdateNotifications(t, pool, "0.4.0"); n != 1 {
		t.Errorf("a second pass finding the same release produced %d notifications "+
			"about 0.4.0, want 1. An inbox that repeats the same line daily is one "+
			"nobody opens, which costs the warning the whole feature is for.", n)
	}

	// A different version is a new fact and does arrive.
	newer := newReleaseStub(t, release("v0.5.0"))
	backdateTheCheck(t, pool, 25*time.Hour)
	if err := newUpdateService(t, pool, "0.3.0", newer.server.URL,
		notify.NewService(pool)).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := countUpdateNotifications(t, pool, "0.5.0"); n != 1 {
		t.Errorf("a later release produced %d notifications, want 1: the guard is "+
			"keyed on the version, not on having ever said anything", n)
	}
}

// TestNobodyButThePrincipalIsTold.
//
// Whether the box is up to date is an operator's question, and a workspace
// member can act on none of it. The second account here holds a membership and
// no instance grant, which is what every ordinary user of an instance looks
// like.
func TestNobodyButThePrincipalIsTold(t *testing.T) {
	pool := newDB(t)
	answerTheQuestion(t, pool, true)
	principal := principalOn(t, pool, "operator@example.com")

	member, err := newService(pool).Register(context.Background(), auth.RegisterInput{
		Email: "member@example.com", Name: "Member", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatal(err)
	}

	stub := newReleaseStub(t, release("v0.4.0"))
	if err := newUpdateService(t, pool, "0.3.0", stub.server.URL,
		notify.NewService(pool)).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, tc := range []struct {
		who  string
		id   *auth.Identity
		want int
	}{
		{who: "the instance principal", id: principal, want: 1},
		{who: "an ordinary account", id: member, want: 0},
	} {
		var n int
		if err := pool.QueryRow(t.Context(),
			`SELECT count(*) FROM notifications WHERE user_id = $1 AND kind = $2`,
			tc.id.UserID, notify.KindUpdateAvailable).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != tc.want {
			t.Errorf("%s has %d release notifications, want %d", tc.who, n, tc.want)
		}
	}
}

// TestADevelopmentBuildTellsNobodyToUpgrade.
//
// The whole path, not just the comparison: a `dev` binary asks — it is a
// perfectly ordinary instance and the request is the same request — and then
// says nothing, because there is no version to be behind. Unit tests cover
// IsNewer; this covers that nothing downstream of it writes a row anyway.
func TestADevelopmentBuildTellsNobodyToUpgrade(t *testing.T) {
	pool := newDB(t)
	answerTheQuestion(t, pool, true)
	principalOn(t, pool, "operator@example.com")

	stub := newReleaseStub(t, release("v9.9.9"))
	if err := newUpdateService(t, pool, "dev", stub.server.URL,
		notify.NewService(pool)).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("a dev build made %d requests, want 1: the check is not skipped, "+
			"the comparison is", got)
	}
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM notifications WHERE kind = $1`,
		notify.KindUpdateAvailable).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a dev build produced %d release notifications, want none. A "+
			"development binary telling its operator to upgrade is noise.", n)
	}
}

// TestTheRequestFromARealInstanceCarriesTheRunningVersion.
//
// internal/update's unit test holds the exact form of the request. This is the
// same claim one layer out: the version the *service* was built with is the one
// that reaches the wire, so a wiring change that passed the wrong string would
// be caught here rather than by reading main.go.
func TestTheRequestFromARealInstanceCarriesTheRunningVersion(t *testing.T) {
	pool := newDB(t)
	answerTheQuestion(t, pool, true)
	stub := newReleaseStub(t, release("v0.0.1"))

	if err := newUpdateService(t, pool, "0.3.0", stub.server.URL, nil).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case agent := <-stub.agents:
		if agent != "LinkCtrl/0.3.0" {
			t.Errorf("User-Agent = %q, want %q", agent, "LinkCtrl/0.3.0")
		}
	default:
		t.Fatal("the endpoint recorded no request")
	}
}

// --- the two prompts --------------------------------------------------------

// setupFixture is the dashboard, wired for the two pages that carry the
// question: the setup form a fresh instance answers on, and the dashboard where
// an upgraded one is asked.
type setupFixture struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	pool   *pgxpool.Pool
	auth   *auth.Service
}

func newSetup(t *testing.T, updateCheck bool) *setupFixture {
	t.Helper()
	pool := newDB(t)

	cfg := config.Config{
		AppEnv:        config.Development,
		BaseURL:       "http://links.test",
		SecureCookies: false,
		UpdateCheck:   updateCheck,
	}
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
	})
	instanceSvc := instance.NewService(pool, instance.Config{Audit: audit.NewService(pool)})
	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	// The dashboard needs both to render at all, and the prompt is drawn on it.
	// A fixture without them would answer 500 to the page the upgrade prompt
	// lives on, and every assertion about that prompt would be about an error
	// page instead.
	linkSvc := link.NewService(pool, link.Config{
		Policy:  link.DefaultDestinationPolicy(),
		BaseURL: cfg.BaseURL,
	})
	stats := analytics.NewReader(pool)

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config:   cfg,
		Health:   &httpx.Health{DB: pool},
		Auth:     authSvc,
		Links:    linkSvc,
		Stats:    stats,
		Notify:   notify.NewService(pool),
		Instance: instanceSvc,
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc,
			Links: linkSvc, Stats: stats,
			Notify: notify.NewService(pool), Instance: instanceSvc,
		},
	}))
	t.Cleanup(srv.Close)

	jar, _ := newCookieJar()
	return &setupFixture{t: t, server: srv, pool: pool, auth: authSvc, client: &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// signIn puts an account on the instance and signs the fixture's browser in as
// it. `admin` decides whether it holds instance.admin, which is the whole of
// what separates the two readers the prompt distinguishes.
func (f *setupFixture) signIn(email string, admin bool) {
	f.t.Helper()
	id, err := f.auth.Register(f.t.Context(), auth.RegisterInput{
		Email: email, Name: "Operator", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		f.t.Fatalf("register %s: %v", email, err)
	}
	if admin {
		grantInstanceScope(f.t, f.pool, id.UserID, auth.PermInstanceAdmin)
	}

	form := url.Values{
		"email":    {email},
		"password": {"a-sufficiently-long-password"},
	}
	resp := f.post("/login", form)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("POST /login = %d, want 303", resp.StatusCode)
	}
}

// post submits a form, as a browser on this origin would.
func (f *setupFixture) post(path string, form url.Values) *http.Response {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost,
		f.server.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", f.server.URL)
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

// dashboard is the page an administrator lands on when they sign in.
func (f *setupFixture) dashboard() string {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet,
		f.server.URL+"/dashboard", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("GET /dashboard = %d, want 200", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *setupFixture) page() string {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+"/setup", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("GET /setup = %d, want 200", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// claim posts the setup form. `answer` nil leaves the checkbox off the request
// entirely, which is what an unticked box sends.
func (f *setupFixture) claim(answer *bool) *http.Response {
	f.t.Helper()
	form := url.Values{
		"name":     {"Operator"},
		"email":    {"operator@example.com"},
		"password": {"a-sufficiently-long-password"},
	}
	if answer != nil && *answer {
		form.Set("update_check", "1")
	}
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost,
		f.server.URL+"/setup", strings.NewReader(form.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", f.server.URL)
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestTheSetupFormAsksAboutTheUpdateCheck.
//
// D149's surface, and the reason the milestone is more than a daily GET: the
// setup form claimed the instance and did not configure it. The control is
// ticked, because *on by default and asks* is the answer the owner gave — a box
// somebody has to find and tick would be the recommendation that was overruled,
// dressed as a form.
func TestTheSetupFormAsksAboutTheUpdateCheck(t *testing.T) {
	f := newSetup(t, true)
	body := f.page()

	if !strings.Contains(body, `name="update_check"`) {
		t.Fatal("the setup page has no update-check control. D149 is that the " +
			"operator is asked at first run, and this page is the only place the " +
			"question can be put to them.")
	}
	if !strings.Contains(body, "checked") {
		t.Error("the control is not ticked. On by default is the decision; an " +
			"unticked box is off by default with extra steps.")
	}
	// The enumeration, on the page rather than only in the documentation. The
	// operator being asked knowingly is the whole of what D149 bought.
	for _, phrase := range []string{"User-Agent", "Nothing else", "LINKCTRL_UPDATE_CHECK"} {
		if !strings.Contains(body, phrase) {
			t.Errorf("the prompt does not mention %q. The operator is being asked to "+
				"agree to an outbound connection; what it carries has to be beside "+
				"the control, not one document away.", phrase)
		}
	}
}

// TestTheSetupFormSaysSoWhenTheDeploymentAlreadyDeclined.
//
// A control that would be ignored is worse than no control: an air-gapped
// instance must not appear to be asking a question its configuration has already
// answered.
func TestTheSetupFormSaysSoWhenTheDeploymentAlreadyDeclined(t *testing.T) {
	f := newSetup(t, false)
	body := f.page()

	if strings.Contains(body, `name="update_check"`) {
		t.Error("the setup page offers an update-check control on a deployment " +
			"with LINKCTRL_UPDATE_CHECK=false, where ticking it would do nothing")
	}
	if !strings.Contains(body, "LINKCTRL_UPDATE_CHECK=false") {
		t.Error("the page says nothing about why the question is absent, which " +
			"reads as the feature not existing rather than as it being off")
	}
}

// TestTheAnswerIsRecordedAndTheInstanceIsClaimed covers both answers, and the
// ordering that makes a *no* survivable.
func TestTheAnswerIsRecordedAndTheInstanceIsClaimed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ticked bool
	}{
		{name: "ticked", ticked: true},
		{name: "unticked", ticked: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetup(t, true)
			resp := f.claim(&tc.ticked)
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("POST /setup = %d, want 303", resp.StatusCode)
			}
			wantSetting(t, f.pool, answered(tc.ticked))

			// The instance really was claimed, or the assertion above would pass
			// for a form that rejected everything.
			var users int
			if err := f.pool.QueryRow(t.Context(),
				`SELECT count(*) FROM users`).Scan(&users); err != nil {
				t.Fatal(err)
			}
			if users != 1 {
				t.Errorf("the instance has %d accounts after setup, want 1", users)
			}
		})
	}
}

// TestARejectedSetupKeepsTheAnswerAndDoesNotClaimTheInstance.
//
// The ordering D161 records: the answer is written before the account, so a
// `Register` that fails afterwards leaves the answer standing rather than
// claiming the instance with the operator's *no* lost. A short password is the
// cheapest way to make the second half fail.
func TestARejectedSetupKeepsTheAnswerAndDoesNotClaimTheInstance(t *testing.T) {
	f := newSetup(t, true)

	form := url.Values{
		"name":     {"Operator"},
		"email":    {"operator@example.com"},
		"password": {"short"},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		f.server.URL+"/setup", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", f.server.URL)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /setup with a short password = %d, want 422", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `name="update_check"`) {
		t.Error("the re-rendered form dropped the update-check control, so the " +
			"operator's answer is silently reset by a typo in their password")
	}

	var users int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Errorf("a refused setup created %d accounts", users)
	}
}

// TestTheApiSetupCarriesTheSameQuestion.
//
// *Every UI feature has API support.* **Omitted answers nothing** (D164), which
// is the field's whole reason for being a pointer: a client written before it
// existed claims an instance exactly as it always did, and cannot consent to an
// outbound connection on its operator's behalf by saying nothing. Such an
// instance is asked at the dashboard prompt instead, like an upgraded one.
func TestTheApiSetupCarriesTheSameQuestion(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want *bool
	}{
		{name: "omitted answers nothing", body: `{"email":"a@example.com","password":"a-sufficiently-long-password"}`, want: nil},
		{name: "false is recorded", body: `{"email":"a@example.com","password":"a-sufficiently-long-password","update_check":false}`, want: answered(false)},
		{name: "true is recorded", body: `{"email":"a@example.com","password":"a-sufficiently-long-password","update_check":true}`, want: answered(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetup(t, true)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				f.server.URL+"/api/v1/auth/setup", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := f.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusCreated {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("POST /api/v1/auth/setup = %d: %s", resp.StatusCode, b)
			}
			var out map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if out["user_id"] == nil {
				t.Fatalf("the response named no account: %v", out)
			}
			wantSetting(t, f.pool, tc.want)
		})
	}
}

// TestTheSettingCannotBeChangedOnceTheInstanceIsClaimed.
//
// The guard is in the service rather than in its callers, because two HTTP
// handlers reach it: an unchecked version would be one route registration away
// from a public endpoint for changing what an instance connects to.
func TestTheSettingCannotBeChangedOnceTheInstanceIsClaimed(t *testing.T) {
	pool := newDB(t)
	svc := instance.NewService(pool, instance.Config{Audit: audit.NewService(pool)})

	if err := svc.SetUpdateCheckAtSetup(t.Context(), false); err != nil {
		t.Fatalf("on an unclaimed instance the write must succeed: %v", err)
	}
	wantSetting(t, pool, answered(false))

	principalOn(t, pool, "operator@example.com")

	if err := svc.SetUpdateCheckAtSetup(t.Context(), true); !errors.Is(err, instance.ErrClaimed) {
		t.Errorf("err = %v, want ErrClaimed. Once an account exists the first-run "+
			"window has closed, and the answer is changed by the deployment's "+
			"environment rather than by an unauthenticated call.", err)
	}
	wantSetting(t, pool, answered(false))
}

// TestTheNewWriteDoesNotChangeWhatAClaimedInstanceAnswers.
//
// Adding a write to both setup handlers is a chance to break the answer a
// claimed instance already gives — by putting it before the `NeedsSetup` check,
// or by letting it fail in a way that escapes as a 500. So this asserts the old
// behaviour still holds with the new write in place: the form still redirects to
// sign-in and the API still answers 404.
//
// **Two mechanisms deliver that answer, and this test is satisfied by either.**
// The `NeedsSetup` pre-check answers first; behind it, `SetUpdateCheckAtSetup`
// refuses a claimed instance and the handlers map that refusal to the same
// answer. Sabotaging either one alone leaves this green, which is defence in
// depth working rather than the test being weak — sabotaging both produces the
// 500 the second mechanism exists to prevent, and that is what was checked
// rather than assumed.
//
// The refusal itself is provoked directly, at the service, by
// TestTheSettingCannotBeChangedOnceTheInstanceIsClaimed. What cannot be provoked
// from here is the ordering that makes the second mechanism necessary in
// production: `NeedsSetup` losing a race with a concurrent setup request.
func TestTheNewWriteDoesNotChangeWhatAClaimedInstanceAnswers(t *testing.T) {
	t.Run("the form redirects to sign-in", func(t *testing.T) {
		f := newSetup(t, true)
		principalOn(t, f.pool, "first@example.com")

		yes := true
		resp := f.claim(&yes)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /setup on a claimed instance = %d, want 303", resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != "/login" {
			t.Errorf("Location = %q, want /login", got)
		}
	})

	t.Run("the API answers 404", func(t *testing.T) {
		f := newSetup(t, true)
		principalOn(t, f.pool, "first@example.com")

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			f.server.URL+"/api/v1/auth/setup",
			strings.NewReader(`{"email":"a@example.com","password":"a-sufficiently-long-password","update_check":false}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST /api/v1/auth/setup on a claimed instance = %d, want 404",
				resp.StatusCode)
		}
	})
}

// --- the upgrade prompt (D164) ----------------------------------------------

// upgraded is an instance in the state migration 04300 leaves an existing one
// in: claimed by somebody, and with the update-check question unanswered.
//
// It signs in as an administrator, because that is the reader D164 names — the
// question is put at the first *administrative* sign-in, and the dashboard is
// where a sign-in lands.
func upgraded(t *testing.T, updateCheck bool) *setupFixture {
	t.Helper()
	f := newSetup(t, updateCheck)
	f.signIn("operator@example.com", true)
	wantSetting(t, f.pool, nil)
	return f
}

// promptText is a phrase from the prompt and from nowhere else on the page.
const promptText = "Check for new LinkCtrl releases?"

// TestAnUpgradedInstanceAsksTheAdministratorWhoSignsIn.
//
// D164's surface. The prompt has to carry the enumeration for the reason the
// setup form's does: the operator is being asked to agree to an outbound
// connection, and *what it sends* has to be beside the control rather than one
// document away. It also has to say why it is being asked now, or it reads as a
// feature announcement rather than as a question.
func TestAnUpgradedInstanceAsksTheAdministratorWhoSignsIn(t *testing.T) {
	f := upgraded(t, true)
	page := f.dashboard()

	if !strings.Contains(page, promptText) {
		t.Fatal("an upgraded instance does not ask its administrator anything. " +
			"D164 is that it is asked at the first administrative sign-in, and " +
			"until it answers the check is off — so with no prompt the feature " +
			"never runs on any instance that existed before 0.3.0.")
	}
	for _, phrase := range []string{"User-Agent", "Nothing else", "LINKCTRL_UPDATE_CHECK"} {
		if !strings.Contains(page, phrase) {
			t.Errorf("the prompt does not mention %q. What the request carries "+
				"belongs beside the control, not one document away.", phrase)
		}
	}
	if !strings.Contains(page, "upgraded") {
		t.Error("the prompt does not say why it is being asked now, which reads " +
			"as an announcement rather than as a question about this instance")
	}
}

// TestOnlyAnAdministratorIsAsked.
//
// Whether the box phones home is the operator's decision, and the same bound the
// release notification is under: a workspace member can act on none of it and
// must not be able to answer for the instance.
//
// **Two mechanisms deliver that and this test is satisfied by either**, which
// was checked rather than assumed: removing the handler's `Can(PermAdmin)`
// leaves it green, because `UpdateCheckAnswered` refuses in the service and a
// failed read draws no prompt. Removing both turns it red on all three
// assertions. That is defence in depth working, and it is the same shape
// TestTheNewWriteDoesNotChangeWhatAClaimedInstanceAnswers records about the
// setup path.
func TestOnlyAnAdministratorIsAsked(t *testing.T) {
	f := newSetup(t, true)
	f.signIn("member@example.com", false)

	if strings.Contains(f.dashboard(), promptText) {
		t.Error("an account with no instance grant is asked whether the instance " +
			"may contact GitHub. That is the operator's decision, and this reader " +
			"is not the operator.")
	}

	// And the refusal is not merely cosmetic: the route refuses them too.
	resp := f.post("/instance/update-check", url.Values{"answer": {"yes"}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusSeeOther {
		t.Error("a member's answer was accepted. Hiding a control is not " +
			"authorization; the check belongs in the service, where both surfaces " +
			"reach it.")
	}
	wantSetting(t, f.pool, nil)
}

// TestTheDeploymentsNoIsNotReopenedAsAQuestion.
//
// D160's asymmetry, at the surface D164 added: an air-gapped instance has been
// answered from the side that outranks the browser, so asking again would offer
// an administrator a choice the deployment has already taken away from them.
func TestTheDeploymentsNoIsNotReopenedAsAQuestion(t *testing.T) {
	f := upgraded(t, false)

	if strings.Contains(f.dashboard(), promptText) {
		t.Error("an instance with LINKCTRL_UPDATE_CHECK=false asks whether to " +
			"check for releases. The variable is the deployment's answer and it " +
			"only ever says no; a prompt here offers a choice that cannot be acted on.")
	}
}

// TestTheAdministratorsAnswerIsRecordedAndTheQuestionCloses covers both answers,
// and that either one ends the asking.
func TestTheAdministratorsAnswerIsRecordedAndTheQuestionCloses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   bool
	}{
		{name: "yes", answer: "yes", want: true},
		{name: "no", answer: "no", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := upgraded(t, true)

			resp := f.post("/instance/update-check", url.Values{"answer": {tc.answer}})
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("POST /instance/update-check = %d, want 303", resp.StatusCode)
			}
			if got := resp.Header.Get("Location"); got != "/dashboard" {
				t.Errorf("Location = %q, want /dashboard", got)
			}
			wantSetting(t, f.pool, answered(tc.want))

			if strings.Contains(f.dashboard(), promptText) {
				t.Error("the prompt is still on the page after being answered, so " +
					"an operator who said no is asked again on every visit")
			}
		})
	}
}

// TestASecondAnswerChangesNothing.
//
// The write is conditional on the question still being open, which is what keeps
// this route from becoming the instance-settings page D161 declined to build.
// Two tabs, a double click, or a second principal all reach the same statement,
// and the first answer is the one that stands.
//
// It is a 303 rather than an error because the reader wanted the question
// settled and it is; a failure page would be reporting a state they asked for.
func TestASecondAnswerChangesNothing(t *testing.T) {
	f := upgraded(t, true)

	first := f.post("/instance/update-check", url.Values{"answer": {"no"}})
	_ = first.Body.Close()
	wantSetting(t, f.pool, answered(false))

	second := f.post("/instance/update-check", url.Values{"answer": {"yes"}})
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusSeeOther {
		t.Errorf("a second answer = %d, want 303: the question is closed, which is "+
			"what the reader wanted, not a failure", second.StatusCode)
	}
	wantSetting(t, f.pool, answered(false))
}

// TestAFormWithNoAnswerAnswersNothing.
//
// Unlike the setup form's checkbox, absence here is not *no*: this is two
// buttons, and a post carrying neither did not come from the prompt. Recording it
// as a refusal would let a stray submission answer for the operator, and the
// answer it gave would be the one that stands.
func TestAFormWithNoAnswerAnswersNothing(t *testing.T) {
	f := upgraded(t, true)

	resp := f.post("/instance/update-check", url.Values{})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a post with no answer = %d, want 400", resp.StatusCode)
	}
	wantSetting(t, f.pool, nil)
}

// TestTheAnswerGivenOnTheDashboardIsTheOneThePassReads.
//
// The two halves of this file, joined once. Everything above drives the pass
// against a row a test wrote; this drives it against a row an administrator
// wrote through the browser, so a surface that recorded the answer somewhere the
// pass does not read would fail here and nowhere else.
func TestTheAnswerGivenOnTheDashboardIsTheOneThePassReads(t *testing.T) {
	f := upgraded(t, true)
	stub := newReleaseStub(t, release("v0.0.1"))
	svc := newUpdateService(t, f.pool, "0.3.0", stub.server.URL, nil)

	if err := svc.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stub.calls.Load(); got != 0 {
		t.Fatalf("an unanswered instance made %d requests before anybody answered", got)
	}

	resp := f.post("/instance/update-check", url.Values{"answer": {"yes"}})
	_ = resp.Body.Close()

	if err := svc.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Errorf("after the administrator said yes the instance made %d requests, "+
			"want 1", got)
	}
}
