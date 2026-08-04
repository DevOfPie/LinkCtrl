package gate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Service answers the three questions a gated redirect asks Postgres: does this
// password match, does this workspace's key verify this signature, and is there
// any budget left.
//
// Postgres and not Redis, for all three, and the reason is the inherited rule
// rather than a preference. The cache is optional: an instance with Redis
// switched off must still refuse the second visit to a one-time link and must
// still reject a wrong password. A counter that disappears with the cache
// re-opens every spent link at once, which is not a degraded mode — it is the
// gate not existing.
type Service struct {
	q      *dbgen.Queries
	hasher *auth.Hasher

	// keys caches workspace signing secrets in process, because verifying a
	// signature is on the hot path and a query per request would put the
	// database inside the 20ms budget for every signed link.
	//
	// A plain map behind a mutex rather than the LRU the snapshot cache uses:
	// the key space is workspaces, not aliases, and an instance with enough
	// workspaces for this to matter has bigger numbers to worry about than a few
	// hundred bytes each.
	keysMu sync.RWMutex
	keys   map[uuid.UUID]cachedSecret
	keyTTL time.Duration
	now    func() time.Time

	// dbTimeout bounds every query below. See DefaultDBTimeout.
	dbTimeout time.Duration
}

type cachedSecret struct {
	secret  []byte
	expires time.Time
}

// DefaultSecretTTL is how long a workspace signing secret is trusted in process.
//
// Short, and the number is a revocation bound rather than a performance one. An
// operator who clears the column to invalidate every signature a workspace has
// issued — the only revocation there is, and docs/SECURITY.md says so — has to
// wait for each replica's copy to expire. One minute is a wait somebody can sit
// through; caching for an hour would make the revocation something nobody could
// rely on having happened.
const DefaultSecretTTL = time.Minute

// DefaultDBTimeout bounds one gate query when the caller configures nothing.
//
// **Every query in this file is on the redirect path, and until F96 none of them
// was bounded by anything.** `RequestTimeout` wraps the application handler only;
// the redirect tree is mounted bare, deliberately, because `http.TimeoutHandler`
// buffers the response and would break the `Location` write and swallow the
// challenge page. There is no `statement_timeout` anywhere in this tree, and the
// pool sets connect and lifetime limits rather than per-query ones. So a query
// that stalled ran for as long as Postgres let it, holding a connection while
// requests queued behind it.
//
// The bound is per call and it lives here rather than around the handler, which
// is the shape `redirect.Resolver` already uses one package over — for the reason
// stated there: a query still running after the budget is not going to produce a
// useful answer, it is going to hold a connection while more requests queue. The
// number matches `REDIRECT_TIMEOUT`, which the resolver takes for the same path,
// and `Config.DBTimeout` is how the process passes it in.
//
// **None of these bounds detaches from the request context**, and that is the
// half worth stating. `Consume` writes: a client that disconnects mid-consume
// must not have spent a one-time link's only click on a redirect nobody
// received, so the cancellation has to reach Postgres. The resolver detaches
// because its result is shared by every waiter on a singleflight; nothing here is
// shared with anyone.
const DefaultDBTimeout = 250 * time.Millisecond

type Config struct {
	// Hasher verifies link passwords. Required for the password gate; a nil
	// Hasher makes every password check fail closed rather than pass.
	Hasher *auth.Hasher
	// SecretTTL overrides DefaultSecretTTL.
	SecretTTL time.Duration
	// DBTimeout overrides DefaultDBTimeout. The process passes REDIRECT_TIMEOUT.
	DBTimeout time.Duration
	// Now overrides the clock, for tests.
	Now func() time.Time
}

func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	if cfg.SecretTTL <= 0 {
		cfg.SecretTTL = DefaultSecretTTL
	}
	if cfg.DBTimeout <= 0 {
		cfg.DBTimeout = DefaultDBTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{
		q:         dbgen.New(pool),
		hasher:    cfg.Hasher,
		keys:      make(map[uuid.UUID]cachedSecret),
		keyTTL:    cfg.SecretTTL,
		now:       cfg.Now,
		dbTimeout: cfg.DBTimeout,
	}
}

// bounded returns ctx with this service's query budget applied.
//
// One helper rather than five spellings, so a query added later is bounded by
// having been written at all — the failure F96 recorded is a call that nobody
// remembered to wrap, and five copies of the same two lines is how that happens
// again. `context.WithTimeout` and never `context.WithoutCancel`: see
// DefaultDBTimeout.
func (s *Service) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.dbTimeout)
}

// --- password ---------------------------------------------------------------

// ErrNoPassword means the link carries no password hash, so there is nothing to
// verify against. Distinct from a mismatch: the caller answers 405 rather than
// re-serving the challenge, because a POST to a link with no password is a
// request the route does not accept rather than a wrong guess.
var ErrNoPassword = errors.New("gate: link has no password")

// VerifyPassword checks a submitted password against the link's stored hash.
//
// **The hash is read here and nowhere else.** It is deliberately absent from the
// cached snapshot — which carries a bare `HasPassword` boolean — so that
// whatever can read the cache cannot walk away with an offline cracking target
// for every password link on the instance. The cost of that decision is this
// query, and it lands only on a submitted password: rendering the challenge
// never runs it.
func (s *Service) VerifyPassword(ctx context.Context, linkID uuid.UUID, password string) (bool, error) {
	if s.hasher == nil {
		return false, errors.New("gate: no hasher configured")
	}
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	hash, err := s.q.GetLinkPasswordHash(ctx, linkID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNoPassword
		}
		return false, fmt.Errorf("read link password: %w", err)
	}
	if hash == nil || *hash == "" {
		return false, ErrNoPassword
	}
	switch err := s.hasher.Verify(password, *hash); {
	case err == nil:
		return true, nil
	case errors.Is(err, auth.ErrMismatch):
		return false, nil
	default:
		// A hash the decoder could not read. Reported rather than folded into
		// "wrong password", so a corrupt row is investigated instead of leaving
		// somebody convinced they have forgotten their own password.
		return false, fmt.Errorf("verify link password: %w", err)
	}
}

// --- click budget -----------------------------------------------------------

// ClickLimit is the number of clicks a snapshot's gates permit, and whether
// there is a limit at all.
//
// One function rather than a branch at the call site, because "one-time" and
// "max 5" are the same gate with different numbers and the redirect path should
// not have to know that. Both set is the smaller of the two: a one-time link
// with max_clicks=10 is a one-time link, and reading it any other way would let
// the wider setting quietly widen the narrower one.
func ClickLimit(oneTime bool, maxClicks *int64) (int64, bool) {
	limit := int64(0)
	have := false
	if maxClicks != nil {
		limit, have = *maxClicks, true
	}
	if oneTime && (!have || limit > 1) {
		limit, have = 1, true
	}
	return limit, have
}

// Consume spends one click of a link's durable budget, reporting whether there
// was one to spend.
//
// False means the link has been followed as often as it may be, and the caller
// answers 410 — the alias existed and is now spent, which is exactly what Gone
// is for and exactly what a crawler should stop retrying.
//
// Errors are **not** treated as exhaustion. A database that cannot answer is a
// failure of ours, and refusing a link over it would turn a blip into a
// permanent-looking 410 that link checkers act on; the caller answers 503
// instead. The direction is deliberate and it is the opposite of the fail-open
// choice the rate limiter makes, because the thing being protected is different:
// a limiter that under-counts costs an attacker a little less work, while a
// budget that miscounts either sends somebody to a destination they should not
// see or destroys a link that was fine.
func (s *Service) Consume(ctx context.Context, linkID, workspaceID uuid.UUID, limit int64) (bool, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	if _, err := s.q.ConsumeClickBudget(ctx, dbgen.ConsumeClickBudgetParams{
		LinkID: linkID, WorkspaceID: workspaceID, ClickLimit: limit,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("consume click budget: %w", err)
	}
	return true, nil
}

// --- sequential rotation (M36) ----------------------------------------------

// Rotate advances a link's sequential split and returns the position it
// advanced to, counting from one.
//
// The same durable counter argument as Consume, in a different column and
// without a limit: a rotation is not spent, it advances, and there is nothing
// for it to run out of. It lives in this service because the row it writes is
// the row Consume writes — one table, one upsert shape, one place where "a
// counter Redis cannot hold" is implemented.
//
// An error is returned rather than swallowed, and the caller answers 503. The
// alternative — choosing an arm anyway — would make the order approximate, which
// is the one property D8 refused.
func (s *Service) Rotate(ctx context.Context, linkID, workspaceID uuid.UUID) (int64, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	position, err := s.q.NextVariantRotation(ctx, dbgen.NextVariantRotationParams{
		LinkID: linkID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return 0, fmt.Errorf("advance split rotation: %w", err)
	}
	return position, nil
}

// Budget reports how much of a link's allowance has been spent, without
// spending any of it.
//
// Two callers, and the second one is why this is worth a sentence. The
// dashboard reads it to show a ceiling's remainder. The redirect path reads it
// for **HEAD only** — a request asking whether the link is alive, which must be
// answered 410 when the budget is gone and must not consume the budget in order
// to find out. A GET never calls it: Consume answers the same question by
// writing, and a read in front of that write would be a query on every gated
// redirect for information the write already returns.
func (s *Service) Budget(ctx context.Context, linkID uuid.UUID) (consumed int64, exhaustedAt *time.Time, err error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	row, err := s.q.GetClickBudget(ctx, linkID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No row means no click has been spent, which is a budget of zero
			// rather than a missing one.
			return 0, nil, nil
		}
		return 0, nil, fmt.Errorf("read click budget: %w", err)
	}
	return row.Consumed, row.ExhaustedAt, nil
}

// --- signing secrets --------------------------------------------------------

// Secret returns a workspace's signing secret, from the in-process cache when it
// is fresh.
//
// A workspace that has never minted one yields ErrNoSecret, and the negative is
// cached like the positive: an instance where nothing is signed must not answer
// a scanner's `?sig=` with a database query per request.
func (s *Service) Secret(ctx context.Context, workspaceID uuid.UUID) ([]byte, error) {
	now := s.now()

	s.keysMu.RLock()
	entry, ok := s.keys[workspaceID]
	s.keysMu.RUnlock()
	if ok && now.Before(entry.expires) {
		if len(entry.secret) == 0 {
			return nil, ErrNoSecret
		}
		return entry.secret, nil
	}

	qctx, cancel := s.bounded(ctx)
	defer cancel()
	secret, err := s.q.GetWorkspaceSigningSecret(qctx, workspaceID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read signing secret: %w", err)
	}
	s.cacheSecret(workspaceID, secret, now)
	if len(secret) == 0 {
		return nil, ErrNoSecret
	}
	return secret, nil
}

func (s *Service) cacheSecret(workspaceID uuid.UUID, secret []byte, now time.Time) {
	s.keysMu.Lock()
	s.keys[workspaceID] = cachedSecret{secret: secret, expires: now.Add(s.keyTTL)}
	s.keysMu.Unlock()
}

// EnsureSecret returns the workspace's signing secret, minting one on first use.
//
// Lazy rather than at workspace creation, so the column stays NULL for every
// workspace that never signs anything and the presence of a secret is itself a
// statement that somebody asked for one.
//
// **Not bounded by DBTimeout, and it is the only method here that is not.** This
// is the one call on the management path — minting a key is `links.update`
// through the API — so it is already inside `RequestTimeout`, and applying the
// redirect path's budget to it would give a dashboard write a 250ms ceiling it
// never had.
func (s *Service) EnsureSecret(ctx context.Context, workspaceID uuid.UUID) ([]byte, error) {
	candidate, err := NewSecret()
	if err != nil {
		return nil, err
	}
	secret, err := s.q.EnsureWorkspaceSigningSecret(ctx, dbgen.EnsureWorkspaceSigningSecretParams{
		Candidate: candidate, ID: workspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("workspace %s not found", workspaceID)
		}
		return nil, fmt.Errorf("mint signing secret: %w", err)
	}
	s.cacheSecret(workspaceID, secret, s.now())
	return secret, nil
}
