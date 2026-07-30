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
}

// Create issues a key. The response carries the token, and it is the only
// response that ever will.
func (a *KeyAPI) Create(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	in := auth.CreateAPIKeyInput{Name: req.Name, Scopes: req.Scopes}
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
