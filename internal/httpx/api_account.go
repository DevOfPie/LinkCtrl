package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/account"
	"github.com/DevOfPie/LinkCtrl/internal/config"
)

// AccountAPI serves the account's own lifecycle (M52).
//
// One operation, and it is the one the product never had: `users` appeared in
// none of the schema's DELETE statements and `anonymized_at` had no writer, while
// five places described erasure in the present tense (F44).
type AccountAPI struct {
	Accounts *account.Service
	Config   config.Config
}

type deleteAccountRequest struct {
	Password string `json:"password"`
}

// Delete removes the acting account.
//
// **`DELETE` with a body**, which is unusual and is the right shape here. The
// confirmation is the account's own password, and a password does not belong in
// a query string, where it lands in access logs, browser history and referrers.
// RFC 9110 permits a body on DELETE and says only that it has no defined
// semantics; the semantics are defined here.
//
// 204 rather than 200. There is nothing to return: the caller's session is gone
// with the account, and the cookie is expired on the way out for the reason
// Logout expires it unconditionally — a browser that keeps sending a dead
// session gets a 401 on every request until something clears it.
func (a *AccountAPI) Delete(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())

	var req deleteAccountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	// Every refusal this can give — an API key, a wrong password, the instance
	// principal, the sole owner of a surviving organization — is already a
	// domain error the problem writer maps, so there is no per-case branching
	// here. The two conflicts carry their own sentence, which WriteError puts in
	// `detail`, because "which organizations" is the only part of the refusal
	// the caller can act on.
	if err := a.Accounts.Delete(r.Context(), id, req.Password); err != nil {
		WriteError(w, r, err)
		return
	}

	http.SetCookie(w, ClearSessionCookie(a.Config.SecureCookies))
	w.WriteHeader(http.StatusNoContent)
}
