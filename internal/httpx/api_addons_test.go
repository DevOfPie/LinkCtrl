package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// These tests are about the boundary and not about the host: what a multipart
// body has to look like, and what a caller learns when it does not. Whether an
// add-on actually installs is internal/addon's, and test/integration drives the
// two together against a real runtime.

// nopAddonLifecycle installs and removes nothing. It exists for maximalDeps, so
// the two routes are registered in the pass both router guards iterate.
type nopAddonLifecycle struct{}

func (nopAddonLifecycle) Install(
	context.Context, *auth.Identity, addon.InstallRequest,
) (addon.Installed, error) {
	return addon.Installed{}, domain.ErrNotFound
}

func (nopAddonLifecycle) InstallFromURL(
	context.Context, *auth.Identity, addon.URLInstallRequest,
) (addon.Installed, error) {
	return addon.Installed{}, domain.ErrNotFound
}

func (nopAddonLifecycle) Remove(
	context.Context, *auth.Identity, string,
) (addon.Installed, error) {
	return addon.Installed{}, domain.ErrNotFound
}

// recordingLifecycle keeps what the handler handed it.
type recordingLifecycle struct {
	req addon.InstallRequest
	// fetched is M68.6's shape, kept beside the upload so a test can assert which
	// of the two operations the one endpoint chose.
	fetched addon.URLInstallRequest
	removed string
	out     addon.Installed
	err     error

	// The manager half (M68): what the double will answer, and what it was asked.
	listed   []addon.Managed
	orphans  []addon.Orphan
	settings []addon.SettingView
	detailed string
	savedTo  string
	saved    map[string]string
	purged   []string
}

func (r *recordingLifecycle) Install(
	_ context.Context, _ *auth.Identity, req addon.InstallRequest,
) (addon.Installed, error) {
	r.req = req
	return r.out, r.err
}

func (r *recordingLifecycle) InstallFromURL(
	_ context.Context, _ *auth.Identity, req addon.URLInstallRequest,
) (addon.Installed, error) {
	r.fetched = req
	return r.out, r.err
}

func (r *recordingLifecycle) Remove(
	_ context.Context, _ *auth.Identity, name string,
) (addon.Installed, error) {
	r.removed = name
	return r.out, r.err
}

// addonUpload builds a multipart body from field name to content.
func addonUpload(t *testing.T, parts map[string][]byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for name, body := range parts {
		part, err := w.CreateFormFile(name, name+".bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

func postAddon(t *testing.T, a *AddonAPI, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/addons", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	a.Install(rec, req)
	return rec
}

// The two parts arrive as the two fields, verbatim, and nothing else about the
// request decides anything.
func TestAnInstallUploadCarriesTheManifestAndTheModule(t *testing.T) {
	svc := &recordingLifecycle{out: addon.Installed{Name: "minimal", Version: "1.0.0"}}
	a := &AddonAPI{Addons: svc}

	ct, body := addonUpload(t, map[string][]byte{
		"manifest": []byte(`{"name":"minimal"}`),
		"module":   {0x00, 0x61, 0x73, 0x6d},
	})
	rec := postAddon(t, a, ct, body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("install answered %d, want 201: %s", rec.Code, rec.Body)
	}
	if got := string(svc.req.Manifest); got != `{"name":"minimal"}` {
		t.Errorf("the manifest reached the service as %q", got)
	}
	if !bytes.Equal(svc.req.Module, []byte{0x00, 0x61, 0x73, 0x6d}) {
		t.Errorf("the module reached the service as %v", svc.req.Module)
	}
	var out addon.Installed
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the answer is not an Installed: %v", err)
	}
	if out.Name != "minimal" {
		t.Errorf("the answer names %q", out.Name)
	}
}

// A part this endpoint does not know is a caller expecting something that will
// not happen, and it is refused rather than dropped — the manifest parser's rule
// about unknown keys, applied to the body that carries the manifest.
func TestAnInstallUploadRefusesAPartItDoesNotKnow(t *testing.T) {
	svc := &recordingLifecycle{}
	a := &AddonAPI{Addons: svc}

	ct, body := addonUpload(t, map[string][]byte{
		"manifest":  []byte(`{}`),
		"module":    {0x00},
		"migration": []byte("create table t()"),
	})
	rec := postAddon(t, a, ct, body)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown part answered %d, want 422: %s", rec.Code, rec.Body)
	}
	if svc.req.Module != nil {
		t.Error("the service was called with a body the reader should have refused")
	}
	if !strings.Contains(rec.Body.String(), "migration") {
		t.Errorf("the refusal does not name the part it refused: %s", rec.Body)
	}
}

// Not multipart at all is a refusal that says what the endpoint takes, rather
// than a 500 from a reader that assumed.
func TestAnInstallRefusesABodyThatIsNotMultipart(t *testing.T) {
	a := &AddonAPI{Addons: &recordingLifecycle{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/addons",
		strings.NewReader(`{"module":"aGk="}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Install(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a JSON body answered %d, want 422: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "multipart/form-data") {
		t.Errorf("the refusal does not say what the endpoint takes: %s", rec.Body)
	}
}

// The bound is on the body rather than on a part, so two parts that each fit
// cannot add up to a body that does not.
func TestAnInstallRefusesABodyPastTheBound(t *testing.T) {
	a := &AddonAPI{Addons: &recordingLifecycle{}}
	ct, body := addonUpload(t, map[string][]byte{
		"manifest": []byte(`{}`),
		"module":   bytes.Repeat([]byte{0x41}, addon.MaxUploadBytes+1),
	})
	rec := postAddon(t, a, ct, body)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an oversized body answered %d, want 422: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "32 MiB") {
		t.Errorf("the refusal does not name the bound: %s", rec.Body)
	}
}

// Removal answers with the summary rather than 204, because the orphaned schema
// is what somebody standing at this moment can decide about.
func TestRemovalAnswersWithTheOrphanItLeft(t *testing.T) {
	svc := &recordingLifecycle{out: addon.Installed{
		Name: "minimal", Version: "1.0.0", Schema: "addon_minimal",
	}}
	a := &AddonAPI{Addons: svc}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/addons/minimal", nil)
	req.SetPathValue("name", "minimal")
	rec := httptest.NewRecorder()
	a.Remove(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("removal answered %d, want 200: %s", rec.Code, rec.Body)
	}
	if svc.removed != "minimal" {
		t.Errorf("the service was asked to remove %q", svc.removed)
	}
	var out addon.Installed
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the answer is not an Installed: %v", err)
	}
	if out.Schema != "addon_minimal" {
		t.Errorf("the answer does not name the orphaned schema: %+v", out)
	}
}

// The service's refusals reach the caller as themselves, which is what keeps the
// permission check in one place: this handler asks nothing about who is calling.
func TestTheLifecycleRefusalsReachTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"forbidden", domain.ErrForbidden, http.StatusForbidden},
		{"conflict", domain.ErrConflict, http.StatusConflict},
		{"unavailable", addon.ErrNoAddonsDir, http.StatusServiceUnavailable},
		{"not found", domain.ErrNotFound, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &AddonAPI{Addons: &recordingLifecycle{err: tc.err}}
			req := httptest.NewRequest(http.MethodDelete, "/api/v1/addons/x", nil)
			req.SetPathValue("name", "x")
			rec := httptest.NewRecorder()
			a.Remove(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%v answered %d, want %d: %s", tc.err, rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// The manager half of the interface (M68), on both doubles.
//
// Empty answers rather than fixtures: these tests are about the upload boundary,
// and what the manager's endpoints return is asserted in web_addons_test.go and
// against a real host in test/integration. What these methods buy is that both
// doubles satisfy the one interface the router holds, which is what keeps the
// upload tests from needing a second wiring.

func (nopAddonLifecycle) List(context.Context, *auth.Identity) ([]addon.Managed, error) {
	return nil, nil
}

func (nopAddonLifecycle) Detail(context.Context, *auth.Identity, string) (addon.Managed, error) {
	return addon.Managed{}, domain.ErrNotFound
}

func (nopAddonLifecycle) SaveSettings(
	context.Context, *auth.Identity, string, map[string]string,
) ([]addon.SettingView, error) {
	return nil, domain.ErrNotFound
}

func (nopAddonLifecycle) Orphans(context.Context, *auth.Identity) ([]addon.Orphan, error) {
	return nil, nil
}

func (nopAddonLifecycle) PurgeData(
	context.Context, *auth.Identity, string,
) (addon.Orphan, error) {
	return addon.Orphan{}, domain.ErrNotFound
}

func (r *recordingLifecycle) List(context.Context, *auth.Identity) ([]addon.Managed, error) {
	return r.listed, r.err
}

func (r *recordingLifecycle) Detail(
	_ context.Context, _ *auth.Identity, name string,
) (addon.Managed, error) {
	r.detailed = name
	for _, m := range r.listed {
		if m.Name == name {
			return m, nil
		}
	}
	return addon.Managed{}, domain.ErrNotFound
}

func (r *recordingLifecycle) SaveSettings(
	_ context.Context, _ *auth.Identity, name string, values map[string]string,
) ([]addon.SettingView, error) {
	r.savedTo, r.saved = name, values
	return r.settings, r.err
}

func (r *recordingLifecycle) Orphans(context.Context, *auth.Identity) ([]addon.Orphan, error) {
	return r.orphans, r.err
}

func (r *recordingLifecycle) PurgeData(
	_ context.Context, _ *auth.Identity, name string,
) (addon.Orphan, error) {
	r.purged = append(r.purged, name)
	for _, o := range r.orphans {
		if o.Name == name {
			return o, r.err
		}
	}
	return addon.Orphan{Name: name}, r.err
}

// --- M68.6: the second shape at the same address ------------------------------

// The URL shape reaches the fetch operation, and the two fields arrive as typed.
//
// One endpoint and one reader for both shapes, which is what makes the dashboard
// and `curl` the same path rather than two paths somebody keeps in step. The
// service double records which of its two operations was called, so this asserts
// the choice rather than that something was called.
func TestAnInstallFromAURLReachesTheFetchOperation(t *testing.T) {
	svc := &recordingLifecycle{out: addon.Installed{Name: "minimal", Version: "1.0.0"}}
	a := &AddonAPI{Addons: svc}

	digest := strings.Repeat("ab", 32)
	ct, body := addonUpload(t, map[string][]byte{
		"url": []byte("  https://example.com/minimal.tar\n"), "sha256": []byte(digest),
	})
	rec := postAddon(t, a, ct, body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("a URL install answered %d, want 201: %s", rec.Code, rec.Body)
	}
	if svc.fetched.URL != "https://example.com/minimal.tar" {
		t.Errorf("the URL reached the service as %q; a field pasted out of a terminal "+
			"carries whitespace and that is not a reason to refuse it", svc.fetched.URL)
	}
	if svc.fetched.SHA256 != digest {
		t.Errorf("the digest reached the service as %q", svc.fetched.SHA256)
	}
	if len(svc.req.Manifest) != 0 || len(svc.req.Module) != 0 {
		t.Error("the upload operation was reached as well; the two shapes are one call")
	}
}

// The two shapes are exclusive, and the refusal says so rather than picking one.
//
// It is enforced here as well as in the OpenAPI document's `oneOf`, because the
// schema binds a client that validates and this binds the server. A body carrying
// both is a caller who believes something will happen that will not — the same
// reading the unknown-part refusal above applies.
func TestAnInstallIsAnUploadOrAFetchAndNotBoth(t *testing.T) {
	svc := &recordingLifecycle{}
	a := &AddonAPI{Addons: svc}

	ct, body := addonUpload(t, map[string][]byte{
		"manifest": []byte(`{}`), "module": {0x00},
		"url": []byte("https://example.com/x.tar"), "sha256": []byte(strings.Repeat("a", 64)),
	})
	rec := postAddon(t, a, ct, body)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a body carrying both shapes answered %d, want 422: %s", rec.Code, rec.Body)
	}
	if len(svc.req.Module) != 0 || svc.fetched.URL != "" {
		t.Error("a body carrying both shapes reached the service anyway")
	}
}

// An empty field is a field that was not filled in.
//
// The dashboard draws two forms and a browser sends only the one that was used —
// but an unfilled file input still produces an empty part in some browsers, and a
// caller building the body by hand may send all four names. If emptiness were not
// what the reader judged, the ordinary upload from a browser would be refused as
// ambiguous, which is the shape of defect this test exists for.
func TestAnEmptyFieldDoesNotMakeAnInstallAmbiguous(t *testing.T) {
	svc := &recordingLifecycle{out: addon.Installed{Name: "minimal"}}
	a := &AddonAPI{Addons: svc}

	ct, body := addonUpload(t, map[string][]byte{
		"manifest": []byte(`{"name":"minimal"}`), "module": {0x00},
		"url": nil, "sha256": nil,
	})
	if rec := postAddon(t, a, ct, body); rec.Code != http.StatusCreated {
		t.Fatalf("an upload sent beside two empty URL fields answered %d: %s",
			rec.Code, rec.Body)
	}
	if svc.fetched.URL != "" {
		t.Error("an empty url field was read as a URL install")
	}
	if len(svc.req.Module) == 0 {
		t.Error("the upload did not reach the service")
	}
}

// A digest with no URL is its own refusal rather than an upload with no parts,
// because the two produce completely different advice.
func TestADigestWithNoURLIsRefusedAsSuch(t *testing.T) {
	svc := &recordingLifecycle{}
	a := &AddonAPI{Addons: svc}

	ct, body := addonUpload(t, map[string][]byte{"sha256": []byte(strings.Repeat("a", 64))})
	rec := postAddon(t, a, ct, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a digest with no URL answered %d, want 422: %s", rec.Code, rec.Body)
	}
	if svc.fetched.SHA256 != "" {
		t.Error("a digest with no URL reached the service")
	}
}
