//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// M67's claims that need a real database: who may install an add-on, that no API
// key can be one of them, and that the act leaves an instance-wide audit record
// somebody can read.
//
// The mechanism — staging, the rename, the pool drain, the leak bound — is
// internal/addon's own, asserted against a real wasm runtime without a database.
// What lives here is everything that needs an identity, and an identity is a row.

type lifecycleFixture struct {
	t         *testing.T
	pool      *pgxpool.Pool
	host      *addon.Host
	dir       string
	auth      *auth.Service
	keys      *auth.APIKeyService
	audit     *audit.Service
	principal *auth.Identity
	tenant    *auth.Identity
	module    []byte
	log       *logSink
}

func newAddonLifecycle(t *testing.T) *lifecycleFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	auditSvc := audit.NewService(pool)
	keySvc, err := auth.NewAPIKeyService(pool, authSvc, auth.APIKeyConfig{Pepper: testPepper})
	if err != nil {
		t.Fatal(err)
	}

	// The first account claims the instance, which is what makes it the principal —
	// and the principal is what holds `addons.manage`, because migration 04700 puts
	// the scope in auth.InstancePrincipalScopes and the setup path confers the whole
	// list in one transaction.
	principal, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "principal@example.com", Name: "Principal",
		Password: "a-sufficiently-long-password", IsFirstUser: true,
	})
	if err != nil {
		t.Fatalf("claim the instance: %v", err)
	}
	// An ordinary account, owner of its own organization exactly as registration
	// makes one. This is the account F15's shape would have handed the instance to.
	tenant, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "tenant@example.com", Name: "Tenant",
		Password: "another-sufficiently-long-password",
	})
	if err != nil {
		t.Fatalf("register a tenant: %v", err)
	}

	dir := t.TempDir()
	sink := &logSink{}
	host, err := addon.Open(t.Context(), addon.Options{
		Dir:    dir,
		Audit:  auditSvc,
		Logger: slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("open a host on an empty directory: %v\n%s", err, sink.String())
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	return &lifecycleFixture{
		t: t, pool: pool, host: host, dir: dir, auth: authSvc, keys: keySvc,
		audit: auditSvc, principal: principal, tenant: tenant,
		module: addonFixture(t, "pages"), log: sink,
	}
}

// upload is the pair a caller sends, describing the fixture module honestly.
func (f *lifecycleFixture) upload(name string, permissions ...string) addon.InstallRequest {
	f.t.Helper()
	m := manifestForModule(f.t, name, f.module, permissions)
	raw, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	return addon.InstallRequest{Manifest: raw, Module: f.module}
}

func manifestForModule(t *testing.T, name string, code []byte, permissions []string) addon.Manifest {
	t.Helper()
	return addon.Manifest{
		SchemaVersion: 1,
		Name:          name,
		Version:       "1.0.0",
		ABIVersion:    1,
		Module:        name + ".wasm",
		SHA256:        addonDigest(code),
		FailureClass:  "degrade",
		Permissions:   permissions,
	}
}

// The permission, from both sides. The account that claimed the instance may
// install; an owner of their own organization may not, which is the whole reason
// the scope is instance-level rather than a role permission.
func TestOnlyTheInstancePrincipalMayInstallAnAddon(t *testing.T) {
	f := newAddonLifecycle(t)

	if !f.principal.Can(auth.PermAddonsManage) {
		t.Fatalf("the account that claimed the instance does not hold %s; "+
			"migration 04700 or auth.InstancePrincipalScopes is not doing its job",
			auth.PermAddonsManage)
	}
	if f.tenant.Can(auth.PermAddonsManage) {
		t.Fatalf("an ordinary registrant holds %s: on SIGNUP_MODE=open that is one "+
			"registration away from a stranger uploading code into this server",
			auth.PermAddonsManage)
	}

	req := f.upload("gated", "routes.own_prefix")
	if _, err := f.host.Install(t.Context(), f.tenant, req); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a tenant's install answered %v, want forbidden", err)
	}
	if f.host.Len() != 0 {
		t.Fatal("a refused install started an add-on")
	}
	if _, err := f.host.Install(t.Context(), f.principal, req); err != nil {
		t.Fatalf("the principal's install: %v\n%s", err, f.log.String())
	}
	if _, err := f.host.Remove(t.Context(), f.tenant, "gated"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a tenant's removal answered %v, want forbidden", err)
	}
	if f.host.Len() != 1 {
		t.Error("a refused removal unloaded the add-on")
	}
}

// D18's second limb, in the form the enumeration actually enforces it: the scope
// is in auth.NonDelegableScopes, so a key cannot be issued with it at all.
//
// Asserted twice on purpose. The map is the mechanism and a test reading the map
// is a test of the mechanism; minting a key is what proves the mechanism is
// wired to the operation somebody would actually perform.
func TestNoAPIKeyCanHoldAddonsManage(t *testing.T) {
	f := newAddonLifecycle(t)

	if _, blocked := auth.NonDelegableScopes[auth.PermAddonsManage]; !blocked {
		t.Fatalf("%s is not in NonDelegableScopes; a key holding it would carry "+
			"whatever an add-on's own manifest declares, past every scope the key "+
			"was issued with", auth.PermAddonsManage)
	}

	_, err := f.keys.Create(t.Context(), f.principal, auth.CreateAPIKeyInput{
		Name: "installer", Scopes: []string{auth.PermAddonsManage},
	})
	if err == nil {
		t.Fatal("a key was issued holding addons.manage")
	}
}

// The record, as the operator who has to answer *who put that module there* reads
// it: instance-wide, so it is at `/instance/audit` and belongs to no tenant, and
// naming the module, the version and the digest.
func TestTheLifecycleIsAudited(t *testing.T) {
	f := newAddonLifecycle(t)
	req := f.upload("recorded", "routes.own_prefix")

	installed, err := f.host.Install(t.Context(), f.principal, req)
	if err != nil {
		t.Fatalf("installing: %v\n%s", err, f.log.String())
	}
	if _, err := f.host.Remove(t.Context(), f.principal, "recorded"); err != nil {
		t.Fatalf("removing: %v", err)
	}

	page, err := f.audit.ListInstance(t.Context(), f.principal, audit.Filter{Limit: 50})
	if err != nil {
		t.Fatalf("read the instance audit log: %v", err)
	}
	seen := map[string]map[string]any{}
	for _, e := range page.Items {
		seen[e.Action] = e.Metadata
	}
	for _, action := range []string{"addon.installed", "addon.removed"} {
		meta, ok := seen[action]
		if !ok {
			t.Errorf("no %s record is in the instance-wide log", action)
			continue
		}
		if meta["module"] != "recorded" {
			t.Errorf("%s names module %v", action, meta["module"])
		}
		if meta["sha256"] != installed.SHA256 {
			t.Errorf("%s names digest %v, want %q", action, meta["sha256"], installed.SHA256)
		}
		if meta["version"] != "1.0.0" {
			t.Errorf("%s names version %v", action, meta["version"])
		}
	}
	// A tenant cannot read it, which is what "instance-wide" costs and buys: the
	// record is outside every organization, so `audit.read` does not reach it.
	if _, err := f.audit.ListInstance(t.Context(), f.tenant, audit.Filter{Limit: 1}); err == nil {
		t.Error("a tenant read the instance-wide audit log")
	}
}

// Arrival and departure, driven through the surface an add-on actually serves:
// the module answers a request, and after the removal the same request reaches
// nothing at all.
func TestAnInstalledAddonServesAndAReRemovedOneDoesNot(t *testing.T) {
	f := newAddonLifecycle(t)
	req := f.upload("serving", "routes.own_prefix")

	if _, err := f.host.Route(t.Context(), "serving", pageRequest()); !errors.Is(err, addon.ErrNoRoute) {
		t.Fatalf("an add-on that is not installed answered %v", err)
	}
	if _, err := f.host.Install(t.Context(), f.principal, req); err != nil {
		t.Fatalf("installing: %v\n%s", err, f.log.String())
	}
	resp, err := f.host.Route(t.Context(), "serving", pageRequest())
	if err != nil {
		t.Fatalf("the installed add-on did not answer: %v\n%s", err, f.log.String())
	}
	if resp.Status != http.StatusOK {
		t.Errorf("the installed add-on answered %d", resp.Status)
	}

	out, err := f.host.Remove(t.Context(), f.principal, "serving")
	if err != nil {
		t.Fatalf("removing: %v", err)
	}
	if out.Draining {
		t.Error("a removal with nothing in flight reported an interruption")
	}
	if _, err := f.host.Route(t.Context(), "serving", pageRequest()); !errors.Is(err, addon.ErrNoRoute) {
		t.Fatalf("a removed add-on answered %v, want no route", err)
	}
	// And the directory it came from is gone, so the next boot agrees with this
	// process about what is installed.
	if _, err := os.Stat(filepath.Join(f.dir, "serving")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a removed add-on's directory survived: %v", err)
	}
}

// A read-only add-ons directory is what docs/configuration.md tells an operator
// to mount, and it is exactly what stops this API working. The refusal says so
// rather than surfacing a bare permission error.
func TestAReadOnlyAddonsDirectoryRefusesAnInstallAndSaysWhy(t *testing.T) {
	f := newAddonLifecycle(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root, which ignores the mode this test sets")
	}
	if err := os.Chmod(f.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.dir, 0o700) })

	_, err := f.host.Install(t.Context(), f.principal, f.upload("readonly"))
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("installing into a read-only directory answered %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("the refusal does not name what an operator has to change: %v", err)
	}
}

func pageRequest() addon.RequestIn {
	return addon.RequestIn{
		Method: http.MethodGet, Path: "/",
		ClientIP:  netip.MustParseAddr("198.51.100.9"),
		UserAgent: "integration/1.0",
	}
}

// addonDigest is the manifest's `sha256` field, computed from the bytes it is
// about, so a test cannot assert against a manifest that lies by accident.
func addonDigest(code []byte) string {
	sum := sha256.Sum256(code)
	return hex.EncodeToString(sum[:])
}

// The same gate on the second door (M68.6).
//
// Asserted here rather than in internal/addon for that package's reason: nothing
// outside internal/auth can mint an authority, so a unit test cannot produce an
// actor holding `addons.manage` and a gate only reachable through the check would
// be a gate only testable against a database. This is that database.
//
// **Both directions, and the refused one is checked before the fetch.** A tenant's
// URL install must be forbidden rather than refused later for some other reason —
// if the permission check sat after the fetch, an ordinary registrant on an open
// instance would be able to make this server connect to an address of their
// choosing, which is the request forgery the whole design is arranged against.
// The address here is documentation space and is carved out, so a fetch that
// happened would be visible as a different refusal than this one.
func TestOnlyTheInstancePrincipalMayInstallAnAddonFromAURL(t *testing.T) {
	f := newAddonLifecycle(t)

	req := addon.URLInstallRequest{
		URL:    "https://192.0.2.1/gated.tar",
		SHA256: strings.Repeat("ab", 32),
	}
	_, err := f.host.InstallFromURL(t.Context(), f.tenant, req)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a tenant's URL install answered %v, want forbidden — and anything "+
			"other than forbidden means this server was asked to make a request on "+
			"the word of somebody who may not install add-ons", err)
	}
	if f.host.Len() != 0 {
		t.Fatal("a refused URL install started an add-on")
	}

	// The principal gets past the gate and is stopped by the address policy
	// instead, which is the pair of facts this asserts: the permission is what
	// decides who may ask, and the address policy is what decides where.
	_, err = f.host.InstallFromURL(t.Context(), f.principal, req)
	if errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the instance principal's URL install was forbidden: %v", err)
	}
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("the principal's URL install answered %v, want a refusal naming a "+
			"bound: 192.0.2.0/24 is documentation space and is not dialled", err)
	}
	if ve[0].Code != "fetch_address_refused" {
		t.Errorf("the principal's URL install was refused with %q, want the address "+
			"policy — a refusal naming the wrong bound is one an operator cannot act on",
			ve[0].Code)
	}
	if f.host.Len() != 0 {
		t.Fatal("a refused fetch installed something")
	}
}
