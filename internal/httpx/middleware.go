package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

type ctxKey int

const ctxIdentity ctxKey = iota

// IdentityFrom returns the authenticated identity, or nil.
func IdentityFrom(ctx context.Context) *auth.Identity {
	id, _ := ctx.Value(ctxIdentity).(*auth.Identity)
	return id
}

// ClientIPFrom returns the resolved client address.
//
// The value itself is carried by internal/auth, so the service layer can read
// it without importing the HTTP layer — an audit event records the network a
// change came from, and the services that write those events are below this
// package. This stays as the name the handlers here already use.
func ClientIPFrom(ctx context.Context) netip.Addr {
	return auth.ClientIPFrom(ctx)
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
				// Values, not Get: some proxies (HAProxy among them) append the
				// client as a NEW header line rather than extending the first,
				// and Get returns only the first line — which is the one the
				// client sent. Joining every occurrence in order reconstructs
				// the full hop list; the rightmost entries are the ones the
				// trusted proxy wrote.
				if fwd := strings.Join(r.Header.Values("X-Forwarded-For"), ","); fwd != "" {
					// Right to left, taking the last address not itself a
					// trusted proxy. Reading left to right would take the
					// client-supplied value, which is trivially forged.
					parts := strings.Split(fwd, ",")
					for i := len(parts) - 1; i >= 0; i-- {
						candidate, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
						if err != nil {
							// Stop, do not skip. Everything left of an entry is
							// client-controlled, so continuing past one the
							// proxy wrote in a form we cannot parse hands the
							// walk straight to forged values: IIS and ARR append
							// the client as ip:port and Squid writes the literal
							// "unknown", and either made the next entry leftward
							// — the attacker's — look like the trusted hop.
							// Falling back to the peer address is the safe
							// answer: it is always real, only less specific.
							break
						}
						// Unmap before the trust check: a proxy that writes its
						// upstream hop as ::ffff:10.0.0.5 must match a 10.0.0.0/8
						// trust entry, and netip.Prefix.Contains never matches a
						// mapped address against an IPv4 prefix.
						candidate = candidate.Unmap()
						if !isTrusted(candidate, trusted) {
							addr = candidate
							break
						}
					}
				}
			}

			next.ServeHTTP(w, r.WithContext(auth.WithClientIP(r.Context(), addr)))
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
			// An API key already authenticated this request. The explicit
			// credential wins, and a stale cookie riding along on the same
			// request must not silently replace it with a stronger identity.
			if IdentityFrom(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}

			cookie, err := r.Cookie(name)
			if err == nil && cookie.Value != "" {
				id, err := a.Authenticate(r.Context(), cookie.Value)
				switch {
				case err == nil:
					r = r.WithContext(context.WithValue(r.Context(), ctxIdentity, id))
				case auth.IsSessionInvalid(err):
					// A cookie that will never resolve again is cleared, so the
					// browser stops sending it on every subsequent request.
					http.SetCookie(w, ClearSessionCookie(secure))
				default:
					// The lookup failed, which says nothing about the session.
					// Clearing here meant a Postgres restart signed out every
					// user who made a request during it — the cookie destroyed
					// on the way past, so the session was unrecoverable even
					// though the row was still there. Leave it alone and
					// continue anonymous; RequireAuth turns that into a
					// redirect to sign-in, and the next request succeeds.
					observability.LoggerFrom(r.Context()).Warn(
						"session lookup failed; continuing without an identity",
						slog.Any("error", err))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BearerAuth attaches an identity from an `Authorization: Bearer` API key.
//
// Unlike Session it does reject, and the asymmetry is deliberate. A cookie that
// no longer resolves is an ordinary event — an expired login — so the request
// continues anonymously and whatever it reaches decides. A bearer token is an
// explicit, deliberate credential: continuing anonymously would answer a
// revoked key with "authentication required", which reads as "the endpoint
// needs auth" rather than "your key is dead", and sends the caller looking in
// the wrong place.
//
// Runs before Session, so a request carrying both uses the key.
func BearerAuth(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			id, err := a.Authenticate(r.Context(), token)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="linkctrl"`)
				WriteError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxIdentity, id)))
		})
	}
}

// bearerToken extracts the credential from an Authorization header.
//
// Only the Bearer scheme is recognised. Anything else — Basic, a bare token
// with no scheme — is left alone rather than guessed at, so a client sending
// the wrong scheme gets "authentication required" rather than a confusing
// report about an invalid key it never sent.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	return token, token != ""
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
	// G124 wants Secure hardcoded true. It is configuration here because local
	// HTTP development needs it false, and the compensating control is stronger
	// than a constant: config validation refuses SECURE_COOKIES=false whenever
	// APP_ENV is production, and refuses an http BaseURL there too.
	return &http.Cookie{ //nolint:gosec // G124: Secure is config-driven and forced on in production
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
	return &http.Cookie{ //nolint:gosec // G124: mirrors NewSessionCookie; attributes must match to replace it
		Name:     auth.CookieName(secure),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// csp is the dashboard's content security policy.
//
// There is no 'unsafe-inline' anywhere in it, and the templates are written to
// keep it that way: scripts and styles are external files, dynamic bar widths
// are SVG attributes rather than style attributes, and the charts are inline
// SVG markup, which CSP does not restrict. htmx runs under script-src 'self'
// as long as none of its eval-based features (hx-on, js: expressions, bracket
// event filters) are used — which is a constraint on the templates worth the
// price, because 'unsafe-eval' would waive most of the policy's value.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"object-src 'none'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'"

// SecurityHeaders sets the defensive headers every response carries.
func SecurityHeaders(cfg config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), interest-cohort=()")
			h.Set("Content-Security-Policy", csp)
			if cfg.AppEnv.IsProduction() {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
