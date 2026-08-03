package httpx

import (
	"net/http"
)

// Domains over the API (M39).
//
// Thin, like every other handler here: the ownership rule, the hostname syntax
// and the refusals all live in internal/link, so the dashboard forms and a
// bearer token get identical answers by calling the same methods.
//
// **`/domains` is not `/domain`.** The singular endpoint is the instance
// default's settings — where its root sends a stray visitor, and its bot
// policy — and it predates there being more than one domain. The plural is the
// collection: which hostnames this workspace has registered, and the operations
// that add and remove them. They are different resources and neither is a
// version of the other, which is why the older path is left exactly as it was.
//
// **Nothing registered here is served.** A hostname comes back with
// `verified` false and stays that way: verification and serving are M40's, and
// until then no router resolves a Host header at all.
type createDomainRequest struct {
	Hostname string `json:"hostname"`
}

type updateDomainRequest struct {
	Hostname string `json:"hostname"`
}

func (a *LinkAPI) ListDomains(w http.ResponseWriter, r *http.Request) {
	domains, err := a.Links.Domains(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"domains": domains})
}

func (a *LinkAPI) CreateDomain(w http.ResponseWriter, r *http.Request) {
	var req createDomainRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := a.Links.RegisterDomain(r.Context(), IdentityFrom(r.Context()), req.Hostname)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, d)
}

func (a *LinkAPI) UpdateRegisteredDomain(w http.ResponseWriter, r *http.Request) {
	domainID, err := pathUUID(r, "domainID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateDomainRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := a.Links.RenameDomain(r.Context(), IdentityFrom(r.Context()), domainID, req.Hostname)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, d)
}

func (a *LinkAPI) DeleteRegisteredDomain(w http.ResponseWriter, r *http.Request) {
	domainID, err := pathUUID(r, "domainID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.DeleteDomain(r.Context(), IdentityFrom(r.Context()), domainID); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
