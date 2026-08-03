package httpx

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

// The gates a link can put in front of its destination (M35).
//
// Everything in this file runs for a link whose snapshot says it is gated, and
// for no other link. `Snapshot.Gated()` is one boolean expression over fields the
// resolver already produced, so a default instance — where nothing is gated —
// pays a single comparison and reaches none of the code below.
//
// The order is signature, then password, then budget, and it is the order in
// which a refusal costs least.
//
//   - A **signature** is a hash over four short strings against a key this
//     process already holds. It refuses without touching Postgres.
//   - A **password** costs an argon2id verification and a query, so it is
//     deliberately behind the signature: a link that demands both must not let
//     an unsigned request spend the server's key-derivation budget.
//   - The **budget** is a write, and it is last for the sharpest reason of the
//     three. It is the only gate that *consumes* something, so it must not run
//     until every other gate has passed — a wrong password that burned a
//     one-time link's single click would destroy the link on behalf of whoever
//     guessed at it.

//go:embed static/password.html
var passwordPage []byte

//go:embed static/password_wrong.html
var passwordWrongPage []byte

//go:embed static/403-signature.html
var unsignedPage []byte

// log returns the handler's logger, or the default one.
//
// The gates are the first thing on this path that logs outside an error branch a
// production instance never takes, so they are the first to meet a nil Logger —
// which is the ordinary state in a fixture that only cares about status codes. A
// nil-tolerant accessor rather than a guard at each call site, so a log line
// added later cannot reintroduce the panic.
func (h *RedirectHandler) log() *slog.Logger {
	if h.Logger == nil {
		return slog.Default()
	}
	return h.Logger
}

// gateResult is what passGates tells the handler to do.
type gateResult struct {
	// answered is true when the gate has already written the response.
	answered bool
	// label is the metric outcome to record for an answered request.
	label string
}

var gatePassed = gateResult{}

// passGates runs a gated link's gates, answering the request itself when one of
// them refuses.
//
// Called after the destination has been computed and before the 302 is written.
// That position is load-bearing at both ends: after, because a deep link this
// alias cannot forward is a 404 and must not spend a click; before, because the
// budget must be consumed by the request that is actually going to be redirected
// rather than by one that is about to be refused for some other reason.
func (h *RedirectHandler) passGates(
	w http.ResponseWriter, r *http.Request, snap *redirect.Snapshot, alias string,
) gateResult {
	// Fail closed. A gated link on an instance whose gate service was not wired
	// is a link whose owner asked for a restriction this process cannot enforce,
	// and serving it anyway would silently publish a password-protected
	// destination to anybody who asked. 503 says "not now" rather than "here you
	// are".
	if h.Gates == nil {
		h.log().Error("a gated link was requested but no gate service is configured; "+
			"refusing rather than serving it unguarded",
			slog.String("alias", alias))
		h.unavailable(w, r)
		return gateResult{answered: true, label: "error"}
	}

	if res := h.signatureGate(w, r, snap, alias); res.answered {
		return res
	}
	if res := h.passwordGate(w, r, snap, alias); res.answered {
		return res
	}
	return h.budgetGate(w, r, snap, alias)
}

// signatureGate refuses a request that does not carry a valid, unexpired HMAC
// for this alias.
//
// The workspace's key comes from the gate service's in-process cache, so a
// signed link costs one query per workspace per minute rather than one per
// request. A workspace that has never minted a key refuses everything, which is
// the correct reading of "this link requires a signature" on a workspace that
// cannot produce one.
func (h *RedirectHandler) signatureGate(
	w http.ResponseWriter, r *http.Request, snap *redirect.Snapshot, alias string,
) gateResult {
	if !snap.RequireSignature {
		return gatePassed
	}
	secret, err := h.Gates.Secret(r.Context(), snap.WorkspaceID)
	if err != nil && !errors.Is(err, gate.ErrNoSecret) {
		h.log().Error("could not read the signing secret for a link that requires one",
			slog.String("alias", alias), slog.Any("error", err))
		h.unavailable(w, r)
		return gateResult{answered: true, label: "error"}
	}
	if err := gate.Verify(secret, h.DomainID, alias, r.URL.Query(), time.Now()); err != nil {
		// Logged with the cause, answered without it. Which way a signature was
		// wrong is information about the key, and the visitor gets one page for
		// all four causes.
		h.log().Debug("refusing an unsigned or invalid request",
			slog.String("alias", alias), slog.Any("reason", err))
		h.errorPage(w, r, http.StatusForbidden, unsignedPage)
		return gateResult{answered: true, label: "unsigned"}
	}
	return gatePassed
}

// passwordGate serves the challenge, or verifies a submitted password.
//
// **This is the one POST the redirect tree accepts (D53), and everything it does
// not do is as deliberate as what it does.** It sets no cookie, issues no unlock
// token and creates no session: verification answers the 302 itself, so a
// visitor who comes back later types the password again. That is what makes the
// absent CSRF token defensible — a forged submission changes nothing and cannot
// read the cross-origin Location it would receive — and it is a condition rather
// than a permission. Anything that makes an unlock persist voids the reasoning
// and reopens the decision.
func (h *RedirectHandler) passwordGate(
	w http.ResponseWriter, r *http.Request, snap *redirect.Snapshot, alias string,
) gateResult {
	if !snap.HasPassword {
		return gatePassed
	}
	if r.Method != http.MethodPost {
		h.challenge(w, r, passwordPage)
		return gateResult{answered: true, label: "password_required"}
	}

	// Brute-force protection (D54), and both limbs are checked because either
	// alone is dodgeable. Per address stops one machine grinding through a
	// wordlist; per alias stops the same wordlist driven through a thousand
	// visitors' browsers, which is the CSRF variant with real teeth and the one
	// that a per-address limit would never see.
	//
	// It fails open when Redis is down: the shared limiter falls back to this
	// instance's own buckets, so the limit stops being a limit across replicas
	// and becomes one per replica. That is the only behaviour consistent with
	// the cache being optional, and it makes this protection best-effort rather
	// than a guarantee.
	if ok, retry := h.passwordAllowed(r, alias); !ok {
		h.Metrics.ObserveThrottled("link_password")
		h.tooManyRequests(w, r, retry)
		return gateResult{answered: true, label: "throttled"}
	}

	if err := r.ParseForm(); err != nil {
		h.challenge(w, r, passwordWrongPage)
		return gateResult{answered: true, label: "password_wrong"}
	}
	ok, err := h.Gates.VerifyPassword(r.Context(), snap.LinkID, r.PostFormValue("password"))
	switch {
	case errors.Is(err, gate.ErrNoPassword):
		// The snapshot said the link had a password and Postgres says it does
		// not, which means the password was removed while this entry was cached.
		// Answered as the ordinary redirect rather than as an error: the link is
		// open now, and the visitor asked for it.
		return gatePassed
	case err != nil:
		h.log().Error("could not verify a link password",
			slog.String("alias", alias), slog.Any("error", err))
		h.unavailable(w, r)
		return gateResult{answered: true, label: "error"}
	case !ok:
		h.challenge(w, r, passwordWrongPage)
		return gateResult{answered: true, label: "password_wrong"}
	}
	return gatePassed
}

// passwordAllowed asks both limbs of the password limit.
//
// Both are consumed rather than short-circuited on the first refusal, so an
// attacker cannot learn which limb they are hitting from the timing — and so
// that spreading guesses across addresses still empties the alias bucket.
func (h *RedirectHandler) passwordAllowed(r *http.Request, alias string) (bool, time.Duration) {
	if h.PasswordLimiter == nil {
		return true, 0
	}
	byAddr, addrRetry := h.PasswordLimiter.Allow(ClientIPFrom(r.Context())) //nolint:contextcheck // deliberate: see ratelimit.Shared.take
	// Prefixed and domain-scoped: the key table holds addresses too, and the same
	// alias on two domains is two links.
	byAlias, aliasRetry := h.PasswordLimiter.AllowKey("pw:" + h.DomainID.String() + ":" + alias) //nolint:contextcheck // deliberate: see ratelimit.Shared.take
	if byAddr && byAlias {
		return true, 0
	}
	return false, max(addrRetry, aliasRetry)
}

// budgetGate spends one click of a one-time or max-click link's durable budget.
//
// **Postgres, not Redis, and that is the inherited cache-is-optional rule rather
// than a preference.** A link that may be followed once must be followed once on
// an instance with the cache switched off, and a counter that disappears with
// the cache re-opens every spent link at once.
//
// HEAD never consumes. It is a client asking about the link rather than
// following it — the same rule the click recorder follows — and a crawler that
// could burn a one-time link by checking whether it is alive would destroy the
// feature.
func (h *RedirectHandler) budgetGate(
	w http.ResponseWriter, r *http.Request, snap *redirect.Snapshot, alias string,
) gateResult {
	limit, gated := gate.ClickLimit(snap.OneTime, snap.MaxClicks)
	if !gated || r.Method == http.MethodHead {
		return gatePassed
	}
	ok, err := h.Gates.Consume(r.Context(), snap.LinkID, snap.WorkspaceID, limit)
	switch {
	case err != nil:
		// Not treated as exhaustion. A 410 is a durable claim that link checkers
		// and crawlers act on, and answering it because the database blinked
		// would retire a live link on the strength of a timeout.
		h.log().Error("could not consume a link's click budget",
			slog.String("alias", alias), slog.Any("error", err))
		h.unavailable(w, r)
		return gateResult{answered: true, label: "error"}
	case !ok:
		h.gone(w, r)
		return gateResult{answered: true, label: "spent"}
	}
	return gatePassed
}

// challenge writes the password page.
//
// 200 rather than 401, because there is no WWW-Authenticate scheme being
// offered and a 401 without one is a status no client can act on. It is a page
// asking a person a question, and the status says the server answered.
//
// no-store and noindex for the same reason every other page on this tree sets
// them: a shortener accumulates gated links, and a cached or indexed challenge
// is either served to the wrong person or advertised to a crawler.
func (h *RedirectHandler) challenge(w http.ResponseWriter, r *http.Request, body []byte) {
	h.errorPage(w, r, http.StatusOK, body)
}

// methodNotAllowed refuses a POST to an alias that is not a password link.
//
// The mux lets POST through to this handler because password verification needs
// it (D53), and this is the boundary of that permission: every alias that does
// not ask a question answers the same 405 the mux used to.
func (h *RedirectHandler) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	head := w.Header()
	head.Set("Allow", "GET, HEAD")
	head.Set("X-Robots-Tag", "noindex, nofollow")
	head.Set("Cache-Control", "no-store")
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusMethodNotAllowed)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("Method not allowed\n"))
	}
}

// Gatekeeper is what the redirect handler needs from internal/gate.
//
// An interface at the consumer, so a test can substitute one without a database
// and so the handler cannot reach for anything else the gate service happens to
// expose.
type Gatekeeper interface {
	VerifyPassword(ctx context.Context, linkID uuid.UUID, password string) (bool, error)
	Consume(ctx context.Context, linkID, workspaceID uuid.UUID, limit int64) (bool, error)
	Secret(ctx context.Context, workspaceID uuid.UUID) ([]byte, error)
	// Rotate advances a sequential split's durable counter (M36, D8).
	//
	// Here rather than in an interface of its own because it is the same table,
	// the same service and the same nil check: a second dependency on this
	// handler would be a second thing to wire and a second thing to forget,
	// for one method that exists for exactly the reason the click budget does —
	// a counter Redis cannot hold. The name of the interface is now slightly
	// wider than "gatekeeper", and that is the cheaper of the two prices.
	Rotate(ctx context.Context, linkID, workspaceID uuid.UUID) (int64, error)
}
