//go:build integration

package integration

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// A caller-supplied count of seconds (or hours) is multiplied into a
// time.Duration — an int64 of nanoseconds — before the range checks run, so a
// count large enough to wrap the product mod 2^64 used to arrive at those
// checks already disguised: as ~290ms (a signature born expired), as a negative
// (which reads as "no value named" and silently takes the default), or as a
// small positive that sits inside the accepted band. These tests pin the
// repaired contract at the HTTP surface: every such count is refused with the
// same out_of_range answer an honestly over-the-ceiling request gets, on all
// three surfaces that accept one.

// problemErrors is the error half of a problem+json response.
type problemErrors struct {
	Status int                            `json:"status"`
	Errors []struct{ Field, Code string } `json:"errors"`
}

func (p problemErrors) has(field, code string) bool {
	for _, e := range p.Errors {
		if e.Field == field && e.Code == code {
			return true
		}
	}
	return false
}

func TestSignRefusesTTLSecondsThatWouldWrap(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	created := f.do(http.MethodPost, "/api/v1/links", map[string]any{
		"url": "https://example.com/overflow",
	})
	var made struct {
		ID string `json:"id"`
	}
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create link returned %d", created.StatusCode)
	}
	f.decode(created, &made)

	// 18446744074s multiplies to ~290ms; 9223372037s multiplies to a negative.
	// Before the guard, the first was a 201 whose signature could be expired by
	// the time the response arrived, and the second was a 201 carrying the 24h
	// default — the exact clamping the field's contract says never happens.
	for _, ttl := range []int64{18446744074, 9223372037} {
		resp := f.do(http.MethodPost, "/api/v1/links/"+made.ID+"/sign",
			map[string]any{"ttl_seconds": ttl})
		var problem problemErrors
		f.decode(resp, &problem)
		if problem.Status != http.StatusUnprocessableEntity {
			t.Fatalf("ttl_seconds=%d returned %d, want 422", ttl, problem.Status)
		}
		if !problem.has("ttl_seconds", "out_of_range") {
			t.Errorf("ttl_seconds=%d: errors = %+v, want out_of_range on ttl_seconds", ttl, problem.Errors)
		}
	}

	// The ceiling itself still signs, so the refusals above are the guard and
	// not a broken fixture.
	resp := f.do(http.MethodPost, "/api/v1/links/"+made.ID+"/sign",
		map[string]any{"ttl_seconds": int64((30 * 24 * time.Hour).Seconds())})
	f.decode(resp, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the 30-day ceiling returned %d, want 201", resp.StatusCode)
	}
}

func TestRotateRefusesGraceSecondsThatWouldWrap(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	key := f.createKey("wrap-probe", "links.read")

	// 2^55 seconds multiplies to exactly zero nanoseconds, and zero is the
	// service's "use the default" — so before the guard this was a 201 with an
	// hour's grace, granted to a caller who asked for a billion years.
	resp := f.doWithKey(key.Key, http.MethodPost, "/api/v1/api-keys/rotate",
		map[string]any{"grace_seconds": int64(36028797018963968)})
	var problem problemErrors
	f.decode(resp, &problem)
	if problem.Status != http.StatusUnprocessableEntity {
		t.Fatalf("grace_seconds=2^55 returned %d, want 422", problem.Status)
	}
	if !problem.has("grace", "out_of_range") {
		t.Errorf("errors = %+v, want out_of_range on grace", problem.Errors)
	}
}

// newWebWithGates is newWeb with the gate service attached to the link service,
// which is what makes the dashboard's sign form reach a signer at all — the
// plain web fixture leaves Gates nil and Sign refuses before any ttl is judged.
func newWebWithGates(t *testing.T) *webFixture {
	t.Helper()
	pool := newDB(t)

	cfg := config.Config{
		AppEnv:        config.Development,
		BaseURL:       "http://links.test",
		SecureCookies: false,
	}
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: cfg.Auth.SessionAbsoluteTTL, Idle: cfg.Auth.SessionIdleTTL},
	})
	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}
	linkSvc := link.NewService(pool, link.Config{
		Policy:  link.DefaultDestinationPolicy(),
		BaseURL: cfg.BaseURL,
		Hasher:  authSvc.Hasher(),
		Gates:   gate.NewService(pool, gate.Config{Hasher: authSvc.Hasher()}),
	})
	stats := analytics.NewReader(pool)
	notifySvc := notify.NewService(pool)

	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg,
		Health: &httpx.Health{DB: pool},
		Auth:   authSvc,
		Keys:   keySvc,
		Links:  linkSvc,
		Stats:  stats,
		Notify: notifySvc,
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc, Keys: keySvc,
			Links: linkSvc, Stats: stats, Notify: notifySvc,
		},
	}))
	t.Cleanup(srv.Close)

	jar, _ := newCookieJar()
	return &webFixture{
		t:      t,
		server: srv,
		pool:   pool,
		client: &http.Client{
			Jar: jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func TestWebSignRefusesHoursThatWouldWrap(t *testing.T) {
	f := newWebWithGates(t)
	f.claim()

	detail := f.createLink("https://example.com/wrap-form", "wrapform")

	// The form's min/max live in the browser, so the server sees whatever a
	// crafted POST carries. 5,124,096 hours multiplies to ~2^64 nanoseconds and
	// wraps to a small positive product; before the guard that sailed under the
	// 30-day ceiling and produced a signature.
	resp := f.postForm(detail+"/sign", url.Values{"ttl_hours": {"5124096"}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("ttl_hours=5124096 returned %d, want 422", resp.StatusCode)
	}

	// An in-range request still signs, so the 422 above is the refusal working
	// and not the fixture failing to reach the signer.
	ok := f.postForm(detail+"/sign", url.Values{"ttl_hours": {"24"}}, nil)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("ttl_hours=24 returned %d, want 200", ok.StatusCode)
	}
	if page := f.body(ok); !strings.Contains(page, "sig=") {
		t.Error("the success page carries no signed URL")
	}
}
