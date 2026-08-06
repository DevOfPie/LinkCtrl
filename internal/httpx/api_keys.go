package httpx

import (
	"net/http"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// KeyAPI serves /api/v1/api-keys.
//
// Thin, like the other handlers: every rule about who may mint a key and which
// scopes they may grant lives in the service, so the dashboard's key page in
// M11 inherits the same behaviour by calling the same methods.
type KeyAPI struct {
	Keys *auth.APIKeyService
}

type createKeyRequest struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expires_at"`
	OrgWide   bool     `json:"org_wide"`
}

// rotateKeyRequest carries only what a rotation may narrow. There is no id: the
// key being rotated is the key that made the request, and offering a field for
// it would advertise a capability the service refuses.
type rotateKeyRequest struct {
	Scopes []string `json:"scopes"`
	// GraceSeconds is the window during which both secrets verify. Seconds
	// rather than a duration string, because the callers are scripts.
	GraceSeconds *int `json:"grace_seconds"`
}

// Create issues a key. The response carries the token, and it is the only
// response that ever will.
func (a *KeyAPI) Create(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	in := auth.CreateAPIKeyInput{Name: req.Name, Scopes: req.Scopes, OrgWide: req.OrgWide}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		at, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			WriteError(w, r, domain.ValidationErrors{{
				Field: "expires_at", Code: "invalid",
				Message: "expiry must be an RFC 3339 timestamp, or omitted for a key that never expires",
			}})
			return
		}
		in.ExpiresAt = &at
	}

	created, err := a.Keys.Create(r.Context(), IdentityFrom(r.Context()), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, created)
}

func (a *KeyAPI) List(w http.ResponseWriter, r *http.Request) {
	keys, err := a.Keys.List(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": keys})
}

// Rotate replaces the key that made the request.
//
// The route is `POST /api-keys/rotate` and not `/api-keys/{id}/rotate`, which
// is the API saying out loud what the service enforces: a key rotates itself.
// An id in the path would read as "name the key to rotate", and every caller
// who tried would get a 403 for asking the question the URL invited.
func (a *KeyAPI) Rotate(w http.ResponseWriter, r *http.Request) {
	// The body is optional, unlike every other write in this API. "Rotate me,
	// same scopes, default window" is the call an unattended deployment makes,
	// and making it send `{}` to say nothing would be a wart in every script
	// that ever rotates a credential.
	var req rotateKeyRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}

	in := auth.RotateAPIKeyInput{Scopes: req.Scopes}
	if req.GraceSeconds != nil {
		in.Grace = rotateGrace(*req.GraceSeconds)
	}

	rotated, err := a.Keys.Rotate(r.Context(), IdentityFrom(r.Context()), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, rotated)
}

// rotateGrace converts the request's grace_seconds into the duration the
// service's band check judges. The band itself — floor, ceiling, zero-means-
// default — lives in internal/auth and is not repeated here; saturating at the
// ceiling is what keeps that true for a count so large the multiplication
// would wrap, which could otherwise land inside the band or at the exact zero
// that means "use the default".
func rotateGrace(seconds int) time.Duration {
	return saturateDuration(int64(seconds), time.Second, auth.MaxRotationGrace)
}

func (a *KeyAPI) Revoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Keys.Revoke(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
