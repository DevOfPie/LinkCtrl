package httpx

import (
	"context"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// This file is M68's HTTP surface: what is installed, what one add-on is
// configured with, and the orphaned data left over from modules that are not.
//
// # Why it is not in api_addons.go
//
// That file is the upload, and the whole of its risk is that a request body
// becomes code the server runs. Everything here is a read except two writes whose
// risk is ordinary — a form and a delete — and keeping them apart is what lets a
// reviewer hold the dangerous file at once. The two share one interface and one
// struct, so nothing is wired twice.
//
// # `orphaned-data` cannot collide with an add-on's name
//
// `GET /addons/{name}` and `GET /addons/orphaned-data` are two patterns under one
// prefix, and this product does not resolve that kind of ambiguity by precedence
// (D263's rule, applied twice already in this subsystem). It does not have to: an
// add-on's name matches `^[a-z][a-z0-9_]{1,30}$` — `store.checkAddonName` — and a
// hyphen is not in it, so no manifest can ever claim this path. The reservation is
// a property of the name grammar rather than a list somebody has to maintain, and
// TestOrphanPathCannotBeAnAddonName holds it against the grammar rather than
// against this comment.

// AddonManager is the whole add-on administrative surface: the lifecycle
// [AddonLifecycle] describes, plus the reads and writes the Add-on manager needs.
//
// One interface rather than two fields on the router, because one object
// implements all of it and a second field naming the same host would be a way for
// an instance to wire half of it.
type AddonManager interface {
	AddonLifecycle
	List(ctx context.Context, actor *auth.Identity) ([]addon.Managed, error)
	Detail(ctx context.Context, actor *auth.Identity, name string) (addon.Managed, error)
	SaveSettings(ctx context.Context, actor *auth.Identity, name string,
		values map[string]string) ([]addon.SettingView, error)
	Orphans(ctx context.Context, actor *auth.Identity) ([]addon.Orphan, error)
	PurgeData(ctx context.Context, actor *auth.Identity, name string) (addon.Orphan, error)
}

// AddonOrphanPath is the single segment the orphan endpoints live under, and the
// same segment the dashboard uses. Named once so the router, the spec and the
// grammar test cannot come to disagree about it.
const AddonOrphanPath = "orphaned-data"

// addonListResponse is what `GET /addons` answers.
//
// An object with one array in it rather than a bare array, like every other
// collection this API serves: a top-level array cannot grow a field, and the
// orphan count is exactly the kind of thing that wants adding later.
type addonListResponse struct {
	Addons []addon.Managed `json:"addons"`
}

type addonOrphanResponse struct {
	Orphans []addon.Orphan `json:"orphans"`
}

// addonSettingsResponse is what a detail read and a settings write both answer.
type addonSettingsResponse struct {
	Settings []addon.SettingView `json:"settings"`
}

// addonSettingsRequest is a settings write.
//
// A flat map rather than a list of {name, value} objects: the keys are the
// manifest's declared setting names, which are already a validated identifier
// grammar, and a map is what a form posts. Unknown keys are refused by the
// service rather than ignored, for the reason an unknown multipart part is.
type addonSettingsRequest struct {
	Values map[string]string `json:"values"`
}

// List answers what is installed on this instance.
func (a *AddonAPI) List(w http.ResponseWriter, r *http.Request) {
	out, err := a.Addons.List(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, addonListResponse{Addons: out})
}

// Detail answers one add-on, with its declared settings resolved.
func (a *AddonAPI) Detail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, r, domain.ErrNotFound)
		return
	}
	out, err := a.Addons.Detail(r.Context(), IdentityFrom(r.Context()), name)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

// SaveSettings writes the values an operator configured.
//
// `PUT` rather than `PATCH`, and the body is why: the manager posts every editable
// setting, an absent one is not "leave it alone" but "the form did not carry it",
// and a value that arrives empty means *unset*. That is a replace of what was
// sent, which is what PUT says.
func (a *AddonAPI) SaveSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, r, domain.ErrNotFound)
		return
	}
	var req addonSettingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Addons.SaveSettings(r.Context(), IdentityFrom(r.Context()), name, req.Values)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, addonSettingsResponse{Settings: out})
}

// Orphans answers every add-on schema no installed module owns.
func (a *AddonAPI) Orphans(w http.ResponseWriter, r *http.Request) {
	out, err := a.Addons.Orphans(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, addonOrphanResponse{Orphans: out})
}

// Purge drops one orphaned add-on's schema.
//
// `200` with the row that was deleted rather than `204`, for the reason a removal
// answers with a body: after the drop there is nothing left to measure, so this
// response is the only place the size of what went is available to a **client**.
// Two more carriers exist and neither is one — the server log line beside the
// drop, and the `addon.data_purged` audit row, which is the **durable** one
// because the log's retention bounds it and nothing keeps this response at all.
// The body also carries what the drop did *not* take, which is the same four
// counts the confirmation put in front of whoever pressed the button.
func (a *AddonAPI) Purge(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, r, domain.ErrNotFound)
		return
	}
	out, err := a.Addons.PurgeData(r.Context(), IdentityFrom(r.Context()), name)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}
