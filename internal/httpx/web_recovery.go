package httpx

import (
	"errors"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/recovery"
)

// The browser half of account recovery (M51): the form that asks for a link,
// and the page that link lands on.
//
// Both work with JavaScript switched off. They are ordinary forms posting to
// ordinary paths, which is the constraint the rest of this dashboard is built
// under and the reason there is no htmx in this file.
//
// Both are public by definition — somebody who has lost their password has no
// session — and both are registered under the shared login limiter, so a reset
// sweep and a credential sweep draw on one budget rather than two.

// --- the request form ---------------------------------------------------------

type forgotPageData struct {
	shell
	Form struct{ Email string }
	// Sent is the state after a successful post. It says a message is on its
	// way, and says nothing about whether an account was found — that is the
	// whole enumeration stance in one field.
	Sent bool

	FieldErrors map[string]string
	Error       string
	// NoMailer replaces the form entirely. The one place in the product that
	// refuses rather than degrading when SMTP_HOST is unset, because the mail
	// *is* the mechanism here.
	NoMailer bool
}

func (h *Web) forgotPage(r *http.Request) forgotPageData {
	return forgotPageData{
		shell:       h.shell(r, "Reset your password", ""),
		FieldErrors: map[string]string{},
	}
}

// recoveryAvailable reports whether the reset form can do anything.
//
// An unwired service closes it, which is the direction a missing dependency has
// to fail in; a wired one with no relay draws the honest refusal instead of a
// form.
func (h *Web) recoveryAvailable() bool {
	return h.Recovery != nil && h.Recovery.MailerConfigured()
}

// ForgotPage renders the form that asks for a reset link.
//
// The refusal for a mail-free instance is rendered here, on the GET, rather than
// discovered at submit time — the same shape SignupPage uses, and for the same
// reason: filling in an address and then being told the instance could never
// have sent anything is a worse answer than being told first.
//
// Unlike signup's refusal, this one names the reason. There is nothing to
// protect: an instance with no relay has already said so at the sign-up form,
// and "ask the operator" is useless advice without the sentence explaining why.
func (h *Web) ForgotPage(w http.ResponseWriter, r *http.Request) {
	if IdentityFrom(r.Context()) != nil {
		// Somebody signed in who wants a new password has /account, where the
		// current one is the authority and no mail is involved.
		seeOther(w, r, "/account")
		return
	}
	data := h.forgotPage(r)
	data.NoMailer = !h.recoveryAvailable()
	h.render(w, r, http.StatusOK, "forgot", data)
}

// ForgotSubmit queues a reset link, or the message that says none was created.
//
// One response for every outcome. A found account, an unknown address and an
// account this mechanism refuses all render the same block with the same words,
// and the service spends the same argon2 cost on each — so neither the page nor
// a stopwatch answers whether an address is registered.
func (h *Web) ForgotSubmit(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	data := h.forgotPage(r)
	data.Form.Email = r.PostFormValue("email")

	if _, err := h.Recovery.Request(r.Context(), r.PostFormValue("email")); err != nil {
		switch {
		case errors.Is(err, recovery.ErrNoMailer):
			data.NoMailer = true
			h.render(w, r, http.StatusServiceUnavailable, "forgot", data)
		case errors.Is(err, domain.ErrValidation):
			data.FieldErrors, data.Error = fieldErrors(err)
			h.render(w, r, http.StatusUnprocessableEntity, "forgot", data)
		default:
			h.webError(w, r, err)
		}
		return
	}

	// The address is deliberately not echoed into the confirmation. Signup's
	// equivalent echoes it because a typo is the commonest failure there and
	// nothing is secret about an address somebody just typed; here the same
	// echo would make the page's bytes vary with the input, and the claim being
	// made is that they do not.
	data.Sent = true
	h.render(w, r, http.StatusOK, "forgot", data)
}

// --- the reset link ------------------------------------------------------------

type resetPageData struct {
	shell
	Token string
	// Done is the state after the password has been written. The page says so
	// and points at the sign-in form; it never starts a session.
	Done bool

	FieldErrors map[string]string
	Error       string
}

func (h *Web) resetPage(r *http.Request) resetPageData {
	return resetPageData{
		shell:       h.shell(r, "Choose a new password", ""),
		Token:       r.PathValue("token"),
		FieldErrors: map[string]string{},
	}
}

// ResetPage shows the form the emailed link lands on.
//
// The token is not checked here, for the reason VerifyPage does not check its
// own: doing so would make this page an oracle for which tokens exist, and the
// POST answers for all of them identically anyway. A GET that acted would also
// let a mail client's link scanner spend somebody's reset before they had read
// the message.
func (h *Web) ResetPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, http.StatusOK, "reset", h.resetPage(r))
}

// ResetSubmit writes the new password and ends every session on the account.
//
// It lands on the sign-in form rather than starting a session, and that is a
// choice rather than a limitation: the reset revokes every session for the
// account, so signing this browser straight in would mean the one credential
// the recovery is meant to displace gets replaced by a session created in the
// same breath. Typing the new password once is the confirmation that it is
// known, and it is what the setup and invitation forms get for free by having
// the password in hand.
func (h *Web) ResetSubmit(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	data := h.resetPage(r)
	password := r.PostFormValue("password")
	if password != r.PostFormValue("confirm_password") {
		data.FieldErrors["confirm_password"] = "The passwords do not match."
		h.render(w, r, http.StatusUnprocessableEntity, "reset", data)
		return
	}

	if _, err := h.Recovery.Reset(r.Context(), r.PathValue("token"), password); err != nil {
		switch {
		case errors.Is(err, recovery.ErrNotResettable):
			// 404, and never 410. A consumed link, a lapsed one, a second use, a
			// suspended account and an account with no password all answer here
			// with the same words: 410 would confirm that a token had existed,
			// and a distinct refusal for a suspended account would tell whoever
			// holds the link what state it is in. Saying that is the operator's
			// job.
			data.Error = "This link is no longer valid. It may have been used already, " +
				"or it may have expired. Ask for a new one from the sign-in page."
			h.render(w, r, http.StatusNotFound, "reset", data)
		case errors.Is(err, recovery.ErrNoMailer):
			data.Error = "This instance has no mailer configured, so passwords " +
				"cannot be reset here. Ask whoever runs it."
			h.render(w, r, http.StatusServiceUnavailable, "reset", data)
		case errors.Is(err, domain.ErrValidation):
			data.FieldErrors, data.Error = fieldErrors(err)
			h.render(w, r, http.StatusUnprocessableEntity, "reset", data)
		default:
			h.webError(w, r, err)
		}
		return
	}

	data.Done = true
	h.render(w, r, http.StatusOK, "reset", data)
}
