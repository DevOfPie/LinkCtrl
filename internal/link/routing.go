package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Routing rules (M34).
//
// **A rule is guarded by the link's own permissions and by no new one.** Editing
// where a link sends people is `links.update` however it is expressed — the URL
// field on the form and a country rule are the same authority over the same
// link, and inventing `rules.write` would mean an editor who can repoint a link
// outright cannot narrow it to one country. The inherited permission rule (D18)
// asks which limb a *new* permission matches; the answer here is that there is
// no new permission, which is the case that rule does not cover and the one
// worth saying out loud.
//
// **A rule's target is a destination row**, not a URL column, which is what
// 00600's dormant `destination_id` foreign key already said. That is what puts
// the M30 tier check, the normalized URL and the extracted host in the same one
// place they live for every other destination in the product — and it is why
// this file calls Service.checkDestination rather than validating a URL of its
// own. `surfaceRoutingRule` exists so the source scan in surfaces_test.go can
// see that it does.

// CreateRuleInput is a new rule.
type CreateRuleInput struct {
	// URL is where a matching visitor is sent. Judged by every tier before the
	// rule exists.
	URL string
	// Priority orders evaluation: lower wins. Defaulted to the column's 100 when
	// left at zero, so a caller that does not care gets the same priority as
	// every other caller that does not care, and ties break on creation order.
	Priority int32
	// Conditions is the condition set, already parsed and validated.
	Conditions domain.RuleConditions
	// Enabled is whether the rule is evaluated at all. A rule created disabled is
	// a rule somebody is drafting.
	Enabled bool
}

// UpdateRuleInput is a partial update; nil fields are left alone.
type UpdateRuleInput struct {
	URL        *string
	Priority   *int32
	Conditions *domain.RuleConditions
	Enabled    *bool
}

// ListRules returns a link's rules in the order the redirect path evaluates
// them.
func (s *Service) ListRules(ctx context.Context, actor *auth.Identity, linkID uuid.UUID) ([]domain.RoutingRule, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	// The link is read first so that a rule list for a link in another workspace
	// is a 404 rather than an empty list. An empty list is a claim that the link
	// exists and has no rules, which is information about somebody else's
	// tenancy.
	if _, err := s.q.GetLink(ctx, dbgen.GetLinkParams{ID: linkID, WorkspaceID: actor.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load link: %w", err)
	}

	rows, err := s.q.ListRoutingRules(ctx, dbgen.ListRoutingRulesParams{
		LinkID: linkID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("list routing rules: %w", err)
	}
	out := make([]domain.RoutingRule, 0, len(rows))
	for _, r := range rows {
		conds, err := decodeConditions(r.Conditions)
		if err != nil {
			return nil, fmt.Errorf("read conditions of rule %s: %w", r.ID, err)
		}
		out = append(out, domain.RoutingRule{
			ID: r.ID, LinkID: r.LinkID, Priority: r.Priority, URL: r.Url,
			Conditions: conds, Enabled: r.Enabled,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// CreateRule adds a rule to a link.
//
// Two rows in one transaction: the destination the rule points at, and the rule
// itself. Separately committed, a failure between them leaves either a rule
// with no target — which the CHECK added in migration 02000 refuses outright —
// or a destination nothing references, which is an orphan nobody can see or
// delete.
func (s *Service) CreateRule(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, in CreateRuleInput,
) (*domain.RoutingRule, error) {
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

	var errs domain.ValidationErrors
	if err := domain.ValidateRuleConditions(in.Conditions); err != nil {
		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			errs = append(errs, ve...)
		} else {
			return nil, err
		}
	}

	// The full M30 tier check, through the one door every destination-writing
	// surface goes through. Not ValidateDestination, which would inherit the
	// SSRF refusals and skip the embedded list, the operator's blocklist, the
	// heuristics, the opt-in feed and the `destination.blocked` audit record.
	normalized, err := s.checkDestination(ctx, actor, in.URL, surfaceRoutingRule)
	if err != nil {
		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			errs = append(errs, ve...)
		} else {
			return nil, err
		}
	}

	count, err := s.q.CountRoutingRules(ctx, linkID)
	if err != nil {
		return nil, fmt.Errorf("count routing rules: %w", err)
	}
	if count >= domain.MaxRulesPerLink {
		errs = append(errs, domain.FieldError{
			Field: "rules", Code: "too_many",
			Message: fmt.Sprintf(
				"a link may have at most %d routing rules; the whole list is "+
					"evaluated in order on every redirect", domain.MaxRulesPerLink),
		})
	}

	if len(errs) > 0 {
		return nil, errs
	}

	conds, err := json.Marshal(in.Conditions)
	if err != nil {
		return nil, fmt.Errorf("encode conditions: %w", err)
	}

	priority := in.Priority
	if priority == 0 {
		priority = 100
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	position, err := q.NextRuleDestinationPosition(ctx, linkID)
	if err != nil {
		return nil, fmt.Errorf("next destination position: %w", err)
	}
	destID := uuid.Must(uuid.NewV7())
	if _, err := q.CreateRuleDestination(ctx, dbgen.CreateRuleDestinationParams{
		ID: destID, LinkID: linkID, WorkspaceID: actor.WorkspaceID,
		Url: normalized, UrlHost: HostOf(normalized), Position: position,
	}); err != nil {
		return nil, fmt.Errorf("create rule destination: %w", err)
	}

	row, err := q.CreateRoutingRule(ctx, dbgen.CreateRoutingRuleParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: actor.WorkspaceID,
		DestinationID: &destID, Priority: priority, Conditions: conds,
		Enabled: in.Enabled,
	})
	if err != nil {
		return nil, fmt.Errorf("create routing rule: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.invalidateLink(ctx, link.DomainID, link.Alias)

	return &domain.RoutingRule{
		ID: row.ID, LinkID: row.LinkID, Priority: row.Priority, URL: normalized,
		Conditions: in.Conditions, Enabled: row.Enabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// UpdateRule changes a rule.
//
// The destination is updated in place rather than replaced, so a rule's target
// keeps its identity across an edit. Replacing it would mean a new row every
// time somebody fixes a typo, and every old one an orphan.
func (s *Service) UpdateRule(
	ctx context.Context, actor *auth.Identity, linkID, ruleID uuid.UUID, in UpdateRuleInput,
) (*domain.RoutingRule, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf("%w: editing links requires %s", domain.ErrForbidden, PermUpdate)
	}

	existing, err := s.q.GetRoutingRule(ctx, dbgen.GetRoutingRuleParams{
		ID: ruleID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load routing rule: %w", err)
	}
	// The rule is addressed through its link, so a rule that belongs to a
	// different one is a 404 rather than an edit that quietly works. Without
	// this, /links/{a}/rules/{rule-of-b} would edit b's rule and invalidate a's
	// cache — the wrong link's snapshot cleared and the right one's left stale.
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
	var conds []byte
	if in.Conditions != nil {
		if err := domain.ValidateRuleConditions(*in.Conditions); err != nil {
			var ve domain.ValidationErrors
			if errors.As(err, &ve) {
				errs = append(errs, ve...)
			} else {
				return nil, err
			}
		}
		if conds, err = json.Marshal(*in.Conditions); err != nil {
			return nil, fmt.Errorf("encode conditions: %w", err)
		}
	}

	var normalized string
	if in.URL != nil {
		if normalized, err = s.checkDestination(ctx, actor, *in.URL, surfaceRoutingRule); err != nil {
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

	row, err := q.UpdateRoutingRule(ctx, dbgen.UpdateRoutingRuleParams{
		ID: ruleID, WorkspaceID: actor.WorkspaceID,
		Priority: in.Priority, Conditions: conds, Enabled: in.Enabled,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("update routing rule: %w", err)
	}

	url := existing.Url
	if in.URL != nil && existing.DestinationID != nil {
		if err := q.UpdateRuleDestinationURL(ctx, dbgen.UpdateRuleDestinationURLParams{
			ID: *existing.DestinationID, WorkspaceID: actor.WorkspaceID,
			Url: normalized, UrlHost: HostOf(normalized),
		}); err != nil {
			return nil, fmt.Errorf("update rule destination: %w", err)
		}
		url = normalized
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.invalidateLink(ctx, link.DomainID, link.Alias)

	out, err := decodeConditions(row.Conditions)
	if err != nil {
		return nil, fmt.Errorf("read conditions: %w", err)
	}
	return &domain.RoutingRule{
		ID: row.ID, LinkID: row.LinkID, Priority: row.Priority, URL: url,
		Conditions: out, Enabled: row.Enabled,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// DeleteRule removes a rule and the destination it pointed at.
func (s *Service) DeleteRule(ctx context.Context, actor *auth.Identity, linkID, ruleID uuid.UUID) error {
	if !actor.Can(PermUpdate) {
		return fmt.Errorf("%w: editing links requires %s", domain.ErrForbidden, PermUpdate)
	}

	existing, err := s.q.GetRoutingRule(ctx, dbgen.GetRoutingRuleParams{
		ID: ruleID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("load routing rule: %w", err)
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

	destID, err := q.DeleteRoutingRule(ctx, dbgen.DeleteRoutingRuleParams{
		ID: ruleID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("delete routing rule: %w", err)
	}
	// Deleted in the same transaction as the rule. The guard inside the query
	// refuses to touch the link's own destination whatever it is handed.
	if destID != nil {
		if err := q.DeleteRuleDestination(ctx, dbgen.DeleteRuleDestinationParams{
			ID: *destID, WorkspaceID: actor.WorkspaceID,
		}); err != nil {
			return fmt.Errorf("delete rule destination: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	s.invalidateLink(ctx, link.DomainID, link.Alias)
	return nil
}

// invalidateLink drops the link's cached snapshot everywhere.
//
// Every rule write goes through here, and it is the whole of M34's answer to
// "rule CRUD invalidates snapshots across replicas". InvalidateAlias clears this
// process's memory tier, deletes the shared Redis entry with the retries a
// failed delete needs, and publishes on the M23 channel so the other replicas
// clear their own in-process copies. Without the publish, a rule change would
// take up to REDIRECT_TTL to reach a replica that had already cached the alias —
// and during that window two replicas would send the same visitor to two
// different places.
func (s *Service) invalidateLink(ctx context.Context, domainID uuid.UUID, alias string) {
	if s.cache == nil {
		return
	}
	s.cache.InvalidateAlias(ctx, domainID, alias)
}

// decodeConditions reads a stored condition set.
//
// Lenient where ParseRuleConditions is strict, and the asymmetry is deliberate:
// the strict parser guards what a *client* may send, and this reads bytes this
// program wrote. A row carrying a key a future build understands and this one
// does not should be read as far as it can be, not refused — refusing would turn
// a rollback into a link that cannot be listed.
func decodeConditions(raw []byte) (domain.RuleConditions, error) {
	var c domain.RuleConditions
	if len(raw) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	return c, nil
}
