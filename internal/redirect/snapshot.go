// Package redirect resolves an alias to a destination.
//
// Everything here runs inside a 20ms budget, so the design is shaped by what
// the hot path must NOT do: no joins, no session lookup, no template
// rendering, no synchronous write, and no dependency whose failure can take
// the path down.
package redirect

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// Snapshot is everything the redirect handler needs, in one cacheable value.
//
// Deliberately not the full link row. It carries only what a decision depends
// on, so the cached payload stays small and a schema change to columns the hot
// path ignores does not invalidate the cache.
//
// The Phase 2 fields are present now because they change what must be cached,
// not what is currently enforced: adding them later would mean a cache-key
// version bump and a cold cache on upgrade.
type Snapshot struct {
	LinkID      uuid.UUID  `json:"i"`
	WorkspaceID uuid.UUID  `json:"w"`
	URL         string     `json:"u"`
	Status      string     `json:"s"`
	ExpiresAt   *time.Time `json:"e,omitempty"`

	ForwardQuery bool `json:"q,omitempty"`

	// The gates (M35). Three of these four fields have been in the struct since
	// Phase 1 for exactly this milestone — adding them later would have meant a
	// cache-key bump of their own — and M35 is where something finally reads
	// them.
	//
	// **HasPassword is a boolean and never the hash.** That is the one property
	// of this struct worth stating as a rule rather than as a comment on a
	// field: this value is serialized into Redis on every cache write, so
	// carrying the argon2id hash would put an offline cracking target for every
	// password link on the instance into an optional dependency that is
	// routinely dumped, replicated and inspected. The hash is read from Postgres
	// on the submit path only — see internal/gate — and
	// TestCachedSnapshotCarriesNoPasswordHash asserts the payload.
	//
	// MaxClicks and OneTime say what the *limit* is, never how much of it is
	// left. The remaining budget is a durable Postgres counter, because a cached
	// count that vanishes with Redis re-opens every spent link at once.
	//
	// RequireSignature is M35's own addition, and it rides M34's v2 bump rather
	// than asking for one of its own: it ships in the same release, so no
	// instance can ever hold a v2 entry written without it. Its absence would
	// decode as false — "this link needs no signature" — which is the direction
	// that would matter if it ever were stale, so the shared bump is doing real
	// work rather than being borrowed for convenience.
	HasPassword      bool   `json:"p,omitempty"`
	MaxClicks        *int64 `json:"m,omitempty"`
	OneTime          bool   `json:"o,omitempty"`
	RequireSignature bool   `json:"sg,omitempty"`

	// Routing rules (M34) and split testing (M36), and the two milestones that
	// bumped CacheKeyVersion — to v2 and then to v3.
	//
	// Two fields, because the same destination is routinely the target of more
	// than one rule and because "the destination list" is a thing in its own
	// right. Destinations holds the rule targets and the split arms,
	// deduplicated; the link's own destination is URL above and is where a
	// request that neither matched a rule nor found an arm goes. Rules index
	// into it.
	//
	// **The slice order is the evaluation order.** Nothing here carries a
	// priority number, because the query that built this list already applied
	// it: match rules come back ordered by (priority, created_at), split arms by
	// position, disabled rules filtered out, and first match short-circuits.
	// Storing the priority as well would be a second copy of the ordering that a
	// re-sort could disagree with — and the only correct thing to do with it here
	// would be to sort by it again.
	//
	// Both are omitempty, so a link with no rules — the default, and the
	// overwhelming majority — carries exactly the payload it carried before
	// either milestone existed.
	Destinations []SnapshotDest `json:"d,omitempty"`
	Rules        []SnapshotRule `json:"r,omitempty"`

	// Deep-link path forwarding (M33). Added without bumping CacheKeyVersion,
	// and the reason is narrower than the one written below for bot blocking.
	//
	// It is not that nobody could have set the column yet. A rolling restart
	// runs migrations at boot and then serves from old and new containers at
	// once, so an old binary goes on writing entries without this field while
	// the feature is already switched on somewhere — that is F41, recorded
	// against the paragraph below, and it is a real sequence rather than a
	// hypothetical one.
	//
	// What makes the omitted bump safe here is which way the zero value falls.
	// An absent `fp` decodes as false, false means *do not forward*, and that is
	// exactly what this alias did before the milestone existed. A visitor whose
	// deep link lands on a stale entry gets the 404 they would have got
	// yesterday, for at most REDIRECT_TTL, and the next fetch fixes it. The
	// failure is a feature not yet working, not a control not being applied —
	// so there is nothing here a cold cache would buy. A field whose absence
	// meant "forward" would have needed the bump, because then the stale
	// reading would send somebody somewhere the owner never configured.
	//
	// This held only while the cache key was v1 for this build and the previous
	// one. M34 has now bumped it to v2, which is what that ordering claimed
	// would happen; the reasoning above is what carried the field safely across
	// the one release where it was not yet true.
	ForwardPath bool `json:"fp,omitempty"`

	// Bot blocking (M32.5). Both halves of the precedence rule travel together,
	// because the whole point is that a cache hit answers the question without
	// asking anything: a link's setting alone cannot decide, and fetching the
	// domain's separately would be the round trip this design exists to avoid.
	//
	// Adding them did NOT bump CacheKeyVersion, and that is a claim worth being
	// explicit about. Both are omitempty, so an entry written by the previous
	// build decodes with the zero values — inherit and off — which is exactly
	// "no blocking".
	//
	// **The reason originally given for that being safe was wrong** (F41). It
	// read: on any instance holding such an entry the columns did not exist a
	// moment ago, so nobody can have switched blocking on yet. That assumes the
	// old build stops before the new one starts. Migrations run at boot before
	// the listener opens (docs/releasing.md), so a rolling restart has both
	// builds serving at once: the new one switches blocking on, the old one goes
	// on writing bot-less entries under the same key, and a bot is answered 302
	// for up to REDIRECT_TTL. Reproduced at the Redis layer — three 302s to a
	// bot against a link whose row said block_bots. SetBotBlocking's
	// unconditional InvalidateDomain sweeps entries written *before* blocking is
	// switched on, which is why only the concurrent-write case survives.
	//
	// **What actually makes it safe is arithmetic, not reasoning.** 0.1.0 is the
	// only release that exists and it keys on v1; this build keys on v3, so no
	// entry any released build ever wrote is one this build can read. M32.5, M34
	// and M36 all land inside the same unreleased minor. The residue is empty
	// because there is nowhere for it to be, exactly as F132's was.
	//
	// **The rule for next time, which is what this comment is really for.** A
	// new omitempty field may skip the bump only when the zero value means the
	// same thing to a visitor as the true value would — not when nobody has had
	// time to configure it yet, because two builds serving at once is a state a
	// rolling restart produces on purpose. If the stale reading is a control the
	// owner configured being silently absent, bump it and pay for the cold
	// cache; that is the argument v2 and v3 were both bumped on.
	BotPolicy       domain.BotPolicy       `json:"bb,omitempty"`
	DomainBotPolicy domain.DomainBotPolicy `json:"db,omitempty"`

	// NotFound marks a negative cache entry. Storing misses matters: an
	// unknown alias is the single most common request a public shortener
	// receives, mostly from scanners, and without this every one of them is a
	// database query.
	NotFound bool `json:"n,omitempty"`
}

// SnapshotDest is one destination the redirect path may send somebody to.
//
// The id is here so a click can be attributed to it — click_events.destination_id
// (M36) — and the weight so a weighted arm's share can be computed without
// asking the database anything. Both are new in v3; before it this was a bare
// string.
//
// The link's own destination is deliberately **not** in this list. It is
// Snapshot.URL, and a click that goes there records a NULL destination_id, which
// the breakdown reads as "the link's own destination". Carrying its id as well
// would put a uuid in the payload of every link on the instance to say something
// the link already says.
type SnapshotDest struct {
	ID  uuid.UUID `json:"i"`
	URL string    `json:"u"`
	// Weight is meaningful only for a weighted arm. Zero for a match rule's
	// target, where nothing reads it.
	Weight int32 `json:"w,omitempty"`
}

// SnapshotRule is one rule as the redirect path evaluates it.
//
// Dest is an index into Snapshot.Destinations rather than a URL, so two rules
// pointing at the same place cost one copy of the string. Out-of-range is
// treated as "no destination" by Route rather than as a panic: the hot path
// must survive a payload it did not write, and a rule that cannot be honoured
// falling through to the link's own destination is a survivable answer where a
// panic on a redirect is not.
//
// Kind is absent for a match rule and omitempty, so M34's rules encode exactly
// as they did — the empty string is `match`, which is what KindOf returns for
// one. That keeps the payload of a link with only match rules the size it was,
// while a split arm pays four bytes to say what it is.
type SnapshotRule struct {
	Dest int                   `json:"d"`
	Cond domain.RuleConditions `json:"c"`
	Kind string                `json:"k,omitempty"`
}

// KindOf is the rule's kind, with the empty string read as `match`.
func (r SnapshotRule) KindOf() string {
	if r.Kind == "" {
		return domain.RuleKindMatch
	}
	return r.Kind
}

// Choice is a destination the redirect path settled on.
//
// ID is the destinations row the click is attributed to, and the zero uuid means
// the link's own destination. A struct rather than two returns, because "which
// URL" and "which row" must not be able to disagree — every place that decides
// one decides the other in the same statement.
type Choice struct {
	URL string
	ID  uuid.UUID
}

// Route returns the destination this request should be sent to by a *match*
// rule, and reports whether one decided it.
//
// The whole of first-match evaluation, and it is short because everything that
// makes it correct happened earlier: the query ordered the rules and dropped the
// disabled ones, the resolver put them in the snapshot, and domain.Match decides
// one rule against one request without reading anything.
//
// A snapshot with no rules — the default state of every link on a default
// instance — returns on the length check without touching the subject, which is
// what makes "links without rules resolve through the unchanged fast path"
// structural rather than a promise. Split arms are skipped here rather than
// filtered out beforehand, because they share the slice with the match rules and
// walking past them costs a string comparison on a list bounded by
// domain.MaxRulesPerLink.
func (s *Snapshot) Route(subject domain.RuleSubject) (Choice, bool) {
	if s == nil || len(s.Rules) == 0 {
		return Choice{}, false
	}
	for _, r := range s.Rules {
		if r.KindOf() != domain.RuleKindMatch {
			continue
		}
		if !domain.Match(r.Cond, subject) {
			continue
		}
		dest, ok := s.destAt(r.Dest)
		if !ok {
			// A rule whose target is not in the list this payload carries. The
			// only way to write one is a corrupt or hand-edited cache entry, and
			// the answer is to behave as though the rule had not matched rather
			// than to send somebody to an empty Location header.
			continue
		}
		return dest, true
	}
	return Choice{}, false
}

// destAt reads a rule's target out of the destination list.
func (s *Snapshot) destAt(i int) (Choice, bool) {
	if i < 0 || i >= len(s.Destinations) {
		return Choice{}, false
	}
	d := s.Destinations[i]
	if d.URL == "" {
		return Choice{}, false
	}
	return Choice{URL: d.URL, ID: d.ID}, true
}

// Split is this link's split test: the enabled arms, in rotation order, and
// their kind (M36).
//
// Returns an empty kind for every link that has none, which is every link on a
// default instance — and returns it after a walk over a slice that is nil for
// such a link, so the loop below never starts.
//
// Disabled arms are already absent: the resolver's query filters on `enabled`,
// which is what makes the `enabled` toggle a feature flag rather than a field
// somebody has to remember to check. An arm switched off stops receiving
// traffic on the next resolve, and the remaining arms re-share it.
func (s *Snapshot) Split() (kind string, arms []Choice) {
	if s == nil || len(s.Rules) == 0 {
		return "", nil
	}
	for _, r := range s.Rules {
		k := r.KindOf()
		if k != domain.RuleKindWeighted && k != domain.RuleKindSequential {
			continue
		}
		dest, ok := s.destAt(r.Dest)
		if !ok {
			continue
		}
		// The first arm found decides the kind, and an arm of the other kind is
		// then ignored rather than allowed to change it mid-walk. The service
		// refuses to create a mixed split, so this is a corrupt payload or a
		// hand-edited row; ignoring is the answer that still sends somebody
		// somewhere the owner configured.
		if kind == "" {
			kind = k
		}
		if k != kind {
			continue
		}
		arms = append(arms, dest)
	}
	if len(arms) == 0 {
		return "", nil
	}
	return kind, arms
}

// Weights are the arms' weights, in the same order Split returns them.
//
// Separate from Split because a sequential rotation must not pay for a slice it
// will not read.
//
// A nested scan rather than a map, and that is the right shape at this size: both
// slices are bounded by domain.MaxRulesPerLink, so the worst case is a few
// hundred integer comparisons against one map allocation and one hash per arm.
// This runs on the redirect path for every request to a weighted link, and an
// allocation there costs more than the comparisons it saves.
func (s *Snapshot) Weights(arms []Choice) []int32 {
	out := make([]int32, len(arms))
	for i, a := range arms {
		for _, d := range s.Destinations {
			if d.ID == a.ID {
				out[i] = d.Weight
				break
			}
		}
	}
	return out
}

// Fallback is where this link sends anybody no rule and no arm claimed (M36).
//
// The first enabled fallback rule wins if a payload somehow carries two; the
// service permits only one, so this is the same "survive a payload we did not
// write" rule the rest of this file follows.
func (s *Snapshot) Fallback() (Choice, bool) {
	if s == nil || len(s.Rules) == 0 {
		return Choice{}, false
	}
	for _, r := range s.Rules {
		if r.KindOf() != domain.RuleKindFallback {
			continue
		}
		if dest, ok := s.destAt(r.Dest); ok {
			return dest, true
		}
	}
	return Choice{}, false
}

// RuleNeeds summarizes which lookups this link's rules can ask for, so the
// caller resolves a city only for a link that mentions one.
//
// Split arms are skipped: their condition set is empty by construction — a
// variant is chosen, never matched — and including it would be asking
// domain.NeedsOf about a struct that is always zero.
func (s *Snapshot) RuleNeeds() domain.RuleNeeds {
	if s == nil || len(s.Rules) == 0 {
		return domain.RuleNeeds{}
	}
	conds := make([]domain.RuleConditions, 0, len(s.Rules))
	for _, r := range s.Rules {
		if r.KindOf() != domain.RuleKindMatch {
			continue
		}
		conds = append(conds, r.Cond)
	}
	return domain.NeedsOf(conds)
}

// Short JSON keys are not premature micro-optimisation: this value is
// serialized and parsed on every cache miss and every cache write, and the
// field names would otherwise be most of the payload.

// Outcome is what the handler should do with a snapshot.
type Outcome int

const (
	// OutcomeRedirect sends the visitor onward.
	OutcomeRedirect Outcome = iota
	// OutcomeNotFound covers unknown, archived and disabled links. They are
	// deliberately indistinguishable: telling a scanner that an alias exists
	// but is archived is information it has no use for.
	OutcomeNotFound
	// OutcomeGone is an expired link. Distinct from not-found because the
	// alias really did exist, and 410 tells crawlers to stop asking.
	OutcomeGone
)

// Gated reports whether this link has anything in front of its destination
// (M35).
//
// One call, on a value the resolver already produced, and it is false for every
// link on a default instance. It is what keeps the gate machinery — a Postgres
// read for a signature key, an argon2 verification, a durable counter write —
// out of the path of a link that asked for none of it.
func (s *Snapshot) Gated() bool {
	return s != nil && (s.HasPassword || s.RequireSignature || s.OneTime || s.MaxClicks != nil)
}

// Decide reports what to do with a snapshot at a given time.
//
// Expiry is evaluated here rather than filtered in SQL so that an expired link
// yields 410 rather than 404, and so the decision is identical whether the
// snapshot came from cache or from the database.
func (s *Snapshot) Decide(now time.Time) Outcome {
	switch {
	case s == nil, s.NotFound:
		return OutcomeNotFound
	case s.ExpiresAt != nil && !now.Before(*s.ExpiresAt):
		return OutcomeGone
	case s.Status == "expired":
		return OutcomeGone
	case s.Status != "active":
		// archived, disabled
		return OutcomeNotFound
	default:
		return OutcomeRedirect
	}
}

// CacheTTL returns how long this snapshot may be cached.
//
// Clamped to the expiry: caching a link for 24h when it expires in 5 minutes
// would keep serving it for hours after it should have stopped. This is the
// kind of bug that only shows up in production, on the one link that mattered.
func (s *Snapshot) CacheTTL(now time.Time, base, negative time.Duration) time.Duration {
	if s == nil || s.NotFound {
		// The same one-second floor as a positive entry, and for a sharper
		// reason: go-redis treats a zero expiration as "no TTL", so a
		// REDIRECT_NEGATIVE_TTL of 0 — the natural way to write "do not cache
		// misses" — wrote a permanent key for every well-formed alias anyone
		// ever probed. A scanner spraying /abc123 paths would fill Redis with
		// keys that never expire.
		if negative < time.Second {
			return time.Second
		}
		return negative
	}
	ttl := base
	if s.ExpiresAt != nil {
		if remaining := s.ExpiresAt.Sub(now); remaining < ttl {
			ttl = remaining
		}
	}
	if ttl < time.Second {
		ttl = time.Second
	}
	return ttl
}

func (s *Snapshot) encode() ([]byte, error) { return json.Marshal(s) }

func decodeSnapshot(b []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// notFoundSnapshot is the negative cache entry.
func notFoundSnapshot() *Snapshot { return &Snapshot{NotFound: true} }
