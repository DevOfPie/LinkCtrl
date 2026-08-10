package httpx

import (
	"errors"
	"html/template"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// The browser half of the second factor (M53): enrolment, the code prompt that
// stands between a right password and a session, and the controls on the account
// page.
//
// Everything here works with JavaScript switched off. They are ordinary forms
// posting to ordinary paths, which is the constraint the rest of this dashboard is
// built under and the reason there is no htmx in this file.

// --- enrolment --------------------------------------------------------------------

type mfaPageData struct {
	shell
	// Secret is base32, shown in text beside the QR code, because a person
	// enrolling from the same device cannot scan their own screen. It also rides
	// the form as a hidden field: the account row is not touched until a code
	// verifies, so the offer has nowhere else to live — see auth.MFAEnrolment for
	// why that is forced rather than chosen.
	Secret string
	// QR is the otpauth URI drawn by internal/qr, inlined as markup. Safe as
	// template.HTML for the reason internal/qr's package comment gives: the SVG
	// is built from integers and from colours already parsed as `#rrggbb`, so its
	// bytes cannot contain a `<` that did not come from that package.
	QR template.HTML
	// URI is the same string the QR encodes, offered as a link for anybody
	// enrolling on the device the page is open on — tapping it hands the entry
	// straight to an authenticator app with nothing to transcribe.
	//
	// **template.URL, and it has to be.** html/template rewrites an href whose
	// scheme it does not recognise to `#ZgotmplZ`, and `otpauth:` is not one it
	// recognises — so the untyped string renders a dead link rather than a
	// refused one, which is the failure mode nobody notices in review. The value
	// is built by auth.TOTPURI from an escaped label and an encoded query, so
	// what is being asserted by the type is true.
	URI template.URL

	// RecoveryCodes is the set, shown once, on the response that completes an
	// enrolment or a regeneration and never again.
	RecoveryCodes []string
	// Regenerated distinguishes the two reasons codes are on screen, so the page
	// says "here are your codes" rather than "you are now enrolled" when nothing
	// was enrolled.
	Regenerated bool

	Status auth.MFAStatus

	FieldErrors map[string]string
	Error       string
}

func (h *Web) mfaPage(r *http.Request) mfaPageData {
	return mfaPageData{
		shell:       h.shell(r, "Two-factor authentication", "account"),
		FieldErrors: map[string]string{},
	}
}

// Whether the offer can do anything is two separate questions, and both surfaces
// ask them separately rather than through one helper. An unwired service leaves
// every route unregistered, so `h.MFA != nil` decides whether the account page
// draws a section at all; a wired service with no MFA_SECRET_KEY is a different
// state that has to be *said*, and MFAStatus.Available is what says it. Collapsing
// them into one `mfaAvailable()` — the shape recoveryAvailable has — would hide
// the second from the one person who most needs to read it, somebody already
// enrolled on an instance whose key has gone.

// MFAPage draws the enrolment offer, or the enrolled state.
//
// The secret is minted on the GET, so opening the page twice offers two different
// secrets and only the one that gets confirmed becomes real. That is a property of
// nothing being stored: there is no candidate to collide with.
func (h *Web) MFAPage(w http.ResponseWriter, r *http.Request) {
	data := h.mfaPage(r)
	actor := IdentityFrom(r.Context())

	st, err := h.MFA.Status(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return
	}
	data.Status = st

	// Already enrolled, or the instance cannot enrol anybody. Either way there is
	// no offer to draw, and the page says which.
	if st.Enabled || !st.Available {
		h.render(w, r, http.StatusOK, "mfa", data)
		return
	}

	out, err := h.MFA.BeginEnrolment(r.Context(), actor)
	if err != nil {
		h.mfaFail(w, r, data, err)
		return
	}
	// Inlined into the page, so it carries the class that lets a 360px viewport
	// shrink it (F184). The API's own call site passes the empty string.
	svg, err := mfaEnrolmentQR(out.URI, qr.FluidClass)
	if err != nil {
		h.webError(w, r, err)
		return
	}
	data.Secret = out.Secret
	data.URI = template.URL(out.URI) //nolint:gosec // G203: see mfaPageData.URI.
	data.QR = template.HTML(svg)     //nolint:gosec // G203: internal/qr emits integers and parsed colours only; see mfaPageData.QR.
	h.render(w, r, http.StatusOK, "mfa", data)
}

// MFAEnrol confirms an enrolment with a code computed from the offered secret.
//
// A wrong code re-renders the *same* secret rather than minting another, so
// somebody whose phone clock is a few seconds out can try again without rescanning
// — and so the QR on screen does not change under them mid-enrolment.
func (h *Web) MFAEnrol(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	actor := IdentityFrom(r.Context())
	secret := r.PostFormValue("secret")

	out, err := h.MFA.ConfirmEnrolment(r.Context(), actor, secret, r.PostFormValue("code"))
	if err != nil {
		data := h.mfaPage(r)
		if st, serr := h.MFA.Status(r.Context(), actor); serr == nil {
			data.Status = st
		}
		// The offer is put back on the page exactly as it was.
		if secret != "" && !data.Status.Enabled {
			uri := auth.TOTPURI(h.mfaIssuer(), actor.Email, secret)
			data.Secret = secret
			data.URI = template.URL(uri) //nolint:gosec // G203: see mfaPageData.URI.
			if svg, qerr := mfaEnrolmentQR(uri, qr.FluidClass); qerr == nil {
				data.QR = template.HTML(svg) //nolint:gosec // G203: see mfaPageData.QR.
			}
		}
		h.mfaFail(w, r, data, err)
		return
	}

	data := h.mfaPage(r)
	if st, serr := h.MFA.Status(r.Context(), actor); serr == nil {
		data.Status = st
	}
	data.RecoveryCodes = out.RecoveryCodes
	h.render(w, r, http.StatusOK, "mfa", data)
}

// MFARegenerate voids the previous recovery codes and shows a new set.
func (h *Web) MFARegenerate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	actor := IdentityFrom(r.Context())

	codes, err := h.MFA.RegenerateRecoveryCodes(r.Context(), actor)
	if err != nil {
		data := h.mfaPage(r)
		if st, serr := h.MFA.Status(r.Context(), actor); serr == nil {
			data.Status = st
		}
		h.mfaFail(w, r, data, err)
		return
	}

	data := h.mfaPage(r)
	if st, serr := h.MFA.Status(r.Context(), actor); serr == nil {
		data.Status = st
	}
	data.RecoveryCodes = codes
	data.Regenerated = true
	h.render(w, r, http.StatusOK, "mfa", data)
}

// MFADisable takes the second factor away, on the password and a code.
//
// It lands back on /account rather than on this page, because with the factor gone
// there is nothing here to look at: the offer is what remains, and offering it in
// the same breath as removing the thing it offers reads as an undo button.
func (h *Web) MFADisable(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	actor := IdentityFrom(r.Context())

	err := h.MFA.Disable(r.Context(), actor, r.PostFormValue("password"), r.PostFormValue("code"))
	if err != nil {
		data := h.mfaPage(r)
		if st, serr := h.MFA.Status(r.Context(), actor); serr == nil {
			data.Status = st
		}
		h.mfaFail(w, r, data, err)
		return
	}
	seeOther(w, r, "/account?mfa=off")
}

// mfaFail renders the enrolment page carrying a refusal.
//
// One mapping for four handlers, so the browser and the API cannot come to
// disagree about which refusal is which — the same reason writeMFAProblem exists
// on the other surface.
func (h *Web) mfaFail(w http.ResponseWriter, r *http.Request, data mfaPageData, err error) {
	switch {
	case errors.Is(err, auth.ErrMFAUnavailable):
		data.Error = "This instance has no MFA_SECRET_KEY configured, so a second " +
			"factor cannot be stored. Ask whoever runs it."
		h.render(w, r, http.StatusServiceUnavailable, "mfa", data)
	case errors.Is(err, auth.ErrMFACodeInvalid):
		data.FieldErrors["code"] = "That code is not valid. Codes last about thirty " +
			"seconds and each one works once."
		h.render(w, r, http.StatusUnprocessableEntity, "mfa", data)
	case errors.Is(err, auth.ErrInvalidCredentials):
		data.FieldErrors["password"] = "That password is not correct."
		h.render(w, r, http.StatusUnprocessableEntity, "mfa", data)
	case errors.Is(err, auth.ErrMFAAlreadyEnabled):
		data.Error = "This account already has a second factor. Turn it off before " +
			"enrolling a new authenticator."
		h.render(w, r, http.StatusConflict, "mfa", data)
	case errors.Is(err, auth.ErrMFANotEnabled):
		data.Error = "This account has no second factor."
		h.render(w, r, http.StatusConflict, "mfa", data)
	case errors.Is(err, domain.ErrForbidden):
		// requireSessionActor's refusal: an API key is not a person and has no
		// second factor. D87's limb, and m53.md names this as the one it asserts
		// by test.
		h.errorPage(w, r, http.StatusForbidden, "Not allowed",
			"Changing the second factor requires a signed-in session, not an API key.")
	default:
		h.webError(w, r, err)
	}
}

// mfaIssuer is what an authenticator app files the entry under, as this surface
// needs it: the dashboard's own host.
func (h *Web) mfaIssuer() string {
	if u := h.Config.AppBaseURLParsed(); u != nil && u.Host != "" {
		return u.Host
	}
	return "LinkCtrl"
}

// --- the prompt between a password and a session ----------------------------------

type mfaChallengeData struct {
	shell
	// Token is the pending login, carried in a hidden field. Not a cookie: it is
	// a credential for one operation with a five-minute life, and a cookie would
	// send it on every request to the origin for the rest of the browser's
	// session.
	Token string
	Next  string
	Error string
}

// MFAChallengePage draws the code prompt.
//
// Reached only by being redirected here from a completed password post, which
// carries the token. A GET with no token renders the form empty and the POST
// refuses it, rather than this page deciding anything — the token is not validated
// here for the reason ResetPage does not validate its own: doing so would make the
// page an oracle for which tokens exist.
func (h *Web) MFAChallengePage(w http.ResponseWriter, r *http.Request) {
	if IdentityFrom(r.Context()) != nil {
		seeOther(w, r, "/dashboard")
		return
	}
	h.render(w, r, http.StatusOK, "mfa_challenge", mfaChallengeData{
		shell: h.shell(r, "Enter your code", ""),
		Token: r.URL.Query().Get("t"),
		Next:  safeNext(r.URL.Query().Get("next")),
	})
}

// MFAChallengeSubmit completes the sign-in.
//
// Under the same login limiter as POST /login, so guessing six digits and guessing
// a password draw on one budget. The refusals are deliberately few and
// indistinguishable from each other for the reason the sign-in form's are (F92).
func (h *Web) MFAChallengeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	token := r.PostFormValue("token")
	next := safeNext(r.PostFormValue("next"))

	res, err := h.MFA.CompleteSecondFactor(r.Context(), token, r.PostFormValue("code"),
		ClientIPFrom(r.Context()), r.UserAgent())
	if err != nil {
		data := mfaChallengeData{
			shell: h.shell(r, "Enter your code", ""),
			Token: token,
			Next:  next,
		}
		switch {
		case errors.Is(err, auth.ErrMFAChallengeInvalid):
			// Back to the start, because there is nothing on this page to retry
			// with: the pending login is gone, lapsed or spent, and the person
			// needs their password again.
			seeOther(w, r, "/login?expired=1")
		case errors.Is(err, auth.ErrMFACodeInvalid), isCredentialFailure(err):
			// One message for a wrong code and for an account that locked itself
			// out while guessing, the same collapse SignInRefusedDetail makes on
			// the form before this one. Distinguishing them would tell whoever is
			// guessing when they had spent the budget.
			data.Error = "That code is not valid. Codes last about thirty seconds and " +
				"each one works once. If you no longer have your authenticator, type " +
				"one of your recovery codes instead. Repeated failures lock the " +
				"account for a while."
			h.render(w, r, http.StatusUnauthorized, "mfa_challenge", data)
		default:
			h.webError(w, r, err)
		}
		return
	}

	maxAge := int(h.Config.Auth.SessionAbsoluteTTL.Seconds())
	http.SetCookie(w, NewSessionCookie(res.Token, h.Config.SecureCookies, maxAge))
	seeOther(w, r, next)
}
