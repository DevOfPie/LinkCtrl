package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// AuthAPI serves the JSON authentication endpoints.
//
// The API surface comes first, before the HTML forms in M11, because Plan.md
// makes "every UI feature has API support" a success criterion. Building the
// form first and retrofitting an endpoint is how that criterion gets broken;
// the forms will post to these same service calls.
type AuthAPI struct {
	Auth   *auth.Service
	Config config.Config
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

// Setup creates the first user. Available only while no users exist, so a
// fresh instance can be claimed without the operator editing the database, and
// closed permanently the moment it is used.
func (a *AuthAPI) Setup(w http.ResponseWriter, r *http.Request) {
	needs, err := a.Auth.NeedsSetup(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !needs {
		// 404 rather than 403: the endpoint genuinely does not exist any more,
		// and saying so does not confirm anything about the instance.
		WriteError(w, r, domain.ErrNotFound)
		return
	}

	var req credentials
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	id, err := a.Auth.Register(r.Context(), auth.RegisterInput{
		Email: req.Email, Name: req.Name, Password: req.Password, IsFirstUser: true,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{
		"user_id": id.UserID, "email": id.Email, "workspace_id": id.WorkspaceID,
	})
}

// Register creates an account, subject to SIGNUP_MODE.
func (a *AuthAPI) Register(w http.ResponseWriter, r *http.Request) {
	if a.Config.Auth.SignupMode != config.SignupOpen {
		// Invite mode is Phase 2; until then anything but open is closed.
		WriteProblem(w, r, Problem{
			Type: problemBase + "signup-closed", Title: "Registration is closed",
			Status: http.StatusForbidden,
			Detail: "This instance does not accept public registration.",
		})
		return
	}

	var req credentials
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	id, err := a.Auth.Register(r.Context(), auth.RegisterInput{
		Email: req.Email, Name: req.Name, Password: req.Password,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{
		"user_id": id.UserID, "email": id.Email, "workspace_id": id.WorkspaceID,
	})
}

func (a *AuthAPI) Login(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	res, err := a.Auth.Login(r.Context(), auth.LoginInput{
		Email:     req.Email,
		Password:  req.Password,
		IP:        ClientIPFrom(r.Context()),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}

	maxAge := int(a.Config.Auth.SessionAbsoluteTTL.Seconds())
	http.SetCookie(w, NewSessionCookie(res.Token, a.Config.SecureCookies, maxAge))

	WriteJSON(w, http.StatusOK, map[string]any{
		"user_id":      res.Identity.UserID,
		"email":        res.Identity.Email,
		"workspace_id": res.Identity.WorkspaceID,
		"role":         res.Identity.Role,
		"expires_at":   res.Expires,
	})
}

func (a *AuthAPI) Logout(w http.ResponseWriter, r *http.Request) {
	if id := IdentityFrom(r.Context()); id != nil {
		if err := a.Auth.Logout(r.Context(), id.SessionID); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	// Cleared unconditionally, so a stale or unparseable cookie is dropped
	// rather than resent on every future request.
	http.SetCookie(w, ClearSessionCookie(a.Config.SecureCookies))
	w.WriteHeader(http.StatusNoContent)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (a *AuthAPI) ChangePassword(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())

	var req changePasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	if err := a.Auth.ChangePassword(r.Context(), id.UserID, id.SessionID,
		req.CurrentPassword, req.NewPassword); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
