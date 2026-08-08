package httpx

import (
	"errors"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
)

// AuthAPI serves the JSON authentication endpoints.
//
// The API surface comes first, before the HTML forms in M11, because Plan.md
// makes "every UI feature has API support" a success criterion. Building the
// form first and retrofitting an endpoint is how that criterion gets broken;
// the forms will post to these same service calls.
type AuthAPI struct {
	Auth *auth.Service
	// Signup owns registration since M29: whether this instance accepts one at
	// all, and the two halves of an accepted one. Nil refuses every
	// registration, which is the direction an unwired dependency has to fail in.
	Signup *signup.Service
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

// Register starts a public registration, subject to the instance's effective
// signup mode.
//
// `invite` is not open registration and never was: it admits accounts through
// POST /invitations/redeem, where an administrator named the address first. This
// endpoint is *public* signup, so anything but `open` refuses here.
//
// 202 rather than 201, and the change is the milestone's point. Nothing exists
// when this returns — no user, no organization, no workspace — because under D1
// the address is proven before the account is usable, and the strongest form of
// that is an account which does not exist yet. The verification link creates it.
func (a *AuthAPI) Register(w http.ResponseWriter, r *http.Request) {
	if a.Signup == nil {
		writeSignupClosed(w, r)
		return
	}

	var req credentials
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	out, err := a.Signup.Register(r.Context(), signup.RegisterInput{
		Email: req.Email, Name: req.Name, Password: req.Password,
	})
	if err != nil {
		if errors.Is(err, signup.ErrClosed) {
			writeSignupClosed(w, r)
			return
		}
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"email":      out.Email,
		"status":     "verification_sent",
		"expires_at": out.ExpiresAt,
	})
}

// writeSignupClosed is the one refusal both registration surfaces give when the
// effective mode is not `open`.
//
// It says nothing about *why* — whether `LINKCTRL_SIGNUP_MODE` is lower or
// there is no mailer to verify an address with. Both are the operator's
// business and neither is a stranger's, and distinguishing them would describe
// how the instance is configured to whoever asked.
func writeSignupClosed(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, Problem{
		Type: problemBase + "signup-closed", Title: "Registration is closed",
		Status: http.StatusForbidden,
		Detail: "This instance does not accept public registration.",
	})
}

// SignInRefusedDetail is the body every failed sign-in gets, on both surfaces.
//
// One string rather than two so the API and the form cannot drift into saying
// different things, which is half of what finding F92 was: the API's answer and
// the browser's prose disagreed about whether an account existed, and fixing
// either alone leaves the other one answering.
//
// **The lockout is named unconditionally**, and that is the point of the second
// sentence rather than a hedge. It is identical whether or not the address is
// registered and whether or not a lockout is in force, so it discloses nothing —
// while somebody certain they typed their own password correctly is told why
// waiting is the thing that helps. Without it, the price of closing the oracle is
// that a real user spends their own lockout being told they cannot type.
const SignInRefusedDetail = "The email or password is incorrect. Repeated failures " +
	"lock an account for a while; if this one is yours, wait a few minutes before " +
	"trying again."

// writeSignInRefused is the one refusal every sign-in failure gets.
//
// Its own writer rather than the generic mapping in WriteError because the detail
// above names the lockout, and `invalid-credentials` also answers
// POST /api/v1/auth/password — where nothing locks and the sentence would be a lie.
// The status, type and title are exactly what WriteError produces for all three
// errors, so a caller that has never read this cannot tell which wrote it.
func writeSignInRefused(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, Problem{
		Type: problemBase + "invalid-credentials", Title: "Invalid credentials",
		Status: http.StatusUnauthorized,
		Detail: SignInRefusedDetail,
	})
}

// isCredentialFailure reports whether err is one of the sign-in refusals that
// must be indistinguishable from each other (F92).
func isCredentialFailure(err error) bool {
	return errors.Is(err, auth.ErrInvalidCredentials) ||
		errors.Is(err, auth.ErrAccountInactive) ||
		errors.Is(err, auth.ErrAccountLocked)
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
		if isCredentialFailure(err) {
			writeSignInRefused(w, r)
			return
		}
		WriteError(w, r, err)
		return
	}

	// The second factor (M53). No cookie is set and no session exists: what comes
	// back is a 401 carrying the pending token, so a client that has never heard
	// of this refuses to proceed rather than believing it signed in.
	if res.SecondFactorRequired() {
		writeSecondFactorRequired(w, r, res.Pending)
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

	// Not with an API key. Changing the password revokes every other session,
	// so allowing it from a token would let a leaked key lock its owner out of
	// their own account.
	if id.IsAPIKey() {
		WriteProblem(w, r, Problem{
			Type: problemBase + "forbidden", Title: "Forbidden",
			Status: http.StatusForbidden,
			Detail: "Changing a password requires a signed-in session, not an API key.",
		})
		return
	}

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
