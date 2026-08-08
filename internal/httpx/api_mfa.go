package httpx

import (
	"errors"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// MFAAPI serves the second factor's JSON surface (M53).
//
// Every control the account page draws has an endpoint here, because "every UI
// feature has API support" is an inherited rule and not a preference. Both
// surfaces call the same auth.MFAService methods; nothing in either of them
// decides anything.
//
// **The challenge endpoint is the interesting one.** POST /auth/login answers 401
// with a `mfa-required` problem and a pending token when the account has a second
// factor, and POST /auth/mfa/challenge is where that token is spent. A client that
// has never heard of a second factor therefore fails closed — it gets no session
// cookie and an error it does not recognise — rather than believing it signed in.
type MFAAPI struct {
	MFA    *auth.MFAService
	Config config.Config
}

// writeMFARefused is the one refusal every rejected second factor gets.
//
// Its own writer for the reason writeSignInRefused is one: a wrong code, a spent
// one, a recovery code that does not match and a secret the instance cannot
// decrypt are one answer, and the detail names the recovery code because somebody
// whose phone is gone has no other way to find out that there is a route.
func writeMFARefused(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, Problem{
		Type: problemBase + "invalid-second-factor", Title: "Invalid code",
		Status: http.StatusUnauthorized,
		Detail: "That code is not valid. Codes last about thirty seconds and each one " +
			"works once. If you no longer have your authenticator, use one of your " +
			"recovery codes instead. Repeated failures lock the account for a while.",
	})
}

// writeMFAUnavailable is what an instance with no MFA_SECRET_KEY answers.
//
// **It says which**, unlike most refusals in this product, and the reason is the
// same one the mail-free recovery page gives: an operator who has configured no
// key knows it, a stranger learns nothing they could not learn by looking at the
// account page, and "not available" without the sentence explaining why is advice
// nobody can act on.
func writeMFAUnavailable(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, Problem{
		Type: problemBase + "mfa-unavailable", Title: "Two-factor authentication is unavailable",
		Status: http.StatusServiceUnavailable,
		Detail: "This instance has no MFA_SECRET_KEY configured, so a second factor " +
			"cannot be stored. Ask whoever runs it.",
	})
}

// writeMFAProblem maps the service's refusals onto problem documents.
//
// One place, so the four endpoints below cannot come to disagree about what a
// rejected code looks like — which is the same reasoning writeSignInRefused rests
// on, applied to the surface beside it.
func writeMFAProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrMFAUnavailable):
		writeMFAUnavailable(w, r)
	case errors.Is(err, auth.ErrMFACodeInvalid):
		writeMFARefused(w, r)
	case errors.Is(err, auth.ErrMFAChallengeInvalid):
		WriteProblem(w, r, Problem{
			Type: problemBase + "mfa-challenge-expired", Title: "Sign-in expired",
			Status: http.StatusUnauthorized,
			Detail: "This sign-in can no longer be completed. Start again from the " +
				"sign-in form.",
		})
	case errors.Is(err, auth.ErrMFAAlreadyEnabled):
		WriteProblem(w, r, Problem{
			Type: problemBase + "conflict", Title: "Already enrolled",
			Status: http.StatusConflict,
			Detail: "This account already has a second factor. Turn it off before " +
				"enrolling a new authenticator.",
		})
	case errors.Is(err, auth.ErrMFANotEnabled):
		WriteProblem(w, r, Problem{
			Type: problemBase + "conflict", Title: "No second factor",
			Status: http.StatusConflict,
			Detail: "This account has no second factor.",
		})
	default:
		WriteError(w, r, err)
	}
}

// Status reports whether this account has a second factor.
func (a *MFAAPI) Status(w http.ResponseWriter, r *http.Request) {
	st, err := a.MFA.Status(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		writeMFAProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, mfaStatusBody(st))
}

func mfaStatusBody(st auth.MFAStatus) map[string]any {
	return map[string]any{
		"available":                st.Available,
		"enabled":                  st.Enabled,
		"enabled_at":               st.EnabledAt,
		"recovery_codes_remaining": st.RecoveryCodesRemaining,
	}
}

// Enrol offers a secret and the URI that carries it.
//
// **The QR code is rendered here and not by the client**, through internal/qr,
// which m53.md calls the milestone's one deliberate reuse across work areas: the
// generator built for links serves the second factor for free, as a new call site
// rather than a new capability. The SVG comes back as a string in the body so a
// client has the same picture the dashboard draws without a second request and
// without knowing how to encode one.
func (a *MFAAPI) Enrol(w http.ResponseWriter, r *http.Request) {
	out, err := a.MFA.BeginEnrolment(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		writeMFAProblem(w, r, err)
		return
	}
	svg, err := mfaEnrolmentQR(out.URI)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"secret":  out.Secret,
		"uri":     out.URI,
		"qr_svg":  string(svg),
		"digits":  auth.TOTPDigits,
		"period":  int(auth.TOTPPeriod.Seconds()),
		"issuer":  "",
		"message": "Scan the QR code, or type the secret in by hand, then confirm with a code.",
	})
}

// mfaEnrolmentQR draws the otpauth URI.
//
// The default style, because there is nothing to brand here — this picture is
// looked at once, by one person, for about four seconds. `qr.MaxContent` is 1024
// and an otpauth URI for this product is around 130 bytes, so the bound is
// comfortable rather than close; it is checked by qr.Render regardless, which is
// why a length check here would be a second enumeration of the same limit.
func mfaEnrolmentQR(uri string) ([]byte, error) {
	style, _ := qr.Style{}.Normalize()
	return qr.Render(uri, style)
}

type mfaConfirmRequest struct {
	Secret string `json:"secret"`
	Code   string `json:"code"`
}

// Confirm turns an offered secret into the account's second factor.
//
// The recovery codes come back on this response and on no other, which is what
// "shown once" means when the surface is JSON: nothing stores them in a readable
// form, so a client that discards the body has the regenerate endpoint and
// nothing else.
func (a *MFAAPI) Confirm(w http.ResponseWriter, r *http.Request) {
	var req mfaConfirmRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.MFA.ConfirmEnrolment(r.Context(), IdentityFrom(r.Context()), req.Secret, req.Code)
	if err != nil {
		writeMFAProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"enabled":        true,
		"recovery_codes": out.RecoveryCodes,
	})
}

// RegenerateRecoveryCodes voids the previous set and issues a new one.
func (a *MFAAPI) RegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	codes, err := a.MFA.RegenerateRecoveryCodes(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		writeMFAProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

type mfaDisableRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// Disable takes the second factor away.
//
// The API-key refusal happens in the service, through requireSessionActor, and is
// not duplicated here — unlike ChangePassword's, which predates the service having
// its own. What reaches this handler is the error, and WriteError already maps it.
func (a *MFAAPI) Disable(w http.ResponseWriter, r *http.Request) {
	var req mfaDisableRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.MFA.Disable(r.Context(), IdentityFrom(r.Context()), req.Password, req.Code); err != nil {
		if isCredentialFailure(err) {
			writeSignInRefused(w, r)
			return
		}
		writeMFAProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type mfaChallengeRequest struct {
	Token string `json:"token"`
	Code  string `json:"code"`
}

// Challenge completes a sign-in that stopped at the second factor.
//
// Unauthenticated, necessarily — there is no session yet, which is the whole point
// — and registered under the same login limiter as POST /auth/login, so guessing
// six digits and guessing a password draw on one budget rather than two.
//
// The response is POST /auth/login's, byte for byte, because a client that
// completed a second factor has finished exactly the same operation and should not
// have to parse two shapes of "you are signed in".
func (a *MFAAPI) Challenge(w http.ResponseWriter, r *http.Request) {
	var req mfaChallengeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.MFA.CompleteSecondFactor(r.Context(), req.Token, req.Code,
		ClientIPFrom(r.Context()), r.UserAgent())
	if err != nil {
		if isCredentialFailure(err) {
			writeSignInRefused(w, r)
			return
		}
		writeMFAProblem(w, r, err)
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

// writeSecondFactorRequired is what POST /auth/login answers when the password
// was right and the account has a second factor.
//
// **401 with a problem document, not 200 with a flag.** A client that does not
// know about this milestone treats a 401 as a failed sign-in, which is the correct
// thing for it to do: it has no session and it should not proceed. A 200 carrying
// `{"mfa_required": true}` would be read as success by every client that checks
// the status code, which is most of them.
//
// The token is in the body rather than in a cookie. It is a credential for one
// operation with a five-minute life, and putting it in a cookie would make it
// travel on every request to the origin for the rest of the browser's session.
func writeSecondFactorRequired(w http.ResponseWriter, r *http.Request, p *auth.PendingSecondFactor) {
	WriteProblem(w, r, Problem{
		Type: problemBase + "mfa-required", Title: "Second factor required",
		Status: http.StatusUnauthorized,
		Detail: "This account is protected by two-factor authentication. Post the " +
			"token below to /api/v1/auth/mfa/challenge with a code from your " +
			"authenticator, or with a recovery code.",
		Extra: map[string]any{
			"mfa_token":      p.Token,
			"mfa_expires_at": p.Expires,
		},
	})
}
