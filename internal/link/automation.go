package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// Automation rules (M43): the writing and reading half.
//
// **Evaluation is not here.** This file creates, edits, audits and lists rules;
// internal/automation walks them on the scheduler and does what they say. The
// two never import each other — the same one-way split M42 made between
// registering a webhook and delivering one, and for the same reason: the thing
// that authorizes a standing instruction and the thing that carries it out have
// almost nothing in common, and joining them would put a permission check inside
// a job with no actor.
//
// This half is here rather than in a package of its own because a rule is a
// workspace resource with an owner, a permission and an audit trail, which is
// exactly what every other collection in this package is, and because
// ArchiveByRule below has to reach the same statements the interactive archive
// reaches.

// Permissions this file enforces.
//
// Their own rather than `links.*`, which is the fork D75 and D80 have already
// been at. A QR code is a property of a link, so whoever may edit the link may
// edit it. An automation rule is a property of nothing: it runs unattended, on a
// clock, and can archive links and make this server connect to an address the
// workspace chose. See decisions.md for which limb of D18 the write half
// matched.
const (
	PermAutomationRead  = "automation.read"
	PermAutomationWrite = "automation.write"
)

// CreateAutomationRuleInput is a new standing instruction.
type CreateAutomationRuleInput struct {
	Name          string
	Trigger       string
	TriggerConfig domain.AutomationTriggerConfig
	Actions       []string
	// Enabled is whether the scheduler evaluates it. A disabled rule is skipped
	// by ListDueAutomationRules rather than evaluated and discarded.
	Enabled bool
}

// UpdateAutomationRuleInput is a partial update; nil fields are left alone.
type UpdateAutomationRuleInput struct {
	Name          *string
	Trigger       *string
	TriggerConfig *domain.AutomationTriggerConfig
	Actions       []string
	Enabled       *bool
}

// AutomationRules lists a workspace's rules.
func (s *Service) AutomationRules(ctx context.Context, actor *auth.Identity) ([]domain.AutomationRule, error) {
	if !actor.Can(PermAutomationRead) {
		return nil, fmt.Errorf("%w: reading automation rules requires %s",
			domain.ErrForbidden, PermAutomationRead)
	}
	rows, err := s.q.ListAutomationRules(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("list automation rules: %w", err)
	}
	out := make([]domain.AutomationRule, 0, len(rows))
	for _, r := range rows {
		rule, err := automationRuleFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, nil
}

// AutomationRule reads one rule.
func (s *Service) AutomationRule(
	ctx context.Context, actor *auth.Identity, id uuid.UUID,
) (*domain.AutomationRule, error) {
	if !actor.Can(PermAutomationRead) {
		return nil, fmt.Errorf("%w: reading automation rules requires %s",
			domain.ErrForbidden, PermAutomationRead)
	}
	r, err := s.q.GetAutomationRule(ctx, dbgen.GetAutomationRuleParams{
		ID: id, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load automation rule: %w", err)
	}
	rule, err := automationRuleFromRow(r)
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

// CreateAutomationRule writes a rule and arms it.
//
// **Armed at creation, not at first firing.** The watermark is set to now, so
// the rule acts on what happens after it exists. A NULL watermark would mean
// "everything that ever happened", and the first run of a rule somebody created
// this afternoon would archive every link that expired last year.
func (s *Service) CreateAutomationRule(
	ctx context.Context, actor *auth.Identity, in CreateAutomationRuleInput,
) (*domain.AutomationRule, error) {
	if !actor.Can(PermAutomationWrite) {
		return nil, fmt.Errorf("%w: managing automation rules requires %s",
			domain.ErrForbidden, PermAutomationWrite)
	}

	var errs domain.ValidationErrors
	errs = append(errs, domain.ValidateAutomationRuleName(in.Name)...)
	errs = append(errs, domain.ValidateAutomationTrigger(in.Trigger)...)
	errs = append(errs, domain.ValidateAutomationTriggerConfig(in.TriggerConfig)...)
	actions, actionErrs := domain.ValidateAutomationActions(in.Trigger, in.Actions)
	errs = append(errs, actionErrs...)

	count, err := s.q.CountAutomationRules(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("count automation rules: %w", err)
	}
	if count >= domain.MaxAutomationRulesPerWorkspace {
		errs = append(errs, domain.FieldError{
			Field: "name", Code: "too_many",
			Message: fmt.Sprintf("a workspace may have at most %d automation rules; "+
				"every enabled one is work the scheduler does on every tick",
				domain.MaxAutomationRulesPerWorkspace),
		})
	}
	if len(errs) > 0 {
		return nil, errs
	}

	config, actionsJSON, err := encodeAutomationRule(in.TriggerConfig, actions)
	if err != nil {
		return nil, err
	}
	armed := time.Now().UTC()

	row, err := s.q.CreateAutomationRule(ctx, dbgen.CreateAutomationRuleParams{
		ID: uuid.Must(uuid.NewV7()), WorkspaceID: actor.WorkspaceID,
		Name: in.Name, Trigger: in.Trigger, TriggerConfig: config,
		Actions: actionsJSON, Enabled: in.Enabled, LastFiredAt: &armed,
	})
	if err != nil {
		return nil, fmt.Errorf("create automation rule: %w", err)
	}

	s.recordAutomationEvent(ctx, actor, audit.ActionAutomationRuleCreated, row.ID, map[string]any{
		"name":      row.Name,
		"trigger":   row.Trigger,
		"actions":   actions,
		"enabled":   row.Enabled,
		"min_count": in.TriggerConfig.Threshold(),
	})

	rule, err := automationRuleFromRow(row)
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

// UpdateAutomationRule changes a rule's name, trigger, threshold, actions or
// switch.
//
// The whole rule is re-validated, not only what changed: an action list that was
// legal against the old trigger can be illegal against the new one — `archive_link`
// on a rule moved to `destination.blocked` has no link to archive — and an edit
// that changed one field must not leave the row in a state a create would refuse.
func (s *Service) UpdateAutomationRule(
	ctx context.Context, actor *auth.Identity, id uuid.UUID, in UpdateAutomationRuleInput,
) (*domain.AutomationRule, error) {
	if !actor.Can(PermAutomationWrite) {
		return nil, fmt.Errorf("%w: managing automation rules requires %s",
			domain.ErrForbidden, PermAutomationWrite)
	}
	existing, err := s.AutomationRule(ctx, actor, id)
	if err != nil {
		return nil, err
	}

	merged := *existing
	if in.Name != nil {
		merged.Name = *in.Name
	}
	if in.Trigger != nil {
		merged.Trigger = *in.Trigger
	}
	if in.TriggerConfig != nil {
		merged.TriggerConfig = *in.TriggerConfig
	}
	if in.Actions != nil {
		merged.Actions = in.Actions
	}

	var errs domain.ValidationErrors
	errs = append(errs, domain.ValidateAutomationRuleName(merged.Name)...)
	errs = append(errs, domain.ValidateAutomationTrigger(merged.Trigger)...)
	errs = append(errs, domain.ValidateAutomationTriggerConfig(merged.TriggerConfig)...)
	actions, actionErrs := domain.ValidateAutomationActions(merged.Trigger, merged.Actions)
	errs = append(errs, actionErrs...)
	if len(errs) > 0 {
		return nil, errs
	}

	config, actionsJSON, err := encodeAutomationRule(merged.TriggerConfig, actions)
	if err != nil {
		return nil, err
	}

	// The re-arm instant. The statement uses it only on the disabled-to-enabled
	// transition; passing it unconditionally keeps that decision in one place
	// (the SQL) rather than splitting it across two.
	rearmed := time.Now().UTC()

	row, err := s.q.UpdateAutomationRule(ctx, dbgen.UpdateAutomationRuleParams{
		ID: id, WorkspaceID: actor.WorkspaceID,
		Name: &merged.Name, Trigger: &merged.Trigger,
		TriggerConfig: config, Actions: actionsJSON,
		Enabled: in.Enabled, RearmedAt: &rearmed,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("update automation rule: %w", err)
	}

	s.recordAutomationEvent(ctx, actor, audit.ActionAutomationRuleUpdated, row.ID, map[string]any{
		"name":      row.Name,
		"trigger":   row.Trigger,
		"actions":   actions,
		"enabled":   row.Enabled,
		"min_count": merged.TriggerConfig.Threshold(),
	})

	rule, err := automationRuleFromRow(row)
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

// DeleteAutomationRule removes a rule.
func (s *Service) DeleteAutomationRule(ctx context.Context, actor *auth.Identity, id uuid.UUID) error {
	if !actor.Can(PermAutomationWrite) {
		return fmt.Errorf("%w: managing automation rules requires %s",
			domain.ErrForbidden, PermAutomationWrite)
	}
	n, err := s.q.DeleteAutomationRule(ctx, dbgen.DeleteAutomationRuleParams{
		ID: id, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		return fmt.Errorf("delete automation rule: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	s.recordAutomationEvent(ctx, actor, audit.ActionAutomationRuleDeleted, id, nil)
	return nil
}

// --- what a rule does --------------------------------------------------------

// ArchiveByRule archives a link because a rule said so.
//
// **No actor, and that is the whole difference from Archive.** The interactive
// path authorizes against a signed-in identity holding `links.delete`; this one
// is authorized by the rule, which somebody holding `automation.write` created.
// Building a synthetic identity that holds `links.delete` would have been the
// other way to do it, and it is worse: `auth.Identity`'s permission set is
// private precisely so nothing outside internal/auth can mint authority, and a
// scheduler that manufactures a principal is a scheduler whose reach nobody can
// audit by reading the role map.
//
// It emits `link.archived` like every other archive, so a webhook receiver
// reconciling state sees the same event whether a person or a rule moved the
// link. It does **not** touch `expires_at`, which the statement says and which
// the trigger vocabulary depends on.
//
// Returns whether the link was still active. An already-archived link is not an
// error — a rule whose window overlapped an interactive archive should not fail
// its whole firing over it — but it is not counted as work either.
func (s *Service) ArchiveByRule(
	ctx context.Context, workspaceID, linkID uuid.UUID,
) (bool, error) {
	row, err := s.q.ArchiveLinkByAutomation(ctx, dbgen.ArchiveLinkByAutomationParams{
		ID: linkID, WorkspaceID: workspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Deleted, or in another workspace. Neither is a failure worth
			// stopping a firing for.
			return false, nil
		}
		return false, fmt.Errorf("archive link %s by rule: %w", linkID, err)
	}

	if s.cache != nil {
		s.cache.InvalidateAlias(ctx, row.DomainID, row.Alias)
	}
	s.emitLink(ctx, domain.EventLinkArchived, &domain.Link{
		ID: row.ID, WorkspaceID: row.WorkspaceID, Alias: row.Alias,
		ShortURL: s.shortURL(ctx, row.DomainID, row.Alias),
		URL:      row.PrimaryUrl, Title: row.Title,
		Status: domain.LinkStatus(row.Status),
	})
	return true, nil
}

// --- plumbing ----------------------------------------------------------------

// encodeAutomationRule renders the two jsonb columns.
//
// Both stay jsonb rather than becoming columns, which is what m43.md asks for
// and what the rule every Phase 2 milestone inherits says: the part of a rule
// most likely to change shape is which subjects count, and that is the
// definition of what belongs in jsonb.
func encodeAutomationRule(
	config domain.AutomationTriggerConfig, actions []string,
) (configJSON, actionsJSON []byte, err error) {
	configJSON, err = json.Marshal(config)
	if err != nil {
		return nil, nil, fmt.Errorf("encode trigger config: %w", err)
	}
	actionsJSON, err = json.Marshal(actions)
	if err != nil {
		return nil, nil, fmt.Errorf("encode actions: %w", err)
	}
	return configJSON, actionsJSON, nil
}

// automationRuleFromRow decodes what the columns hold.
//
// A malformed column is an error rather than a zero value. The rows are written
// only by this file, so a rule whose actions do not parse is either corruption
// or somebody editing the table by hand, and both should be visible rather than
// silently evaluated as a rule that does nothing.
func automationRuleFromRow(r dbgen.AutomationRule) (domain.AutomationRule, error) {
	rule := domain.AutomationRule{
		ID: r.ID, WorkspaceID: r.WorkspaceID, Name: r.Name, Trigger: r.Trigger,
		Enabled: r.Enabled, LastFiredAt: r.LastFiredAt,
		Created: r.CreatedAt, Updated: r.UpdatedAt,
	}
	if len(r.TriggerConfig) > 0 {
		if err := json.Unmarshal(r.TriggerConfig, &rule.TriggerConfig); err != nil {
			return rule, fmt.Errorf("automation rule %s: decode trigger config: %w", r.ID, err)
		}
	}
	if len(r.Actions) > 0 {
		if err := json.Unmarshal(r.Actions, &rule.Actions); err != nil {
			return rule, fmt.Errorf("automation rule %s: decode actions: %w", r.ID, err)
		}
	}
	return rule, nil
}

// recordAutomationEvent writes the audit record for a rule change.
//
// The same trade every administrative write in this package makes: the change is
// what the actor asked for, and failing it because the record could not be
// written would swap a missing log line for an action that did not happen.
func (s *Service) recordAutomationEvent(
	ctx context.Context, actor *auth.Identity, action string,
	id uuid.UUID, metadata map[string]any,
) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Record(ctx, actor, audit.Event{
		Action: action, TargetType: "automation_rule", TargetID: &id, Metadata: metadata,
	}); err != nil {
		s.log.Warn("automation rule changed but the audit record was not written",
			slog.String("action", action), slog.Any("error", err))
	}
}
