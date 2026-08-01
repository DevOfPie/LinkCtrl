package httpx

import (
	"net/http"
)

// Theme choices. "system" is the absence of a choice rather than a third
// appearance, and is stored by clearing the cookie.
const (
	ThemeSystem = "system"
	ThemeLight  = "light"
	ThemeDark   = "dark"
)

// themeCookieName is unprefixed on purpose.
//
// The session cookie carries __Host-, which requires Secure and so cannot be
// set over plain HTTP. That is right for a credential and wrong for this: an
// appearance preference has to work on a local HTTP instance, and the worst a
// forged one can do is show somebody the other theme.
const themeCookieName = "linkctrl_theme"

// themeCookieMaxAge is a year. A preference that expires is a preference the
// person has to set again for no reason they can see.
const themeCookieMaxAge = 365 * 24 * 60 * 60

// themeFrom returns the explicit override, or "" when the visitor has expressed
// nothing and `prefers-color-scheme` should decide.
//
// Anything unrecognised is treated as no preference rather than rejected. The
// value comes from a cookie, so a stale one from a future version, or a hand-
// edited one, must degrade to the system default instead of breaking the page.
func themeFrom(r *http.Request) string {
	c, err := r.Cookie(themeCookieName)
	if err != nil {
		return ""
	}
	switch c.Value {
	case ThemeLight, ThemeDark:
		return c.Value
	default:
		return ""
	}
}

// setTheme writes or clears the preference cookie.
//
// HttpOnly because nothing in the browser reads it — the attribute is rendered
// by the server, which is the whole point of the design. There is no script to
// deny.
func setTheme(w http.ResponseWriter, secure bool, theme string) {
	c := &http.Cookie{ //nolint:gosec // G124: Secure is config-driven, and an appearance preference must work on a local HTTP instance
		Name:     themeCookieName,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
	switch theme {
	case ThemeLight, ThemeDark:
		c.Value = theme
		c.MaxAge = themeCookieMaxAge
	default:
		// System: store nothing. An absent cookie and "follow the system" are
		// the same state, so there is no way for them to disagree.
		c.Value = ""
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// ThemeSet stores the visitor's choice and returns them where they were.
//
// A plain form POST with no JavaScript, and no account: the preference is
// per-browser by design. Two browsers signed into one account may disagree,
// which is correct — the person at each of them chose.
func (h *Web) ThemeSet(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	setTheme(w, h.Config.SecureCookies, r.PostFormValue("theme"))

	// Back to the page the control was on, which is why the form carries it:
	// the control renders on account settings and on the sign-in page, so the
	// POST arrives from either, and Referer is not something to route on.
	seeOther(w, r, safeNext(r.PostFormValue("next")))
}
