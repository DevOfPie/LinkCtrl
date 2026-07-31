package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// Problem is an RFC 9457 problem detail.
//
// One representation for every API error, so a client can branch on `type`
// rather than on prose, and field-level failures arrive in a shape a form can
// render directly.
type Problem struct {
	Type     string              `json:"type"`
	Title    string              `json:"title"`
	Status   int                 `json:"status"`
	Detail   string              `json:"detail,omitempty"`
	Instance string              `json:"instance,omitempty"`
	Errors   []domain.FieldError `json:"errors,omitempty"`
}

const problemBase = "https://linkctrl.dev/problems/"

// WriteProblem emits a problem document.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteError maps a service error to a problem document.
//
// The mapping lives here and only here, which is what lets services return
// domain sentinels without knowing anything about HTTP. It is also the single
// place that decides what a client is allowed to learn: unrecognised errors
// become a flat 500 with the detail logged rather than returned, because an
// internal error string can carry table names, query fragments, or a DSN.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		return

	case errors.Is(err, domain.ErrValidation):
		p := Problem{
			Type:   problemBase + "validation-failed",
			Title:  "Validation failed",
			Status: http.StatusUnprocessableEntity,
			Detail: "One or more fields are invalid.",
		}
		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			p.Errors = ve
		}
		WriteProblem(w, r, p)

	case errors.Is(err, domain.ErrNotFound):
		WriteProblem(w, r, Problem{
			Type: problemBase + "not-found", Title: "Not found",
			Status: http.StatusNotFound,
		})

	case errors.Is(err, domain.ErrConflict):
		WriteProblem(w, r, Problem{
			Type: problemBase + "conflict", Title: "Conflict",
			Status: http.StatusConflict, Detail: err.Error(),
		})

	case errors.Is(err, domain.ErrForbidden):
		// The detail names the missing permission. That is deliberate: the
		// caller is already authenticated, so telling them which permission
		// they lack is useful rather than a disclosure.
		WriteProblem(w, r, Problem{
			Type: problemBase + "forbidden", Title: "Forbidden",
			Status: http.StatusForbidden, Detail: err.Error(),
		})

	case errors.Is(err, domain.ErrUnauthorized),
		errors.Is(err, auth.ErrSessionNotFound),
		errors.Is(err, auth.ErrSessionExpired),
		errors.Is(err, auth.ErrSessionRevoked):
		WriteProblem(w, r, Problem{
			Type: problemBase + "unauthorized", Title: "Authentication required",
			Status: http.StatusUnauthorized,
		})

	case errors.Is(err, auth.ErrAPIKeyInvalid):
		// One response for malformed, unknown, revoked and expired keys. The
		// owner can see which of their keys is which in the key list, and
		// whoever found a leaked one learns nothing about whether it is worth
		// trying elsewhere.
		WriteProblem(w, r, Problem{
			Type: problemBase + "invalid-api-key", Title: "Invalid API key",
			Status: http.StatusUnauthorized,
			Detail: "The API key is unknown, revoked, expired, or malformed.",
		})

	case errors.Is(err, auth.ErrInvalidCredentials):
		WriteProblem(w, r, Problem{
			Type: problemBase + "invalid-credentials", Title: "Invalid credentials",
			Status: http.StatusUnauthorized,
			// Same message whatever the cause; see auth.Login.
			Detail: "The email or password is incorrect.",
		})

	case errors.Is(err, auth.ErrAccountInactive):
		// Deliberately identical to the invalid-credentials response, and not
		// its own type. Login already refuses to say whether an address is
		// registered; telling an unauthenticated caller "this account exists
		// but is suspended" gives back exactly what that refusal withholds.
		// Unmapped, this returned a 500 and an error-level log line per
		// attempt, which is both a worse answer and a louder oracle.
		WriteProblem(w, r, Problem{
			Type: problemBase + "invalid-credentials", Title: "Invalid credentials",
			Status: http.StatusUnauthorized,
			Detail: "The email or password is incorrect.",
		})

	case errors.Is(err, auth.ErrAccountLocked):
		WriteProblem(w, r, Problem{
			Type: problemBase + "account-locked", Title: "Account temporarily locked",
			Status: http.StatusTooManyRequests,
			Detail: "Too many failed sign-in attempts. Try again shortly.",
		})

	case errors.Is(err, auth.ErrEmailTaken):
		WriteProblem(w, r, Problem{
			Type: problemBase + "conflict", Title: "Conflict",
			Status: http.StatusConflict,
			Detail: "That email address is already registered.",
		})

	case errors.Is(err, auth.ErrInvalidEmail):
		// Shaped like every other field failure, so a client's form-error
		// handling covers it. Unmapped, this fell through to a 500 — found by
		// the OpenAPI contract test, which is the kind of thing it is for.
		WriteProblem(w, r, Problem{
			Type:   problemBase + "validation-failed",
			Title:  "Validation failed",
			Status: http.StatusUnprocessableEntity,
			Errors: []domain.FieldError{{
				Field: "email", Code: "invalid",
				Message: "that does not look like an email address",
			}},
		})

	case errors.Is(err, domain.ErrNotImplemented):
		WriteProblem(w, r, Problem{
			Type: problemBase + "not-implemented", Title: "Not implemented",
			Status: http.StatusUnprocessableEntity, Detail: err.Error(),
		})

	case errors.Is(err, context.DeadlineExceeded):
		// The per-request deadline fired. A gateway timeout rather than a 500,
		// because the request may well succeed on a retry and the caller can act
		// on the difference — which is the whole test for whether a status code
		// is the right one.
		WriteProblem(w, r, Problem{
			Type: problemBase + "timeout", Title: "Request timed out",
			Status: http.StatusGatewayTimeout,
			Detail: "The server took too long to answer. Retry, and narrow the request if it persists.",
		})

	case errors.Is(err, context.Canceled):
		// The client went away. Nothing is going to read this response, so the
		// only thing that matters is not calling it an internal error: logging a
		// disconnect as a server fault buries real 500s, and counting it as one
		// makes the 5xx alert fire for people closing tabs.
		//
		// 499 is nginx's convention rather than an IANA code, chosen because it
		// classifies as 4xx in the metrics, which is where a disconnect belongs.
		WriteProblem(w, r, Problem{
			Type: problemBase + "client-closed", Title: "Client closed request",
			Status: 499,
		})

	default:
		// Log the real error, return nothing about it. An unmapped error can
		// carry a query fragment, a column name, or a connection string.
		if r != nil {
			observability.LoggerFrom(r.Context()).Error("unhandled error",
				slog.Any("error", err), slog.String("path", r.URL.Path))
		}
		WriteProblem(w, r, Problem{
			Type: problemBase + "internal", Title: "Internal server error",
			Status: http.StatusInternalServerError,
		})
	}
}

// WriteJSON emits a success response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// AsError is errors.As with the argument order handlers read more naturally.
func AsError(err error, target any) bool { return errors.As(err, target) }
