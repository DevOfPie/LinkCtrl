package httpx

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

type ctxKey int

const (
	ctxIdentity ctxKey = iota
	ctxClientIP
)

// IdentityFrom returns the authenticated identity, or nil.
func IdentityFrom(ctx context.Context) *auth.Identity {
	id, _ := ctx.Value(ctxIdentity).(*auth.Identity)
	return id
}

// ClientIPFrom returns the resolved client address.
func ClientIPFrom(ctx context.Context) netip.Addr {
	addr, _ := ctx.Value(ctxClientIP).(netip.Addr)
	return addr
}

// RealIP resolves the client address, honouring X-Forwarded-For only from
// trusted proxies.
//
// The trust list defaults to empty, and that default is the important part. A
// service that believes X-Forwarded-For unconditionally lets any client claim
// any address, which defeats rate limiting and corrupts analytics — and the
// mistake is invisible until someone abuses it.
func RealIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			addr := directAddr(r)

			if len(trusted) > 0 && isTrusted(addr, trusted) {
				if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
					// Right to left, taking the last address not itself a
					// trusted proxy. Reading left to right would take the
					// client-supplied value, which is trivially forged.
					parts := strings.Split(fwd, ",")
					for i := len(parts) - 1; i >= 0; i-- {
						candidate, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
						if err != nil {
							continue
						}
						if !isTrusted(candidate, trusted) {
							addr = candidate
							break
						}
					}
				}
			}

			ctx := context.WithValue(r.Context(), ctxClientIP, addr)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func directAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Authenticator resolves credentials to an identity.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*auth.Identity, error)
}

// Session attaches an identity when a valid session cookie is present.
//
// It never rejects: an anonymous request continues with no identity, and
// RequireAuth decides. Splitting the two keeps endpoints that behave
// differently for signed-in users from needing a second lookup.
func Session(a Authenticator, secure bool) func(http.Handler) http.Handler {
	name := auth.CookieName(secure)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(name)
			if err == nil && cookie.Value != "" {
				if id, err := a.Authenticate(r.Context(), cookie.Value); err == nil {
					r = r.WithContext(context.WithValue(r.Context(), ctxIdentity, id))
				} else {
					// A cookie that no longer resolves is cleared, so the
					// browser stops sending it on every subsequent request.
					http.SetCookie(w, ClearSessionCookie(secure))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAuth rejects requests with no identity.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IdentityFrom(r.Context()) == nil {
			WriteError(w, r, domain.ErrUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewSessionCookie builds the session cookie.
//
// The __Host- prefix requires Secure, Path=/ and no Domain, and browsers
// enforce that: a cookie with the prefix and any of those wrong is silently
// discarded. SameSite=Lax rather than Strict so following a link to the
// dashboard from elsewhere does not appear signed out, which users read as a
// bug.
func NewSessionCookie(token string, secure bool, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     auth.CookieName(secure),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// ClearSessionCookie expires the session cookie. Attributes must match the
// original or the browser will not replace it.
func ClearSessionCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     auth.CookieName(secure),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// SecurityHeaders sets the defensive headers every response carries.
func SecurityHeaders(cfg config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), interest-cohort=()")
			if cfg.AppEnv.IsProduction() {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
