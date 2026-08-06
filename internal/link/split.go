package link

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Split testing (M36).
//
// **A split arm is a routing rule with a different kind, and that is the whole
// design.** It is a `routing_rules` row pointing at a `destinations` row, in the
// same two-write transaction M34's CreateRule uses, invalidated by the same call,
// checked by the same `Service.checkDestination`. Nothing here is a parallel
// mechanism; it is the same mechanism with `kind` set to something other than
// `match`, which is what 00600's CHECK anticipated and what 02000's queries left
// room for by filtering on `match` everywhere.
//
// **Weights live on the destination**, not on the rule. That is the decision
// m36.md asks to have recorded and 00300 anticipated with a column and a comment
// in Phase 1. A weight is a property of the place traffic goes, not of the
// sentence that sends it there: two links pointing at the same page can weight it
// differently, an arm keeps its weight when its URL is edited, and the redirect
// path reads the weight from the same row it reads the URL from rather than
// joining two.
//
// **A link's arms are all one kind.** "40% of visitors, in rotation" is not a
// thing, and permitting the mix would mean the redirect path carrying a
// precedence rule for a state nobody meant to create. `domain.ValidateSplitKind`
// refuses it at the one door that creates arms.
//
// `surfaceSplitVariant` exists so the source scan in surfaces_test.go can see
// that this file calls checkDestination — that test is a bullet of this
// milestone, not a courtesy.

// CreateVariantInput is a new split arm.
type CreateVariantInput struct {
	// Kind is `weighted` or `sequential` for an arm, or `fallback`.
	Kind string
	// URL is where this arm sends people. Judged by every tier before the arm
	// exists.
	URL string
	// Weight is the arm's share of a weighted split. Ignored for the other two
	// kinds, which store the column's default and never read it.
	Weight int32
	// Enabled is whether the arm receives traffic at all. This is the feature
	// flag: an arm switched off stops being chosen on the next resolve and the
	// remaining arms re-share its traffic, with nothing deleted and no
	// attribution lost.
	Enabled bool
}

// UpdateVariantInput is a partial update; nil fields are left alone.
type UpdateVariantInput struct {
	URL     *string
	Weight  *int32
	Enabled *bool
}

// GetSplit returns a link's whole split test.
func (s *Service) GetSplit(ctx context.Context, actor *auth.Identity, linkID uuid.UUID) (*domain.Split, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	// The link is read first so that a split for a link in another workspace is a
	// 404 rather than an empty split — the same reasoning ListRules gives, and
	// the same information about somebody else's tenancy at stake.
	if _, err := s.q.GetLink(ctx, dbgen.GetLinkParams{ID: linkID, WorkspaceID: actor.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load link: %w", err)
	}
	return s.loadSplit(ctx, actor.WorkspaceID, linkID)
}

// loadSplit reads the split without re-checking the link, for callers that
// already have.
func (s *Service) loadSplit(ctx context.Context, workspaceID, linkID uuid.UUID) (*domain.Split, error) {
	return s.loadSplitWith(ctx, s.q, workspaceID, linkID)
}

// loadSplitWith is loadSplit against a caller-supplied queries handle, so the
// same read can be made inside a transaction that holds the link's lock.
//
// It exists because the split's two shape rules — one kind per link, one
// fallback — were decided on a read taken outside the transaction that writes,
// which is the same check-then-act the rule ceiling had (F67).
func (s *Service) loadSplitWith(
	ctx context.Context, q *dbgen.Queries, workspaceID, linkID uuid.UUID,
) (*domain.Split, error) {
	rows, err := q.ListVariantRules(ctx, dbgen.ListVariantRulesParams{
		LinkID: linkID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("list split variants: %w", err)
	}

	out := &domain.Split{Variants: make([]domain.Variant, 0, len(rows))}
	for _, r := range rows {
		v := domain.Variant{
			ID: r.ID, LinkID: r.LinkID, Kind: r.Kind, URL: r.Url,
			Weight: r.Weight, Enabled: r.Enabled, Position: r.Position,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		}
		if r.Kind == domain.RuleKindFallback {
			fallback := v
			out.Fallback = &fallback
			continue
		}
		if out.Kind == "" {
			out.Kind = r.Kind
		}
		out.Variants = append(out.Variants, v)
	}
	out.Variants = domain.Shares(out.Kind, out.Variants)
	return out, nil
}

// CreateVariant adds an arm to a link's split.
//
// Two rows in one transaction, for the reason CreateRule states: separately
// committed, a failure between them leaves either a rule the CHECK refuses or a
// destination nothing references.
func (s *Service) CreateVariant(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, in CreateVariantInput,
) (*domain.Variant, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf("%w: editing links requires %s", domain.ErrForbidden, PermUpdate)
	}

	link, err := s.q.GetLink(ctx, dbgen.GetLinkParams{ID: linkID, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load link: %w", err)
	}

	existing, err := s.loadSplit(ctx, actor.WorkspaceID, linkID)
	if err != nil {
		return nil, err
	}

	var errs domain.ValidationErrors
	if in.Kind == domain.RuleKindFallback {
		if existing.Fallback != nil {
			errs = append(errs, domain.FieldError{
				Field: "kind", Code: "conflict",
				Message: "this link already has a fallback destination; edit it rather " +
					"than adding a second, since only one can be the answer when " +
					"nothing else applies",
			})
		}
	} else {
		errs = append(errs, domain.ValidateSplitKind(in.Kind, existing.Kind)...)
		if len(existing.Variants) >= domain.MaxVariantsPerLink {
			errs = append(errs, variantCeilingError())
		}
	}
	errs = append(errs, domain.ValidateWeight(in.Weight)...)

	// The full M30 tier check, through the one door every destination-writing
	// surface goes through — not ValidateDestination, which would inherit the
	// SSRF refusals and skip the embedded list, the operator's blocklist, the
	// heuristics, the opt-in feed and the `destination.blocked` audit record.
	normalized, err := s.checkDestination(ctx, actor, in.URL, surfaceSplitVariant)
	if err != nil {
		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			errs = append(errs, ve...)
		} else {
			return nil, err
		}
	}

	// The same ceiling every rule on the link shares, because they share the
	// cached snapshot they travel in and the walk the redirect path makes over
	// it. CountRoutingRules counts every kind, which M34 wrote it to do for
	// exactly this milestone.
	count, err := s.q.CountRoutingRules(ctx, linkID)
	if err != nil {
		return nil, fmt.Errorf("count routing rules: %w", err)
	}
	if count >= domain.MaxRulesPerLink {
		errs = append(errs, combinedCeilingError())
	}

	if len(errs) > 0 {
		return nil, errs
	}

	weight := in.Weight
	if in.Kind != domain.RuleKindWeighted {
		// The column's default rather than whatever was posted. A number nothing
		// reads is a number somebody will eventually believe, and a sequential arm
		// showing "weight 30" beside a rotation is exactly that.
		weight = defaultDestinationWeight
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// Every shape rule this write depends on, re-decided inside the transaction
	// that writes, against a locked link. All three were read above on a
	// connection with nothing holding the state still, so two creates a few
	// milliseconds apart each saw a link that had room and both wrote — a
	// second fallback, a second kind, or one arm past the ceiling, none of
	// which either request would have produced alone (F67).
	//
	// The checks above are not redundant: they are what lets a caller learn
	// about a full link alongside a bad destination, in one answer. These are
	// what make the rules true.
	if err := s.lockLinkForRules(ctx, q, linkID); err != nil {
		return nil, err
	}
	settled, err := s.loadSplitWith(ctx, q, actor.WorkspaceID, linkID)
	if err != nil {
		return nil, err
	}
	switch {
	case in.Kind == domain.RuleKindFallback && settled.Fallback != nil:
		return nil, domain.ValidationErrors{{
			Field: "kind", Code: "conflict",
			Message: "this link already has a fallback destination; edit it rather " +
				"than adding a second, since only one can be the answer when " +
				"nothing else applies",
		}}
	case in.Kind != domain.RuleKindFallback:
		if ve := domain.ValidateSplitKind(in.Kind, settled.Kind); len(ve) > 0 {
			return nil, ve
		}
		if len(settled.Variants) >= domain.MaxVariantsPerLink {
			return nil, domain.ValidationErrors{variantCeilingError()}
		}
	}
	count, err = q.CountRoutingRules(ctx, linkID)
	if err != nil {
		return nil, fmt.Errorf("count routing rules: %w", err)
	}
	if count >= domain.MaxRulesPerLink {
		return nil, domain.ValidationErrors{combinedCeilingError()}
	}

	position, err := q.NextRuleDestinationPosition(ctx, linkID)
	if err != nil {
		return nil, fmt.Errorf("next destination position: %w", err)
	}
	destID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateVariantDestination(ctx, dbgen.CreateVariantDestinationParams{
		ID: destID, LinkID: linkID, WorkspaceID: actor.WorkspaceID,
		Url: normalized, UrlHost: HostOf(normalized), Position: position, Weight: weight,
	}); err != nil {
		return nil, fmt.Errorf("create variant destination: %w", err)
	}

	row, err := q.CreateVariantRule(ctx, dbgen.CreateVariantRuleParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: actor.WorkspaceID,
		DestinationID: &destID, Kind: in.Kind, Enabled: in.Enabled,
	})
	if err != nil {
		return nil, fmt.Errorf("create variant rule: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.invalidateLink(ctx, link.DomainID, link.Alias)

	return &domain.Variant{
		ID: row.ID, LinkID: row.LinkID, Kind: row.Kind, URL: normalized,
		Weight: weight, Enabled: row.Enabled, Position: position,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// defaultDestinationWeight is `destinations.weight`'s column default, restated
// here so a kind that does not use weights writes the same number the column
// would have written for it.
const defaultDestinationWeight int32 = 100

// UpdateVariant changes an arm's destination, weight or enabled flag.
//
// The destination is updated in place rather than replaced, so an arm keeps its
// identity — and therefore its clicks — across an edit. Replacing it would split
// a running test's history in two every time somebody fixed a typo.
func (s *Service) UpdateVariant(
	ctx context.Context, actor *auth.Identity, linkID, variantID uuid.UUID, in UpdateVariantInput,
) (*domain.Variant, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf("%w: editing links requires %s", domain.ErrForbidden, PermUpdate)
	}

	existing, err := s.q.GetVariantRule(ctx, dbgen.GetVariantRuleParams{
		ID: variantID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load split variant: %w", err)
	}
	// Addressed through its link, so an arm belonging to a different one is a 404
	// rather than an edit that quietly works on somebody else's link and
	// invalidates the wrong cache entry.
	if existing.LinkID != linkID {
		return nil, domain.ErrNotFound
	}
	link, err := s.q.GetLink(ctx, dbgen.GetLinkParams{
		ID: existing.LinkID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("load link: %w", err)
	}

	var errs domain.ValidationErrors
	if in.Weight != nil {
		errs = append(errs, domain.ValidateWeight(*in.Weight)...)
	}

	var normalized string
	if in.URL != nil {
		if normalized, err = s.checkDestination(ctx, actor, *in.URL, surfaceSplitVariant); err != nil {
			var ve domain.ValidationErrors
			if errors.As(err, &ve) {
				errs = append(errs, ve...)
			} else {
				return nil, err
			}
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.UpdateVariantRule(ctx, dbgen.UpdateVariantRuleParams{
		ID: variantID, WorkspaceID: actor.WorkspaceID, Enabled: in.Enabled,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("update split variant: %w", err)
	}

	url, weight := existing.Url, existing.Weight
	if existing.DestinationID != nil && (in.URL != nil || in.Weight != nil) {
		params := dbgen.UpdateVariantDestinationParams{
			ID: *existing.DestinationID, WorkspaceID: actor.WorkspaceID,
		}
		if in.URL != nil {
			host := HostOf(normalized)
			params.Url, params.UrlHost = &normalized, &host
			url = normalized
		}
		// Only for a weighted arm. Letting a weight be set on a sequential one
		// would put a number in the column that the redirect path never reads and
		// the dashboard would have to explain.
		if in.Weight != nil && existing.Kind == domain.RuleKindWeighted {
			params.Weight = in.Weight
			weight = *in.Weight
		}
		if err := q.UpdateVariantDestination(ctx, params); err != nil {
			return nil, fmt.Errorf("update variant destination: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.invalidateLink(ctx, link.DomainID, link.Alias)

	return &domain.Variant{
		ID: row.ID, LinkID: row.LinkID, Kind: row.Kind, URL: url, Weight: weight,
		Enabled: row.Enabled, Position: existing.Position,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// DeleteVariant removes an arm and the destination it pointed at.
//
// The clicks already attributed to that destination stay in click_events with an
// id that no longer resolves, and the breakdown reports them as a destination
// that no longer exists. That is deliberate: silently dropping them would make a
// running test's totals change when somebody tidied up.
func (s *Service) DeleteVariant(ctx context.Context, actor *auth.Identity, linkID, variantID uuid.UUID) error {
	if !actor.Can(PermUpdate) {
		return fmt.Errorf("%w: editing links requires %s", domain.ErrForbidden, PermUpdate)
	}

	existing, err := s.q.GetVariantRule(ctx, dbgen.GetVariantRuleParams{
		ID: variantID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("load split variant: %w", err)
	}
	if existing.LinkID != linkID {
		return domain.ErrNotFound
	}
	link, err := s.q.GetLink(ctx, dbgen.GetLinkParams{
		ID: existing.LinkID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		return fmt.Errorf("load link: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	destID, err := q.DeleteVariantRule(ctx, dbgen.DeleteVariantRuleParams{
		ID: variantID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("delete split variant: %w", err)
	}
	// The same guarded delete a rule target gets: the query refuses to touch the
	// link's own destination whatever it is handed.
	if destID != nil {
		if err := q.DeleteRuleDestination(ctx, dbgen.DeleteRuleDestinationParams{
			ID: *destID, WorkspaceID: actor.WorkspaceID,
		}); err != nil {
			return fmt.Errorf("delete variant destination: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	s.invalidateLink(ctx, link.DomainID, link.Alias)
	return nil
}

// variantCeilingError and combinedCeilingError are the single statements of the
// two limits a split arm has to clear.
//
// Functions rather than literals, for ruleCeilingError's reason: each is now
// checked twice per write — once outside the transaction so a caller learns
// about it beside every other validation error, and once inside against a
// locked link so the limit is actually a limit (F67). Two copies of the wording
// would be two things to keep in step, and which one a caller saw would depend
// on how close the race was.
func variantCeilingError() domain.FieldError {
	return domain.FieldError{
		Field: "variants", Code: "too_many",
		Message: fmt.Sprintf("a link may have at most %d split arms; past that "+
			"a test does not produce a result anybody can act on",
			domain.MaxVariantsPerLink),
	}
}

func combinedCeilingError() domain.FieldError {
	return domain.FieldError{
		Field: "variants", Code: "too_many",
		Message: fmt.Sprintf(
			"a link may have at most %d rules and split arms together; the whole "+
				"list travels in the cached snapshot", domain.MaxRulesPerLink),
	}
}
