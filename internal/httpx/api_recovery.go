package httpx

import (
	"errors"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/recovery"
)

// RecoveryAPI is account recovery, as a program sees it (M51).
//
// It exists because *every UI feature has API support* is an inherited rule and
// this is not an exception to it: the browser forms in web_recovery.go post at
// the same two service calls, so a client can ask for a link and spend one
// exactly as a person can.
//
// Both endpoints are unauthenticated, necessarily — the caller has lost the only
// credential they had — and both are registered under the same login limiter as
// POST /auth/login, so an attacker cannot double their budget by alternating
// between recovery and sign-in.
//
// **There is no GET half.** The browser's two GETs draw forms; a JSON client has
// no form to draw, and a GET that answered anything about a token would be the
// enumeration oracle both surfaces are built to avoid.
type RecoveryAPI struct {
	Recovery *recovery.Service
}

type forgotRequest struct {
	Email string `json:"email"`
}

// Forgot asks for a reset link.
//
// **202 for every address**, with the same body and the same argon2 cost,
// whether or not the address has an account and whether or not that account can
// be recovered. The answer goes to the address by mail, which is the stance
// registration already takes (F13): the channel that proves an address exists is
// the mailbox, and a status code reaches whoever typed the address into the
// form.
//
// 503 is the one honest exception, and it is about the instance rather than the
// address: with no `SMTP_HOST` there is no mechanism at all, and answering 202
// to a caller whose instance can send nothing would be the silent success this
// milestone exists to refuse.
func (a *RecoveryAPI) Forgot(w http.ResponseWriter, r *http.Request) {
	var req forgotRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	if _, err := a.Recovery.Request(r.Context(), req.Email); err != nil {
		if errors.Is(err, recovery.ErrNoMailer) {
			writeNoMailer(w, r)
			return
		}
		WriteError(w, r, err)
		return
	}
	// The echoed address is the request's own, normalized by the service and
	// deliberately not returned: a body that varied with what was found would
	// undo the equality above. `status` is a constant, and that is the point.
	WriteJSON(w, http.StatusAccepted, map[string]any{"status": "reset_requested"})
}

type resetRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// Reset spends a token and writes the new password.
//
// 204 rather than a session. The reset revokes every session on the account, so
// answering with one would replace the credential being displaced in the same
// breath; the caller signs in with the password they just set, which is also the
// proof they know it.
//
// The token travels in the body and not in the path, unlike the browser's route.
// A URL is written to the access log, the referrer header and the client's own
// history by default, and this one sets a password — the browser has no choice
// because a link in a mail is a URL, and a JSON client has no such excuse. It is
// the shape POST /invitations/redeem already uses for the same kind of secret.
func (a *RecoveryAPI) Reset(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	if _, err := a.Recovery.Reset(r.Context(), req.Token, req.NewPassword); err != nil {
		switch {
		case errors.Is(err, recovery.ErrNotResettable):
			// Not found, and never gone. Consumed, expired, used twice, suspended
			// and password-less all answer identically here.
			//
			// Its own problem type rather than the generic `not-found`, for the
			// reason redemption has `invitation-not-redeemable`: a client that
			// wants to tell "wrong URL" from "spent link" can, and one that does
			// not still reads the same 404.
			WriteProblem(w, r, Problem{
				Type: problemBase + "reset-not-valid", Title: "Reset link is not valid",
				Status: http.StatusNotFound,
				Detail: "This reset link is not valid. It may have expired, been " +
					"used already, or the account may no longer be recoverable.",
			})
		case errors.Is(err, recovery.ErrNoMailer):
			writeNoMailer(w, r)
		default:
			WriteError(w, r, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeNoMailer is the refusal both recovery endpoints give on an instance with
// no relay configured.
//
// 503 rather than 403 or 404, because the mechanism exists and is unavailable
// rather than forbidden or absent: a client that retries after the operator
// configures a relay will succeed, and that is exactly what 503 promises.
//
// It names the reason, unlike writeSignupClosed. There is nothing to protect —
// an instance with no mailer already says so at the sign-up form — and "ask the
// operator" without the sentence explaining why is advice nobody can act on.
func writeNoMailer(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, Problem{
		Type: problemBase + "no-mailer", Title: "No mailer is configured",
		Status: http.StatusServiceUnavailable,
		Detail: "This instance has no SMTP relay configured, so it cannot send a " +
			"password reset. Whoever operates it can configure one, or set the " +
			"password directly in the database.",
	})
}
