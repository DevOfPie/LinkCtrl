package httpx

import (
	"errors"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/signup"
)

// The browser half of self-serve signup: the public form and the verification
// link it mails. There is no page for choosing the mode, because the mode is
// `LINKCTRL_SIGNUP_MODE` and nothing reachable from a browser changes it (D38).
//
// Both work with JavaScript switched off. They are ordinary forms posting to
// ordinary paths, which is the same constraint the rest of this dashboard is
// built under and the reason there is no htmx in this file.

// --- the public form ---------------------------------------------------------

type signupPageData struct {
	shell
	Form struct{ Email, Name string }
	// Sent is the state after a successful post: nothing exists yet, and the
	// page says which inbox the link went to.
	Sent  bool
	Email string

	FieldErrors map[string]string
	Error       string
}

func (h *Web) signupPage(r *http.Request) signupPageData {
	return signupPageData{
		shell:       h.shell(r, "Create an account", ""),
		FieldErrors: map[string]string{},
	}
}

// signupOpen reports whether the browser form should be reachable at all.
//
// An unwired service closes the form, which is the direction a missing
// dependency has to fail in.
func (h *Web) signupOpen() bool {
	return h.Signup != nil && h.Signup.Effective() == signup.Open
}

// SignupPage renders the public signup form.
//
// A closed instance gets one refusal, on the GET, with no explanation of which
// of the two reasons applies — the mode, or the missing mailer. Refusing here
// rather than at the post is what keeps somebody from filling in a password and
// discovering at submit time that there was never a form to submit; which of the
// two bounds it is remains the operator's business and not a stranger's.
func (h *Web) SignupPage(w http.ResponseWriter, r *http.Request) {
	if IdentityFrom(r.Context()) != nil {
		seeOther(w, r, "/dashboard")
		return
	}
	if !h.signupOpen() {
		h.errorPage(w, r, http.StatusForbidden, "Sign-ups are closed",
			"This instance does not accept public registration. If you were expecting "+
				"to join an organization, ask whoever runs it for an invitation.")
		return
	}
	h.render(w, r, http.StatusOK, "signup", h.signupPage(r))
}

// SignupSubmit takes the form and queues a verification link.
//
// No session is created and no account exists when this returns. That is the
// difference from the setup form, which signs its user straight in: there the
// person had already proven they could reach the machine, and here the address
// is the thing being proven (D1).
func (h *Web) SignupSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.signupOpen() {
		h.errorPage(w, r, http.StatusForbidden, "Sign-ups are closed",
			"This instance does not accept public registration.")
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	data := h.signupPage(r)
	data.Form.Email = r.PostFormValue("email")
	data.Form.Name = r.PostFormValue("name")

	out, err := h.Signup.Register(r.Context(), signup.RegisterInput{
		Email:    r.PostFormValue("email"),
		Name:     r.PostFormValue("name"),
		Password: r.PostFormValue("password"),
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrValidation):
			data.FieldErrors, data.Error = fieldErrors(err)
			h.render(w, r, http.StatusUnprocessableEntity, "signup", data)
		// No ErrEmailTaken branch, deliberately. Register does not return it any
		// more: a taken address gets the same 202 and the same page as a free
		// one, and the answer goes to the address by mail (F13). A branch here
		// would be unreachable, and an unreachable branch asserting the opposite
		// of the behaviour is what the next reader would believe. The text it
		// used to render also offered a password reset, which this product has
		// never had (F141).
		case errors.Is(err, signup.ErrClosed):
			h.errorPage(w, r, http.StatusForbidden, "Sign-ups are closed",
				"This instance does not accept public registration.")
		default:
			h.webError(w, r, err)
		}
		return
	}

	data.Sent, data.Email = true, out.Email
	h.render(w, r, http.StatusOK, "signup", data)
}

// --- the verification link ---------------------------------------------------

type verifyPageData struct {
	shell
	Token string
	// Error is set for every refusal. Success never renders this page — it ends
	// at the sign-in form with the account already made.
	Error string
}

// VerifyPage shows the confirmation the emailed link lands on.
//
// A page with a button rather than a link that acts, for the reason invitation
// redemption is a POST: mail clients and security scanners fetch the URLs in a
// message, and a GET that created an account would let a scanner finish somebody
// else's registration before they had read the mail.
//
// The token is not checked here. Doing so would make this page an oracle for
// which tokens exist, and the POST answers for all of them identically anyway.
func (h *Web) VerifyPage(w http.ResponseWriter, r *http.Request) {
	data := verifyPageData{
		shell: h.shell(r, "Confirm your address", ""),
		Token: r.PathValue("token"),
	}
	h.render(w, r, http.StatusOK, "verify", data)
}

// VerifySubmit completes the registration, creating the account. It is the only
// place in the product where an account comes into being with its address
// already proven.
//
// It ends at the sign-in form rather than starting a session, and that is a
// consequence of the design rather than a choice made here: the password was
// hashed at the signup form and the plaintext never survived that request, so
// there is nothing to sign in with. The setup and invitation forms do sign
// somebody straight in, and both of them had the password in hand.
func (h *Web) VerifySubmit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	if _, err := h.Signup.Verify(r.Context(), token); err != nil {
		data := verifyPageData{
			shell: h.shell(r, "Confirm your address", ""),
			Token: token,
		}
		switch {
		case errors.Is(err, signup.ErrNotVerifiable):
			data.Error = "This link is no longer valid. It may have been used already, " +
				"or it may have expired. Start again from the sign-up form."
			h.render(w, r, http.StatusNotFound, "verify", data)
		case errors.Is(err, signup.ErrClosed):
			data.Error = "This instance has stopped accepting sign-ups since you started."
			h.render(w, r, http.StatusForbidden, "verify", data)
		case errors.Is(err, auth.ErrEmailTaken):
			data.Error = "That address already has an account. Sign in instead."
			h.render(w, r, http.StatusConflict, "verify", data)
		default:
			h.webError(w, r, err)
		}
		return
	}
	seeOther(w, r, "/login?verified=1")
}
