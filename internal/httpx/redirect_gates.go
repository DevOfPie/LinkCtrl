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
//
// **domainID is the domain this request arrived on, not the instance default.**
// It is passed in rather than read off the handler because `h.DomainID` is
// resolved once at boot, and a verified custom hostname is a different alias
// namespace served by the same process (M40). Two gates key on it — the
// signature's MAC and the per-alias password bucket — and reading the boot
// constant made both of them describe a link other than the one being served.
func (h *RedirectHandler) passGates(
	w http.ResponseWriter, r *http.Request, snap *redirect.Snapshot,
	alias string, domainID uuid.UUID,
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

	if res := h.signatureGate(w, r, snap, alias, domainID); res.answered {
		return res
	}
	if res := h.passwordGate(w, r, snap, alias, domainID); res.answered {
		return res
	}
	return h.budgetGate(w, r, snap, alias)
}

// challengePending reports whether this request is going to be answered with the
// password challenge rather than with a destination.
//
// **It exists so that a split does not advance twice for one visit (F87), and
// the password gate is the only gate that needs it.** Every other refusal on
// this path ends the visit: an unsigned request, a wrong password, a spent
// budget and an unforwardable deep link each answer once and are not followed by
// a second request the server can predict. The challenge is not a refusal — it
// is the first half of a visit that arrives in two parts, because the page it
// serves exists to be posted back. A sequential split therefore consumed two
// positions per visitor and served the second of them, which at any even arm
// count meant half the arms were served to nobody and at two arms meant `arms[0]`
// was served to nobody, ever.
//
// Asked here, and answered before the destination is chosen, because the fix
// cannot be to move `h.split` after `passGates`: the gates run after the
// destination is known on purpose (D53, and the ordering note above passGates),
// so that an unforwardable deep link is a 404 before the budget gate can spend a
// one-time link's only click on it. Moving the split instead of skipping it would
// buy this at that price.
//
// A GET to a link that carries both a password and a split is therefore priced
// exactly as a GET to a link that carries a password alone — no arm chosen, no
// rotation written, no arithmetic — and the arm is chosen by the POST that is
// actually going to be redirected. The one thing this changes for that first
// request is which URL `forwardable` is asked about: the link's own destination
// rather than an arm's. That answer does not depend on which of them it is —
// `ForwardPath` is the link's, and `appendPath` fails on the *remainder*, not on
// the target — and it is discarded either way, because a challenged request
// writes no `Location`.
func challengePending(snap *redirect.Snapshot, r *http.Request) bool {
	return snap != nil && snap.HasPassword && r.Method != http.MethodPost
}

// signatureGate refuses a request that does not carry a valid, unexpired HMAC
// for this alias.
//
// The workspace's key comes from the gate service's in-process cache, so a
// signed link costs one query per workspace per minute rather than one per
// request. A workspace that has never minted a key refuses everything, which is
// the correct reading of "this link requires a signature" on a workspace that
// cannot produce one.
//
// The MAC covers the domain the request arrived on, which is the domain the
// link was signed under — `internal/link/gates.go` signs with the link's own
// `domain_id`, and alias uniqueness is `(domain_id, alias)`, so these are the
// same value for a request that resolved to this link at all.
func (h *RedirectHandler) signatureGate(
	w http.ResponseWriter, r *http.Request, snap *redirect.Snapshot,
	alias string, domainID uuid.UUID,
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
	if err := gate.Verify(secret, domainID, alias, r.URL.Query(), time.Now()); err != nil {
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
// token and creates no session: verification answers the redirect itself, so a
// visitor who comes back later types the password again. That is what makes the
// absent CSRF token defensible — a forged submission changes nothing and cannot
// read the cross-origin Location it would receive — and it is a condition rather
// than a permission. Anything that makes an unlock persist voids the reasoning
// and reopens the decision.
//
// That redirect is a **303** and not the instance's configured status (D94,
// correcting D53's "answers the 302 itself"). See redirectStatus: on a 307
// instance the browser would re-POST the password to the destination.
func (h *RedirectHandler) passwordGate(
	w http.ResponseWriter, r *http.Request, snap *redirect.Snapshot,
	alias string, domainID uuid.UUID,
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
	if ok, retry := h.passwordAllowed(r, alias, domainID); !ok {
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
func (h *RedirectHandler) passwordAllowed(
	r *http.Request, alias string, domainID uuid.UUID,
) (bool, time.Duration) {
	if h.PasswordLimiter == nil {
		return true, 0
	}
	byAddr, addrRetry := h.PasswordLimiter.Allow(ClientIPFrom(r.Context())) //nolint:contextcheck // deliberate: see ratelimit.Shared.take
	// Prefixed and domain-scoped: the key table holds addresses too, and the same
	// alias on two domains is two links. The domain is the request's own, so the
	// sentence is true — keyed on the boot default it was one bucket shared by
	// every hostname, which admits fewer guesses rather than more and is why this
	// limb failed safe while the signature's did not.
	byAlias, aliasRetry := h.PasswordLimiter.AllowKey("pw:" + domainID.String() + ":" + alias) //nolint:contextcheck // deliberate: see ratelimit.Shared.take
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
//
// **Never consuming is not the same as never checking, and reading it as such
// published every spent link forever.** A HEAD that skipped this gate fell
// through to the ordinary redirect write, so `Location:` carried the
// destination of a link that answers 410 to a GET — repeatably, and without a
// click row, because the recorder skips HEAD too. So HEAD takes the branch
// below: the same question, asked with a read instead of a write.
func (h *RedirectHandler) budgetGate(
	w http.ResponseWriter, r *http.Request, snap *redirect.Snapshot, alias string,
) gateResult {
	limit, gated := gate.ClickLimit(snap.OneTime, snap.MaxClicks)
	if !gated {
		return gatePassed
	}
	if r.Method == http.MethodHead {
		return h.budgetPeek(w, r, snap, alias, limit)
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

// budgetPeek answers a HEAD against a click budget without spending one.
//
// `Budget` is a read of the same row `Consume` writes, and the predicate is
// `consumed >= limit` rather than `exhausted_at != nil` for one reason: the
// stamp is written by the click that reached the ceiling, so a ceiling *lowered*
// afterwards leaves a spent link with no stamp. `consumed >= limit` is the same
// comparison ConsumeClickBudget makes in its conflict clause, which is what
// makes HEAD and GET agree about the word "spent".
//
// **What this costs, stated rather than implied.** One primary-key read, on a
// HEAD, to a link that carries a budget. A GET is unchanged — it still performs
// exactly the one upsert it always did, and no read was added in front of it,
// because that would have put a query on every gated redirect to buy an answer
// the upsert already gives. A link with no budget, and every link on a default
// instance, reaches none of this.
func (h *RedirectHandler) budgetPeek(
	w http.ResponseWriter, r *http.Request, snap *redirect.Snapshot,
	alias string, limit int64,
) gateResult {
	consumed, _, err := h.Gates.Budget(r.Context(), snap.LinkID)
	if err != nil {
		// The same direction Consume's failure takes, for the same reason: 410 is
		// a durable claim a link checker acts on, and this branch exists to answer
		// link checkers.
		h.log().Error("could not read a link's click budget",
			slog.String("alias", alias), slog.Any("error", err))
		h.unavailable(w, r)
		return gateResult{answered: true, label: "error"}
	}
	if consumed >= limit {
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
	// Budget reads a link's spend without adding to it, for HEAD.
	//
	// It was on the service before this handler asked for it — the dashboard
	// reads it to show how much of a ceiling is gone — and it is here because
	// the answer HEAD needs is "is this spent", which is the only question
	// Consume cannot be asked without also answering it.
	Budget(ctx context.Context, linkID uuid.UUID) (int64, *time.Time, error)
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
