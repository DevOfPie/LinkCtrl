package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/instance"
)

// InstanceAPI exposes the instance-level principal's roster: who holds
// instance-level review, and the two writes that change it.
//
// Authorization is the ordinary permission check in the service — instance.admin
// — with nothing here about which credential the caller used. That permission
// sits in auth.NonDelegableScopes, so no key can ever hold it and these handlers
// never have to ask.
//
// The principal's other surface, the instance-wide audit log, lives on AuditAPI
// beside the organization one: it is the same table, the same page shape and the
// same cursor vocabulary, and splitting it across two files would have been an
// invitation for the two to drift.
type InstanceAPI struct {
	Instance *instance.Service
}

// grantReviewerRequest names an account by address.
//
// An address and nothing else. There is no scope field: what a grant confers is
// enumerated in auth.InstanceGrantable, and a caller who could name the scopes
// could name `instance.admin` — which is exactly the re-delegation D98's bound
// exists to prevent.
type grantReviewerRequest struct {
	Email string `json:"email"`
}

// Reviewers lists who holds instance-level review.
func (a *InstanceAPI) Reviewers(w http.ResponseWriter, r *http.Request) {
	out, err := a.Instance.Reviewers(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

// GrantReviewer confers instance-level review on an account.
func (a *InstanceAPI) GrantReviewer(w http.ResponseWriter, r *http.Request) {
	var req grantReviewerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	out, err := a.Instance.GrantReviewer(r.Context(), IdentityFrom(r.Context()), req.Email)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// 200 rather than 201, because the operation is idempotent and the second
	// call creates nothing: appointing somebody who is already a reviewer is a
	// success that produced no new resource, and answering 201 to it would
	// describe something that did not happen.
	WriteJSON(w, http.StatusOK, out)
}

// RevokeReviewer withdraws it.
func (a *InstanceAPI) RevokeReviewer(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Instance.RevokeReviewer(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
