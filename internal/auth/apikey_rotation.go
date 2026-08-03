package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Rotation, per decision D9.
//
// The tension this resolves is recorded rather than dodged. `apikeys.*` is
// non-delegable precisely so a credential can never mint another credential —
// otherwise revoking a leaked key means nothing, because whoever leaked it
// issued a second one first. Rotation is the one thing a key must nevertheless
// be able to do without a human, because the alternative is a credential that
// can only be replaced by somebody signing in, which is not a thing an unattended
// deployment can arrange at 3am.
//
// So rotation is not "a key minting a key". It is a key replacing **itself**:
//
//   - only its own row, addressed by the token that authenticated the request;
//     the endpoint takes no id, because taking one would imply otherwise
//   - into scopes that are a subset of its own
//   - with the same workspace binding, copied verbatim
//   - once — a key that already has a successor refuses, and a unique index
//     holds that in the database as well, so the lineage is a chain
//
// Nothing there widens anything, which is what makes it safe to leave in a
// credential's hands. `apikeys.write` is still not a scope any key may hold, and
// `TestNonDelegableScopesCoverKeyManagement` is what says so.
//
// The accepted trade, stated rather than buried: **a leaked key can persist
// across rotations.** Whoever holds the secret can rotate it, so revoking the
// key the owner knows about does not necessarily end the intruder's access — they
// hold a successor the owner never saw. It is finite rather than unbounded
// because every generation appears in the owner's key list and the chain is
// visible there, but it is real, and it is the price of unattended rotation. The
// alternative considered was session-only rotation, which is what the product
// already had: mint a new key by hand. That leaves the limitation unsolved.
const (
	// DefaultRotationGrace is how long both secrets verify when the caller does
	// not say. An hour is long enough for a deploy to reach every consumer of
	// the credential and short enough that a rotation nobody finished is not a
	// second live key for the rest of the week.
	DefaultRotationGrace = time.Hour

	// MinRotationGrace is the floor, and it exists because of `last_used_at`.
	//
	// The obvious way to check a rotation landed is to watch whether anything
	// still uses the old key — and `last_used_at` is buffered and flushed on a
	// 30s cadence, so a predecessor that reads as idle may have been used up to
	// 30 seconds ago. A grace window measured in seconds would close before that
	// answer was even available. Five minutes is an order of magnitude above the
	// flush interval, which is what makes the reading mean something.
	MinRotationGrace = 5 * time.Minute

	// MaxRotationGrace is the ceiling, and it is the thing that keeps D9's
	// accepted trade finite. A leaked key persisting across rotations is
	// tolerable because each predecessor stops verifying; an unbounded window
	// would make "stops verifying" a promise about the heat death of the
	// universe.
	MaxRotationGrace = 24 * time.Hour
)

// RotateAPIKeyInput describes a rotation. Every field is optional, and the
// zero value is the common case: same scopes, default grace.
type RotateAPIKeyInput struct {
	// Scopes narrows the successor. Nil means "identical to the predecessor's".
	// A scope the predecessor does not hold is refused rather than trimmed,
	// because silently dropping it would let a caller believe it was granted.
	Scopes []string
	// Grace is how long the predecessor keeps verifying. Zero means
	// DefaultRotationGrace; anything outside [MinRotationGrace, MaxRotationGrace]
	// is refused.
	Grace time.Duration
}

// RotatedPredecessor is what the caller needs to know about the key it just
// replaced: which one it was, and the deadline it now has.
type RotatedPredecessor struct {
	ID     uuid.UUID `json:"id"`
	Prefix string    `json:"prefix"`
	// StopsWorkingAt is the far edge of the grace window. After it the
	// predecessor authenticates nothing, whether or not housekeeping has got
	// round to writing its revocation.
	StopsWorkingAt time.Time `json:"stops_working_at"`
}

// RotatedAPIKey is the successor, plus the fate of the key it replaced.
type RotatedAPIKey struct {
	CreatedAPIKey
	Predecessor RotatedPredecessor `json:"predecessor"`
}

// APIKeyRotation is one rotation, as the audit log records it.
type APIKeyRotation struct {
	PredecessorID     uuid.UUID
	PredecessorPrefix string
	SuccessorID       uuid.UUID
	SuccessorPrefix   string
	GraceExpiresAt    time.Time
	Scopes            []string
	// ScopesNarrowed says the successor holds fewer scopes than the key it
	// replaced. Recorded because the interesting rotation to find afterwards is
	// the one that changed what the credential could do.
	ScopesNarrowed bool
	OrgWide        bool
}

// APIKeyAuditor records key-lifecycle events.
//
// Declared here as an interface rather than taken as an *audit.Service, because
// internal/audit imports internal/auth — the writer resolves an actor into the
// label it stores — so the dependency runs one way and this is the seam.
// *audit.Service satisfies it.
type APIKeyAuditor interface {
	RecordAPIKeyRotation(ctx context.Context, actor *Identity, ev APIKeyRotation) error
}

// ErrAPIKeyAlreadyRotated is the refusal a second rotation of one key gets.
//
// Distinct from ErrAPIKeyInvalid, and deliberately so: the caller holding this
// key is its legitimate owner — it just authenticated — and telling it "invalid"
// would send an automated rotation into a retry loop against a key that is
// working perfectly well. The successor exists; whoever asked has lost it, and
// that is a different problem from a bad credential.
var ErrAPIKeyAlreadyRotated = errors.New("auth: this api key has already been rotated")

// Rotate issues the successor to the key the request authenticated with.
//
// Returns the only copy of the successor's token that will ever exist, exactly
// as Create does, and the deadline the predecessor now carries.
func (s *APIKeyService) Rotate(ctx context.Context, actor *Identity, in RotateAPIKeyInput) (*RotatedAPIKey, error) {
	if actor == nil {
		return nil, domain.ErrUnauthorized
	}
	// The inverse of requireSessionActor, and the only operation in this service
	// that has it. Rotation is a key replacing itself, so the key's own token is
	// the authorization: there is no id to name and no permission to hold. A
	// session that wants another key mints one, which it has always been able to
	// do.
	if !actor.IsAPIKey() {
		return nil, fmt.Errorf(
			"%w: rotation replaces the key that made the request, so it needs that key's own token rather than a signed-in session",
			domain.ErrForbidden)
	}

	grace := in.Grace
	if grace == 0 {
		grace = DefaultRotationGrace
	}
	if grace < MinRotationGrace || grace > MaxRotationGrace {
		return nil, domain.ValidationErrors{{
			Field: "grace", Code: "out_of_range",
			Message: fmt.Sprintf(
				"the grace window must be between %s and %s; below the floor it closes before last_used_at has flushed, and above the ceiling the key it replaces outlives any incident it was rotated for",
				MinRotationGrace, MaxRotationGrace),
		}}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin rotation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	pred, err := q.GetAPIKeyForRotation(ctx, *actor.APIKeyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAPIKeyInvalid
		}
		return nil, fmt.Errorf("load key for rotation: %w", err)
	}
	if pred.SuccessorID != nil {
		return nil, ErrAPIKeyAlreadyRotated
	}
	// Re-checked under the lock. Authentication passed a moment ago, but a
	// revocation landing between then and here must win — otherwise revoking a
	// key racing its own rotation leaves a successor nobody revoked.
	now := time.Now()
	if pred.RevokedAt != nil ||
		(pred.ExpiresAt != nil && !pred.ExpiresAt.After(now)) ||
		(pred.GraceExpiresAt != nil && !pred.GraceExpiresAt.After(now)) {
		return nil, ErrAPIKeyInvalid
	}

	scopes, errs := narrowScopes(pred.Scopes, in.Scopes)
	if len(errs) > 0 {
		return nil, errs
	}

	graceEnds := now.Add(grace)
	successorID := uuid.Must(uuid.NewV7())

	row, token, err := s.insertSuccessor(ctx, tx, dbgen.CreateAPIKeyParams{
		ID:             successorID,
		UserID:         pred.UserID,
		OrganizationID: pred.OrganizationID,
		// Copied verbatim, NULL included. A rotation cannot move a key between
		// workspaces and cannot turn a workspace-bound key into an
		// organization-wide one — that choice is the creator's, and it is made
		// once.
		WorkspaceID: pred.WorkspaceID,
		Name:        pred.Name,
		Scopes:      scopes,
		ExpiresAt:   successorExpiry(pred.CreatedAt, pred.ExpiresAt, now),
	})
	if err != nil {
		return nil, err
	}

	n, err := q.MarkAPIKeyRotated(ctx, dbgen.MarkAPIKeyRotatedParams{
		ID: pred.ID, GraceExpiresAt: &graceEnds, SuccessorID: &successorID,
	})
	if err != nil {
		return nil, fmt.Errorf("mark key rotated: %w", err)
	}
	if n == 0 {
		return nil, ErrAPIKeyAlreadyRotated
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit rotation: %w", err)
	}

	out := &RotatedAPIKey{
		CreatedAPIKey: CreatedAPIKey{APIKeyInfo: keyInfo(row), Key: token},
		Predecessor: RotatedPredecessor{
			ID: pred.ID, Prefix: pred.Prefix, StopsWorkingAt: graceEnds,
		},
	}

	// After the commit, and logged rather than returned on failure: losing the
	// record of a rotation is worse than nothing, but losing the rotation itself
	// after the successor's token has been minted would be worse still — the
	// caller would never see the only copy of a credential that now exists.
	if s.auditor != nil {
		ev := APIKeyRotation{
			PredecessorID: pred.ID, PredecessorPrefix: pred.Prefix,
			SuccessorID: row.ID, SuccessorPrefix: row.Prefix,
			GraceExpiresAt: graceEnds,
			Scopes:         scopes,
			ScopesNarrowed: len(scopes) < len(pred.Scopes),
			OrgWide:        pred.WorkspaceID == nil,
		}
		if err := s.auditor.RecordAPIKeyRotation(ctx, actor, ev); err != nil {
			s.log.Warn("api key rotated but the audit record was not written",
				slog.String("prefix", pred.Prefix), slog.Any("error", err))
		}
	}
	return out, nil
}

// insertSuccessor writes the successor row, retrying a prefix collision on a
// savepoint.
//
// Create retries the same collision by simply looping, because it runs on the
// pool and a failed statement there costs nothing. Inside a transaction it costs
// the transaction: a failed INSERT aborts it, and every later statement — the
// UPDATE that closes the predecessor, the COMMIT — fails with "current
// transaction is aborted" rather than doing anything. pgx's nested Begin is a
// SAVEPOINT, which is the only thing that makes the second attempt reachable.
//
// Prefix and secret are generated per attempt rather than once, because a
// collision means *this* prefix is taken.
func (s *APIKeyService) insertSuccessor(
	ctx context.Context, tx pgx.Tx, params dbgen.CreateAPIKeyParams,
) (dbgen.ApiKey, string, error) {
	var zero dbgen.ApiKey
	for attempt := range 3 {
		token, prefix, secret, err := newAPIKeyToken()
		if err != nil {
			return zero, "", err
		}
		params.Prefix = prefix
		params.KeyHash = APIKeyHash(s.pepper, prefix, secret)

		sp, err := tx.Begin(ctx)
		if err != nil {
			return zero, "", fmt.Errorf("savepoint for successor key: %w", err)
		}
		row, err := s.q.WithTx(sp).CreateAPIKey(ctx, params)
		if err != nil {
			_ = sp.Rollback(ctx)
			if isUniqueViolation(err) && attempt < 2 {
				continue
			}
			return zero, "", fmt.Errorf("create successor key: %w", err)
		}
		if err := sp.Commit(ctx); err != nil {
			return zero, "", fmt.Errorf("release savepoint for successor key: %w", err)
		}
		return row, token, nil
	}
	return zero, "", errors.New("auth: could not allocate an unused api key prefix")
}

// narrowScopes resolves the successor's scopes against the predecessor's.
//
// The predecessor's stored scopes are the ceiling, not the actor's current
// permissions. Those two differ whenever the owner has been demoted since the
// key was minted — `restrictTo` intersects on every request, so the identity
// holds less than the row says — and taking the intersection here would make a
// rotation quietly *narrow* the key by an amount that would come back if the
// owner were re-promoted. Identical means identical: the row's scopes, copied.
//
// Non-delegability is re-checked even for an unchanged set, and that is the one
// place this deliberately does not copy. A permission that became non-delegable
// after the key was minted must not ride a rotation into a new credential; the
// refusal names it so the caller can rotate into a set without it.
func narrowScopes(held, requested []string) ([]string, domain.ValidationErrors) {
	ceiling := make(map[string]struct{}, len(held))
	for _, s := range held {
		ceiling[s] = struct{}{}
	}

	var errs domain.ValidationErrors
	for _, s := range held {
		if isNonDelegable(s) {
			errs = append(errs, domain.FieldError{
				Field: "scopes", Code: "not_delegable",
				Message: fmt.Sprintf(
					"%q is no longer grantable to an API key, so it cannot be carried into a successor; rotate into a set that omits it",
					s),
			})
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	if requested == nil {
		out := make([]string, len(held))
		copy(out, held)
		return out, nil
	}

	seen := make(map[string]struct{}, len(requested))
	out := make([]string, 0, len(requested))
	for _, raw := range requested {
		scope := strings.TrimSpace(raw)
		if scope == "" {
			continue
		}
		if _, dup := seen[scope]; dup {
			continue
		}
		seen[scope] = struct{}{}

		if _, ok := ceiling[scope]; !ok {
			errs = append(errs, domain.FieldError{
				Field: "scopes", Code: "not_held",
				Message: fmt.Sprintf(
					"this key does not hold %q, and a rotation cannot add one; a successor is identical or narrower",
					scope),
			})
			continue
		}
		out = append(out, scope)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if len(out) == 0 {
		return nil, domain.ValidationErrors{{
			Field: "scopes", Code: "required",
			Message: "at least one scope is required; a successor with no scopes could do nothing, and revoking this key is the way to say that",
		}}
	}
	return out, nil
}

// successorExpiry gives the successor the predecessor's **lifetime**, measured
// from now.
//
// Not its deadline, which would produce a successor expiring at the same instant
// as the key it replaced and make rotating an expiring key pointless. Not an
// unbounded one either: a key created to live 30 days rotates into another key
// that lives 30 days, and a key created never to expire rotates into one that
// never expires.
//
// This is the one dimension a rotation deliberately refreshes, and it is worth
// being explicit that it is a refresh. Scopes and workspace cannot widen; the
// deadline moves, because moving it is what rotation is for.
func successorExpiry(createdAt time.Time, expiresAt *time.Time, now time.Time) *time.Time {
	if expiresAt == nil {
		return nil
	}
	lifetime := expiresAt.Sub(createdAt)
	if lifetime <= 0 {
		// A key whose expiry was never after its creation cannot happen through
		// Create, which refuses a past expiry. Handled anyway rather than
		// producing a successor already dead: the floor is the grace window, so
		// a rotation always yields something usable or nothing at all.
		return nil
	}
	at := now.Add(lifetime)
	return &at
}
