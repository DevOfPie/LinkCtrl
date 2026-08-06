// Package automation evaluates a workspace's standing instructions and carries
// them out.
//
// **Nothing here runs on the request path, and nothing can put it there.** The
// only entry point is Evaluate, the only caller is the scheduler, and the
// scheduler calls it under the Postgres advisory lock every other job takes. A
// redirect never touches this package; neither does a link write. That is the
// first of the two claims m43.md turns on, and the import graph is what enforces
// it — internal/link and internal/redirect do not import this package, and this
// package's dependencies are all interfaces it declares itself.
//
// **The per-run cost is a product of four constants, all of them in
// internal/domain.** A run reads at most domain.AutomationRulesPerRun rules; each
// rule runs one indexed range query bounded at domain.AutomationMatchesPerRule;
// each rule runs at most domain.MaxAutomationActions actions, of which only the
// archive is per-subject; and one further statement closes the run by advancing
// the cursor over every rule it looked at. The arithmetic is written out beside
// those constants rather than left for a reader to assemble, because m43.md's
// Risks say the bound is the thing that has to be true and a bound nobody can
// state is not one.
//
// **Every rule comes round, and the column that makes that true is not the
// watermark.** The cap means a run sees a hundred rules and an instance may hold
// more, so which hundred is a fairness question rather than a detail.
// `last_checked_at` answers it: the due query orders on it, and Evaluate advances
// it for every rule the pass reached — fired, matched nothing, or failed. The
// watermark cannot do that job, because it moves only on a firing and idle is
// exactly what keeps it old: ordering on it left the hundred oldest a fixed set,
// with rule 101 never evaluated on any run, which is F83 and what migration
// 03100 separates.
//
// **A rule cannot trigger itself, and the mechanism is the watermark.** Every
// match query orders by (event time, id) and reads the window that opens
// strictly after the pair `(last_fired_at, last_fired_subject_id)` and closes at
// `now`; the claim that fires a rule advances both halves past the last subject
// it handled *before* any action runs. A pair rather than an instant, because a
// capped fetch can stop between two subjects sharing one timestamp and only a
// position in the full match order can say which of them were handled — see
// resumeCursor. A subject is therefore visible to a rule exactly once, whatever
// the actions did and however many times the scheduler ticks.
//
// **A rule cannot loop through another rule either**, and that is a separate
// mechanism: no action writes anything any trigger reads.
// domain.TriggerReads and domain.ActionWrites declare both halves and
// TestNoAutomationActionWritesATriggerSource asserts they never intersect. It is
// why the webhook action emits only `automation.fired` — an event nothing
// triggers on — rather than letting a rule choose. m43.md names the failure as
// *an automation that fires a webhook that fires an automation*; the answer is
// that the vocabulary makes the second half unreachable, and that the assertion
// fails the build if anybody widens it.
package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// SubjectsShown is how many subjects a notification and a webhook payload name.
//
// Five. A firing that matched twenty-five links should produce one inbox item
// somebody reads, not a wall of aliases; the count travels beside the list so a
// truncated list never reads as a complete one.
const SubjectsShown = 5

// checkpointTimeout bounds the one statement that ends a run.
//
// Its own bound because it is written on a context that has deliberately
// outlived the run's — see markChecked — and an unbounded write on a context
// nothing can cancel is a job that never returns. Five seconds is generous for a
// single indexed update of at most AutomationRulesPerRun rows and is a fifth of
// the interval, so a run that spent its whole budget still ends inside the next
// tick.
const checkpointTimeout = 5 * time.Second

// Archiver is internal/link's archive, as this package needs it.
//
// Declared here rather than imported so the graph stays one-way: internal/link
// never imports this package, exactly as it never imports internal/webhook. The
// method takes no actor because there is none — see internal/link/automation.go
// for why a synthetic identity would have been the worse answer.
type Archiver interface {
	ArchiveByRule(ctx context.Context, workspaceID, linkID uuid.UUID) (bool, error)
}

// Notifier is internal/notify's automation half. Nil notifies nobody, which is
// what a process built without a notifier gets.
type Notifier interface {
	AutomationFired(ctx context.Context, orgID, workspaceID, ruleID uuid.UUID,
		ruleName, trigger string, matched int, subjects []string) error
}

// Emitter is internal/webhook's writing half, as internal/link already declares
// it. Nil emits nothing.
type Emitter interface {
	Emit(ctx context.Context, workspaceID uuid.UUID, event string, data map[string]any)
}

// Observer counts firings. Nil counts nothing.
//
// The label vocabulary is bounded by construction: three triggers and two
// outcomes, so the whole metric is six series however many rules exist. M13's
// cardinality rule is what makes a rule name unacceptable as a label.
type Observer interface {
	ObserveAutomationFiring(trigger, outcome string)
}

// Service is the evaluator.
type Service struct {
	q        *dbgen.Queries
	links    Archiver
	notifier Notifier
	events   Emitter
	audit    audit.Recorder
	log      *slog.Logger
	obs      Observer
	// now is the clock, injectable so a test can place a subject on either side
	// of a watermark without sleeping.
	now func() time.Time
}

// Config is what a Service needs. Its own struct rather than config.Config,
// matching every other service in this tree: the package that does the work does
// not read the environment.
type Config struct {
	Links    Archiver
	Notifier Notifier
	Events   Emitter
	Audit    audit.Recorder
	Logger   *slog.Logger
	Observer Observer
	// Now overrides the clock. Nil takes time.Now.
	Now func() time.Time
}

func NewService(pool *pgxpool.Pool, cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		q: dbgen.New(pool), links: cfg.Links, notifier: cfg.Notifier,
		events: cfg.Events, audit: cfg.Audit, log: log, obs: cfg.Observer, now: now,
	}
}

// Evaluate runs one pass over the due rules, and is what the scheduler calls.
//
// One rule's failure never stops the pass: errors are collected and the
// remaining rules are still evaluated, because one workspace's broken rule must
// not stop everybody else's.
func (s *Service) Evaluate(ctx context.Context) error {
	now := s.now().UTC()

	rules, err := s.q.ListDueAutomationRules(ctx, domain.AutomationRulesPerRun)
	if err != nil {
		return fmt.Errorf("automation: list due rules: %w", err)
	}
	if len(rules) == domain.AutomationRulesPerRun {
		// The first bound biting, said out loud. It is not an error — the rules
		// this run skipped keep the older cursor and go first next time, which
		// the advance below is what earns — but an instance permanently at the
		// cap is one where every rule is evaluated less often than the clock
		// suggests, and that is worth being able to see rather than infer.
		s.log.Info("automation evaluation hit its per-run rule cap",
			slog.Int("rules", len(rules)),
			slog.Int("cap", domain.AutomationRulesPerRun))
	}

	var errs []error
	// The rules this pass actually reached. Only these have their cursor moved:
	// a run cut off part-way must leave the rules it never looked at at the head
	// of the queue, or a run that times out at the same rule every time becomes
	// the starvation this column exists to end.
	looked := make([]uuid.UUID, 0, len(rules))
	for _, rule := range rules {
		if ctx.Err() != nil {
			break
		}
		// Recorded before the evaluation rather than after it, because a rule
		// that errors has still been looked at. One that fails on every pass —
		// a hand-edited row, a trigger outside the vocabulary — would otherwise
		// hold its place at the head of the queue forever, which is the defect
		// this column was added for wearing a different hat.
		looked = append(looked, rule.ID)
		if err := s.evaluateRule(ctx, rule, now); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.markChecked(ctx, looked, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// markChecked advances the scheduler's cursor over the rules a run looked at.
//
// **On a context that survives this run's cancellation**, bounded on its own.
// The job's deadline is what cuts a slow pass short, and a pass that was cut
// short has still looked at the rules it reached; losing that fact is how the
// fixed front of the queue would quietly reassemble itself, since every
// following run would start at the same rule and be cut off at the same one.
// The write is one indexed update of ids already read, so there is nothing here
// that a cancelled run should be protected from finishing.
func (s *Service) markChecked(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkpointTimeout)
	defer cancel()
	if err := s.q.MarkAutomationRulesChecked(ctx, dbgen.MarkAutomationRulesCheckedParams{
		CheckedAt: &at, Ids: ids,
	}); err != nil {
		return fmt.Errorf("automation: mark %d rules checked: %w", len(ids), err)
	}
	return nil
}

// subject is one thing a trigger matched.
type subject struct {
	// ID is the subject's identity in its own source table — the link for the
	// two link triggers, the audit row for a blocked attempt — and it is the
	// tiebreak half of the watermark. Every match query orders by (event time,
	// id), so "the last subject handled" is only a resumable position when both
	// columns travel with it; the timestamp alone cannot point inside a group of
	// subjects sharing one instant.
	ID uuid.UUID
	// LinkID is set for the two link-subject triggers and nil for a blocked
	// attempt, which is what makes `archive_link` illegal on that trigger at
	// write time.
	LinkID *uuid.UUID
	// Label is what a person reads: an alias, or the defanged host somebody was
	// refused.
	Label string
	// At is the event's own timestamp, and the watermark is advanced to the last
	// one handled. Not `now`: advancing past subjects that were not handled is
	// how a truncated run would silently drop work.
	At time.Time
}

// resumeCursor is where this rule's next match window opens: strictly after the
// (event time, subject id) pair of the last subject a firing handled.
//
// A keyset cursor over the match queries' ORDER BY, and both halves are
// load-bearing. The instant alone cannot express "stopped part-way through a
// group of subjects sharing one timestamp" — routine for link.expired, because
// bulk-created links share one expires_at — and a capped run that stopped
// inside such a group used to reopen the next window strictly after the shared
// instant, dropping the tied remainder from every future window rather than
// deferring it. Strict `>=` on the instant is not an answer either: it re-fires
// the boundary subject on every run, which is the runaway the watermark exists
// to prevent.
//
// The timestamp falls back to `created_at` and not `now`, because a rule whose
// watermark is NULL was written outside this product — every path in
// internal/link sets it — and firing for the whole history is the wrong
// direction to guess in.
//
// A missing subject id — every row from before 03600, every freshly armed rule,
// every re-arm — falls back to uuid.Max: no uuid sorts after it, so the tiebreak
// admits nothing at the boundary instant and the window degenerates to exactly
// the strict `>` those rows were written under. uuid.Nil would be the opposite
// guess, and the wrong one — it would re-admit every subject tied on the
// instant the last firing already handled.
func resumeCursor(rule dbgen.ListDueAutomationRulesRow) (time.Time, uuid.UUID) {
	after := rule.CreatedAt
	if rule.LastFiredAt != nil {
		after = *rule.LastFiredAt
	}
	afterSubject := uuid.Max
	if rule.LastFiredSubjectID != nil {
		afterSubject = *rule.LastFiredSubjectID
	}
	return after, afterSubject
}

// evaluateRule matches, claims and acts, in that order.
func (s *Service) evaluateRule(ctx context.Context, rule dbgen.ListDueAutomationRulesRow, now time.Time) error {
	actions, config, err := decodeRule(rule)
	if err != nil {
		// Corruption, or somebody editing the table by hand. Visible rather than
		// quietly evaluated as a rule that does nothing.
		return err
	}

	// The window's lower bound: the composite watermark, with resumeCursor
	// holding both fallbacks and the reasoning for their directions.
	after, afterSubject := resumeCursor(rule)

	subjects, err := s.match(ctx, rule, after, afterSubject, now)
	if err != nil {
		return err
	}
	if len(subjects) >= domain.AutomationMatchesPerRule {
		// The second bound biting. Also not an error: the watermark advances only
		// to the last subject handled, so the remainder is picked up next run.
		s.log.Info("automation rule hit its per-run match cap",
			slog.String("rule", rule.Name),
			slog.String("trigger", rule.Trigger),
			slog.Int("cap", domain.AutomationMatchesPerRule))
	}
	if len(subjects) < config.Threshold() {
		// Below the threshold. The watermark deliberately does not move, so what
		// matched accumulates and the rule fires when the count is reached. A
		// threshold that discarded what it counted would be one nobody could
		// reason about.
		return nil
	}

	// **The claim.** Advance the watermark to the last subject handled — both
	// the instant and the subject id, because together they are the position a
	// capped run resumes from — and only if both halves are still where the
	// match query saw them. Everything after this point runs exactly once for
	// these subjects.
	last := subjects[len(subjects)-1]
	claimed, err := s.q.ClaimAutomationRule(ctx, dbgen.ClaimAutomationRuleParams{
		ID:        rule.ID,
		Watermark: &last.At, WatermarkSubject: &last.ID,
		Expected: rule.LastFiredAt, ExpectedSubject: rule.LastFiredSubjectID,
	})
	if err != nil {
		return fmt.Errorf("automation: claim rule %s: %w", rule.ID, err)
	}
	if claimed == 0 {
		// Somebody else fired it, or it was disabled or edited between the two
		// statements. Not an error: the whole point of the compare-and-set is
		// that losing costs nothing.
		return nil
	}

	return s.act(ctx, rule, actions, subjects)
}

// match runs the one query this rule's trigger names.
//
// `afterSubject` is the tiebreak half of the window's lower bound and it feeds
// every arm, because every match query orders by (event time, id) and resumes
// by the same pair. Each arm also records the row's id on the subject it builds
// — that id is what the claim will persist as the next boundary.
func (s *Service) match(
	ctx context.Context, rule dbgen.ListDueAutomationRulesRow,
	after time.Time, afterSubject uuid.UUID, until time.Time,
) ([]subject, error) {
	switch rule.Trigger {
	case domain.TriggerLinkExpired:
		rows, err := s.q.MatchExpiredLinks(ctx, dbgen.MatchExpiredLinksParams{
			WorkspaceID: rule.WorkspaceID, After: &after, AfterSubject: afterSubject,
			Until:    &until,
			RowLimit: domain.AutomationMatchesPerRule,
		})
		if err != nil {
			return nil, fmt.Errorf("automation: match expired links for %s: %w", rule.ID, err)
		}
		out := make([]subject, 0, len(rows))
		for _, r := range rows {
			id := r.ID
			out = append(out, subject{ID: r.ID, LinkID: &id, Label: r.Alias, At: *r.ExpiresAt})
		}
		return out, nil

	case domain.TriggerLinkMaxClicks:
		rows, err := s.q.MatchExhaustedBudgets(ctx, dbgen.MatchExhaustedBudgetsParams{
			WorkspaceID: rule.WorkspaceID, After: &after, AfterSubject: afterSubject,
			Until:    &until,
			RowLimit: domain.AutomationMatchesPerRule,
		})
		if err != nil {
			return nil, fmt.Errorf("automation: match exhausted budgets for %s: %w", rule.ID, err)
		}
		out := make([]subject, 0, len(rows))
		for _, r := range rows {
			id := r.LinkID
			out = append(out, subject{ID: r.LinkID, LinkID: &id, Label: r.Alias, At: *r.ExhaustedAt})
		}
		return out, nil

	case domain.TriggerDestinationBlocked:
		rows, err := s.q.MatchWorkspaceAuditEvents(ctx, dbgen.MatchWorkspaceAuditEventsParams{
			WorkspaceID: &rule.WorkspaceID,
			// The one caller, passing the one action. The trigger name, the audit
			// action and the webhook event are the same string, which
			// TestTheBlockedVocabularyIsOneWord holds together.
			Action: domain.TriggerDestinationBlocked,
			After:  after, AfterSubject: afterSubject, Until: until,
			RowLimit: domain.AutomationMatchesPerRule,
		})
		if err != nil {
			return nil, fmt.Errorf("automation: match blocked attempts for %s: %w", rule.ID, err)
		}
		out := make([]subject, 0, len(rows))
		for _, r := range rows {
			// The subject id is the audit row's own id: a blocked attempt has no
			// link, but it still needs a place in the (occurred_at, id) order for
			// the watermark to stop at.
			out = append(out, subject{ID: r.ID, Label: blockedLabel(r.Metadata), At: r.OccurredAt})
		}
		return out, nil
	}

	// An unknown trigger is a row written outside this product: every write path
	// validates against the closed vocabulary. Reported rather than skipped, so a
	// rule nobody can see firing is visible in the log.
	return nil, fmt.Errorf("automation: rule %s has trigger %q, which is not in %v",
		rule.ID, rule.Trigger, domain.AutomationTriggers)
}

// act runs the rule's actions in order and records the firing.
//
// One action's failure does not stop the others. The subjects are already
// claimed — the watermark moved before this ran — so abandoning the rest would
// mean the actions that did not run never will, and a rule that half-fired
// silently is worse than one that reports what it could not do.
func (s *Service) act(
	ctx context.Context, rule dbgen.ListDueAutomationRulesRow,
	actions []string, subjects []subject,
) error {
	labels := make([]string, 0, len(subjects))
	for _, sub := range subjects {
		labels = append(labels, sub.Label)
	}
	shown := labels
	if len(shown) > SubjectsShown {
		shown = shown[:SubjectsShown]
	}

	var errs []error
	for _, action := range actions {
		switch action {
		case domain.ActionNotify:
			if s.notifier == nil {
				continue
			}
			if err := s.notifier.AutomationFired(ctx, rule.OrganizationID, rule.WorkspaceID,
				rule.ID, rule.Name, rule.Trigger, len(subjects), shown); err != nil {
				errs = append(errs, fmt.Errorf("automation: notify for %s: %w", rule.ID, err))
			}

		case domain.ActionWebhook:
			if s.events == nil {
				continue
			}
			// Always EventAutomationFired, never a name the rule chose. That
			// restraint is the cascade guard: nothing triggers on this event, so
			// a rule cannot manufacture the thing another rule watches for.
			s.events.Emit(ctx, rule.WorkspaceID, domain.EventAutomationFired, map[string]any{
				"rule_id":  rule.ID,
				"rule":     rule.Name,
				"trigger":  rule.Trigger,
				"matched":  len(subjects),
				"subjects": shown,
			})

		case domain.ActionArchiveLink:
			if s.links == nil {
				continue
			}
			for _, sub := range subjects {
				if sub.LinkID == nil {
					// Unreachable: `archive_link` is refused at write time on a
					// trigger with no link subject. Skipped rather than
					// dereferenced, because "unreachable" and "cannot panic"
					// should not be the same sentence.
					continue
				}
				if _, err := s.links.ArchiveByRule(ctx, rule.WorkspaceID, *sub.LinkID); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}

	s.observe(rule.Trigger, errs)
	s.record(ctx, rule, actions, len(subjects), shown)
	return errors.Join(errs...)
}

// record writes the audit trail for one firing.
//
// The actor is a rule, not a person, and the label says so. Every other action
// in that log was taken by somebody signed in or holding a key; without this
// record an automated archive would be a link whose status changed with nothing
// beside it, which is indistinguishable from a bug.
func (s *Service) record(
	ctx context.Context, rule dbgen.ListDueAutomationRulesRow,
	actions []string, matched int, shown []string,
) {
	if s.audit == nil {
		return
	}
	id := rule.ID
	actor := &auth.Identity{
		Name:        "automation:" + rule.Name,
		OrgID:       rule.OrganizationID,
		WorkspaceID: rule.WorkspaceID,
	}
	if err := s.audit.Record(ctx, actor, audit.Event{
		Action: audit.ActionAutomationFired, TargetType: "automation_rule", TargetID: &id,
		Metadata: map[string]any{
			"rule":     rule.Name,
			"trigger":  rule.Trigger,
			"actions":  actions,
			"matched":  matched,
			"subjects": shown,
		},
	}); err != nil {
		s.log.Warn("automation rule fired but the audit record was not written",
			slog.String("rule", rule.Name), slog.Any("error", err))
	}
}

func (s *Service) observe(trigger string, errs []error) {
	if s.obs == nil {
		return
	}
	outcome := "fired"
	if len(errs) > 0 {
		outcome = "partial"
	}
	s.obs.ObserveAutomationFiring(trigger, outcome)
}

// decodeRule reads the two jsonb columns.
func decodeRule(rule dbgen.ListDueAutomationRulesRow) ([]string, domain.AutomationTriggerConfig, error) {
	var actions []string
	var config domain.AutomationTriggerConfig
	if len(rule.Actions) > 0 {
		if err := json.Unmarshal(rule.Actions, &actions); err != nil {
			return nil, config, fmt.Errorf("automation: rule %s: decode actions: %w", rule.ID, err)
		}
	}
	if len(rule.TriggerConfig) > 0 {
		if err := json.Unmarshal(rule.TriggerConfig, &config); err != nil {
			return nil, config, fmt.Errorf("automation: rule %s: decode trigger config: %w", rule.ID, err)
		}
	}
	return actions, config, nil
}

// blockedLabel pulls a readable subject out of a `destination.blocked` record.
//
// The audit metadata already stores the attempted URL **defanged**, and that is
// what travels into an inbox and a webhook payload — a receiver piping this into
// a chat room must not be handed a live link to the thing that was refused. The
// tier is appended when it is there, because "who said no" is what somebody
// reading the notification actually wants.
func blockedLabel(metadata []byte) string {
	var m struct {
		Defanged string `json:"url_defanged"`
		Tier     string `json:"tier"`
	}
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &m)
	}
	label := m.Defanged
	if label == "" {
		label = "a destination"
	}
	if m.Tier != "" {
		label += " (" + m.Tier + ")"
	}
	return label
}
