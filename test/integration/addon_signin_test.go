//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// M69.5's claim that needs a real database and a real module: the sign-in page
// offers an add-on's link **only** once an operator has agreed, and the agreeing
// is M68's settings mechanism rather than an operation of its own.
//
// Everything else about the offer — what a manifest may declare, how the host
// composes the target, what the page renders — is asserted without a database in
// internal/addon, internal/httpx and internal/ui. What cannot be asserted there is
// this: the consent is a row in `addon_settings`, written by `SaveSettings`, and
// the link appears and disappears with it.

// signInFixture is a host with a database behind it, running one module that
// serves routes, can mint, and has asked for a link on the sign-in page.
type signInFixture struct {
	host      *addon.Host
	principal *auth.Identity
	server    *httptest.Server
}

func newSignInHost(t *testing.T, names ...string) *signInFixture {
	t.Helper()
	pool, dsn, dir := newAddonDB(t, names...)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	principal, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "principal@example.com", Name: "Principal",
		Password: "a-sufficiently-long-password", IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("claim the instance: %v", err)
	}
	for _, name := range names {
		writeSignInAddon(t, dir, name)
	}

	sink := &logSink{}
	h, err := addon.Open(context.Background(), addon.Options{
		Dir: dir, DB: pool, DSN: dsn, Audit: audit.NewService(pool),
		Metrics: observability.NewMetrics(),
		Logger:  slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("open a host: %v\n%s", err, sink.String())
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })

	// The whole chain, because the two halves this milestone joins are in different
	// packages: internal/addon decides what is offered and internal/ui decides how
	// it is drawn, and nothing but a request through the router proves the handler
	// carries one to the other. `AddonSignIn` is the field cmd/linkctrl wires from
	// the same host.
	cfg := config.Config{AppEnv: config.Development, BaseURL: "http://links.test"}
	cfg.Auth.SessionAbsoluteTTL = 30 * 24 * time.Hour
	cfg.Auth.SessionIdleTTL = 7 * 24 * time.Hour
	renderer, err := ui.New()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	srv := httptest.NewServer(httpx.NewRouter(httpx.Deps{
		Config: cfg,
		Health: &httpx.Health{DB: pool},
		Auth:   authSvc,
		Web: &httpx.Web{
			UI: renderer, Config: cfg, Auth: authSvc,
			Addons: h, AddonSignIn: h,
		},
	}))
	t.Cleanup(srv.Close)

	return &signInFixture{host: h, principal: principal, server: srv}
}

// loginPage is what an anonymous visitor is served at the front door.
func (f *signInFixture) loginPage(t *testing.T) string {
	t.Helper()
	resp, err := f.server.Client().Get(f.server.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// signInAddonFiles is the manifest and module of an add-on that declares both
// sign-in fields, the grant that makes the offer meaningful, and the grant that
// makes its target reachable.
//
// Returned as bytes rather than written, so the same pair serves the boot route
// and M67's runtime install — which is what the ordering test needs, and is the
// arrangement that keeps the two routes describing one add-on.
func signInAddonFiles(t *testing.T, name string) (manifest, module []byte) {
	t.Helper()
	code := addonFixture(t, "pages")
	sum := sha256.Sum256(code)
	m := addon.Manifest{
		SchemaVersion: addon.SchemaVersion,
		Name:          name,
		Version:       "1.0.0",
		ABIVersion:    1,
		Module:        name + ".wasm",
		SHA256:        hex.EncodeToString(sum[:]),
		// Declared `degrade` and loaded `required` — an add-on holding session.mint
		// is required whatever its manifest says, which is M65's rule and is not this
		// milestone's to restate. It is written here so the fixture is honest about
		// what the host will do with it.
		FailureClass: addon.ClassDegrade,
		Permissions:  []string{"routes.own_prefix", "session.mint"},
		SignInLabel:  "Sign in with " + name,
		SignInPath:   "start",
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return raw, code
}

// writeSignInAddon places that pair in the add-ons directory, the way an
// operator would before a boot.
func writeSignInAddon(t *testing.T, root, name string) {
	t.Helper()
	manifest, module := signInAddonFiles(t, name)
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".wasm"), module, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, addon.ManifestFile), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The operator decides whether it appears at all, and it is off until they do.
func TestASignInLinkNeedsTheOperatorsConsent(t *testing.T) {
	f := newSignInHost(t, "sso")

	if links := f.host.SignInLinks(t.Context()); len(links) != 0 {
		t.Fatalf("an installed add-on offers %v before anybody agreed", links)
	}

	// The consent is a setting on the manager's own page, saved by the same call
	// every other setting is saved by. That is the whole of the mechanism: no
	// operation was added for it, which is why the inherited *every UI feature has
	// API support* rule is discharged by M68 rather than by this milestone.
	views, err := f.host.SaveSettings(t.Context(), f.principal, "sso",
		map[string]string{addon.SignInConsentSetting: "true"})
	if err != nil {
		t.Fatalf("agree to the link: %v", err)
	}
	var consent *addon.SettingView
	for i := range views {
		if views[i].Name == addon.SignInConsentSetting {
			consent = &views[i]
		}
	}
	if consent == nil {
		t.Fatalf("the manager does not render the consent at all; it renders %v", views)
	}
	if !consent.IsSignIn() || !consent.IsToggle() || !consent.On() {
		t.Errorf("the saved consent reads back as %+v", *consent)
	}

	links := f.host.SignInLinks(t.Context())
	if len(links) != 1 {
		t.Fatalf("after the operator agreed the sign-in page offers %v, want one link", links)
	}
	if got, want := links[0].Label, "Sign in with sso"; got != want {
		t.Errorf("label is %q, want %q", got, want)
	}
	// The host's composition, not the manifest's string.
	if got, want := links[0].Href, "/addons/sso/start"; got != want {
		t.Errorf("href is %q, want %q", got, want)
	}

	// And it goes away again. An operator who changes their mind is the same act
	// in the other direction, and a link that could only be turned on would be a
	// consent nobody could withdraw.
	if _, err := f.host.SaveSettings(t.Context(), f.principal, "sso",
		map[string]string{addon.SignInConsentSetting: "false"}); err != nil {
		t.Fatalf("withdraw the consent: %v", err)
	}
	if links := f.host.SignInLinks(t.Context()); len(links) != 0 {
		t.Errorf("the link survives the consent being withdrawn: %v", links)
	}
}

// More than one is ordered, and the order is neither the add-on's nor an
// accident of when things happened.
//
// **`alpha` arrives last and has to be drawn first**, and it arrives by two
// routes at once: the operator consents to `zeta` before `alpha`, and `alpha` is
// installed at runtime after `zeta` was loaded at boot. Both are orders a host
// could plausibly have used and neither is stable — M67's install appends to the
// loaded set, so an add-on installed without a restart would sit last until the
// next boot and then move, which is a link changing position for a reason nobody
// could connect to it. Sorting by name is what survives both.
func TestTwoSignInLinksAreOrderedByTheHost(t *testing.T) {
	f := newSignInHost(t, "zeta")

	if _, err := f.host.SaveSettings(t.Context(), f.principal, "zeta",
		map[string]string{addon.SignInConsentSetting: "true"}); err != nil {
		t.Fatalf("agree to zeta's link: %v", err)
	}

	manifest, module := signInAddonFiles(t, "alpha")
	if _, err := f.host.Install(t.Context(), f.principal, addon.InstallRequest{
		Manifest: manifest, Module: module,
	}); err != nil {
		t.Fatalf("install alpha at runtime: %v", err)
	}
	if _, err := f.host.SaveSettings(t.Context(), f.principal, "alpha",
		map[string]string{addon.SignInConsentSetting: "true"}); err != nil {
		t.Fatalf("agree to alpha's link: %v", err)
	}

	links := f.host.SignInLinks(t.Context())
	if len(links) != 2 {
		t.Fatalf("the sign-in page offers %v, want two links", links)
	}
	if links[0].Addon != "alpha" || links[1].Addon != "zeta" {
		t.Errorf("the order is %s then %s; the host orders by name, and alpha was "+
			"both installed and consented to second", links[0].Addon, links[1].Addon)
	}
	// Stable: asking again is asking the same set in the same order.
	again := f.host.SignInLinks(t.Context())
	for i := range links {
		if again[i] != links[i] {
			t.Errorf("the order moved between two reads: %v then %v", links, again)
		}
	}
}

// The visitor's own view of the whole thing: nothing at the front door until an
// operator agrees, then a link, composed by the host and reachable.
func TestTheSignInPageCarriesTheOfferTheOperatorAgreedTo(t *testing.T) {
	f := newSignInHost(t, "sso")

	if page := f.loginPage(t); strings.Contains(page, "/addons/sso/") {
		t.Fatal("the sign-in page offers an add-on's link before anybody agreed")
	}

	if _, err := f.host.SaveSettings(t.Context(), f.principal, "sso",
		map[string]string{addon.SignInConsentSetting: "true"}); err != nil {
		t.Fatalf("agree to the link: %v", err)
	}

	page := f.loginPage(t)
	for _, want := range []string{`href="/addons/sso/start"`, "Sign in with sso"} {
		if !strings.Contains(page, want) {
			t.Errorf("the sign-in page does not carry %q", want)
		}
	}
	// And the local form is still the way in. An instance whose add-on is broken
	// must still let its operator sign in with a password.
	for _, want := range []string{`action="/login"`, `name="email"`, `name="password"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the local sign-in form lost %q", want)
		}
	}

	// The link leads somewhere this instance serves. Not a 404, which is what
	// m69.5.md's second risk calls worse than no link at all — the module answers,
	// whatever it answers with.
	resp, err := f.server.Client().Get(f.server.URL + "/addons/sso/start")
	if err != nil {
		t.Fatalf("follow the link: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		t.Error("the link this instance drew on its own sign-in page answers 404")
	}
}
