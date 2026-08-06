package httpx

import (
	"log/slog"
	"math/rand/v2"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

// Split testing on the redirect path (M36).
//
// Everything here runs for a link that carries split arms, and for no other
// link. `Snapshot.Split` returns on a length check over a slice that is nil for
// every link on a default instance, so the cost of this file to a link that
// asked for none of it is one function call that reads one integer.
//
// The two kinds pay very different prices, and the difference is the whole of
// D8.
//
//   - **Weighted** is arithmetic over values already in the snapshot: a sum, a
//     random integer below it, and a walk. No query, no lock, no shared state —
//     math/rand/v2's top-level source is per-P and needs no mutex, which matters
//     on a path that runs thousands of times a second.
//   - **Sequential** is a durable Postgres write per request, because "strict"
//     is the property D8 chose and an in-process counter cannot give it: two
//     replicas would each run their own rotation and a visitor's position would
//     depend on which container answered. The write lands only on links that
//     actually carry a sequential arm.
//
// **There is no stickiness, deliberately.** A visitor who follows the same link
// twice may be sent to two different arms. That is what a link-level test is:
// each click is an independent trial, and which arm converted is answered by
// `click_events.destination_id` rather than by remembering people. Remembering
// them would mean either a cookie the rest of this product refuses to set or a
// per-visitor lookup on the hot path, and neither is a cost this milestone was
// asked to pay.

// splitOutcome is what a link's split decided.
type splitOutcome struct {
	// choice is the arm, valid only when chosen is true.
	choice redirect.Choice
	chosen bool
	// failed means the rotation could not be advanced. The caller answers 503
	// rather than guessing: see rotate.
	failed bool
}

var splitPassed = splitOutcome{}

// split chooses an arm for this request, if the link has any.
func (h *RedirectHandler) split(r *http.Request, snap *redirect.Snapshot, alias string) splitOutcome {
	kind, arms := snap.Split()
	if len(arms) == 0 {
		return splitPassed
	}
	// The one request that must not choose an arm (F87). See challengePending.
	if challengePending(snap, r) {
		return splitPassed
	}
	switch kind {
	case domain.RuleKindWeighted:
		return weightedPick(arms, snap.Weights(arms))
	case domain.RuleKindSequential:
		return h.rotate(r, snap, arms, alias)
	default:
		return splitPassed
	}
}

// weightedPick chooses an arm in proportion to its weight.
//
// A total of zero — every enabled arm parked at weight 0 — chooses nothing, and
// the caller falls through to the fallback or to the link's own destination.
// Picking uniformly instead would be inventing traffic for arms whose owner set
// them to receive none; refusing the request would break a link over a setting
// the form permits.
func weightedPick(arms []redirect.Choice, weights []int32) splitOutcome {
	var total int64
	for _, w := range weights {
		if w > 0 {
			total += int64(w)
		}
	}
	if total <= 0 {
		return splitPassed
	}
	// N is exclusive, so n is in [0, total) and the walk below always settles on
	// an arm: the running sum reaches total on the last positive weight.
	// math/rand rather than crypto/rand, deliberately. This picks which of two
	// landing pages somebody sees; it is not a secret, nothing is authorized by
	// it, and a visitor who could predict their own arm would learn which page
	// they were about to be shown. crypto/rand would cost a syscall on the hot
	// path to defend against nothing.
	n := rand.Int64N(total) //nolint:gosec // G404: not a security decision; see above
	var running int64
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		running += int64(w)
		if n < running {
			return splitOutcome{choice: arms[i], chosen: true}
		}
	}
	return splitPassed
}

// rotate advances the link's durable counter and returns the arm whose turn it
// is (D8).
//
// The counter is global and monotonic, so the arms are visited strictly in turn
// across every replica and across restarts. Adding or removing an arm re-phases
// the rotation rather than restarting it, which is the honest behaviour: there is
// no correct answer to "who was next" once the list has changed underneath.
//
// **A request that is later refused still consumed its position, with one
// exception.** The rotation happens where the destination is decided, which is
// before the deep-link join and before the gates, so a visitor who gets a 404 or
// submits a wrong password has advanced the counter. That does not skew the
// split — whether a gate *passes* is independent of which arm came up — and the
// alternative is deciding the destination twice, or moving the gates ahead of the
// join and spending a one-time link's click on a request that was about to 404
// anyway. M35's ordering is the stronger constraint and it wins.
//
// The exception is the password challenge, and it is the exception because it is
// not a refusal at all: it is the first half of a request that arrives in two
// parts. See challengePending.
//
// A failure is a 503 and never a guess. Falling back to an arbitrary arm would
// make the rotation "approximately sequential", which is the thing D8 named as a
// support ticket; falling back to the link's own destination would silently
// retire the test the moment the database hiccuped.
func (h *RedirectHandler) rotate(
	r *http.Request, snap *redirect.Snapshot, arms []redirect.Choice, alias string,
) splitOutcome {
	if h.Gates == nil {
		h.log().Error("a link with a sequential split was requested but no gate "+
			"service is configured; refusing rather than serving an arbitrary arm",
			slog.String("alias", alias))
		return splitOutcome{failed: true}
	}
	// HEAD chooses without advancing (F100).
	//
	// A link checker or an unfurler probing this alias used to move the durable
	// counter on every request, re-phasing every subsequent visitor's arm — and
	// because HEAD writes no click event, the per-destination breakdown could
	// not show why the arms were uneven. That falsified two absolutes in D8's
	// entry at once: *the budget is the only gate that consumes something*, and
	// *HEAD never consumes at all*. M36 put a second durable counter in the same
	// table and neither sentence was revisited.
	//
	// Returning early on HEAD is the fix this looks like and it is wrong: a HEAD
	// would answer the link's own destination while a GET answers an arm, so a
	// checker validates a URL no visitor is ever sent to. It has to choose the
	// arm the next GET would choose, which is what PeekRotation reads — the same
	// shape, and the same caller, as Budget reading a click allowance without
	// spending it.
	rotate := h.Gates.Rotate
	verb := "advance"
	if r.Method == http.MethodHead {
		rotate, verb = h.Gates.PeekRotation, "read"
	}
	position, err := rotate(r.Context(), snap.LinkID, snap.WorkspaceID)
	if err != nil {
		h.log().Error("could not "+verb+" a sequential split's rotation",
			slog.String("alias", alias), slog.Any("error", err))
		return splitOutcome{failed: true}
	}
	// The counter returns 1 for the first click, so the first visitor gets the
	// first arm rather than the second.
	i := (position - 1) % int64(len(arms))
	if i < 0 {
		i = 0
	}
	return splitOutcome{choice: arms[i], chosen: true}
}
