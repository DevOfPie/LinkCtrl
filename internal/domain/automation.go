package domain

import (
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Automation rules (M43).
//
// A rule is a standing instruction: *when this happens in this workspace, do
// these things*. Nothing about it runs on the redirect path — evaluation is a
// job on the leader-elected scheduler, and this file exists so the vocabulary
// that job walks is one closed list rather than three that drift.
//
// **The vocabulary is deliberately tiny, and m43.md says why**: a large trigger
// set is untestable and becomes the place surprises live. Three triggers, three
// actions, one config key. Every combination of those is nine cases, which is a
// number a test can enumerate rather than sample.
//
// **The loop guard is declared here, not only implemented.** `TriggerReads` and
// `ActionWrites` say what each half touches, and
// `TestNoAutomationActionWritesATriggerSource` asserts the two sets never
// intersect. That is the structural half of "a rule cannot trigger itself or
// loop": no action an automation takes produces anything an automation trigger
// looks at, so an automation cannot feed itself however the rules are wired.
// The runtime half is the watermark — see AutomationRule.LastFiredAt.

// The trigger vocabulary. Every value `automation_rules.trigger` may hold.
//
// The names are the audit log's and the webhook vocabulary's where the three
// describe the same event, so an operator reading a rule beside a delivery beside
// an audit row is reading one vocabulary rather than three spellings of one.
const (
	// TriggerLinkExpired fires for links whose `expires_at` has passed.
	//
	// The link is not touched by the expiry itself: an expired link keeps its
	// row and its status, and the redirect path answers OutcomeNotFound from the
	// timestamp. So this is genuinely an observation of the clock rather than of
	// a write, and it is the one trigger whose subject set is knowable in advance.
	TriggerLinkExpired = "link.expired"
	// TriggerLinkMaxClicks fires for links whose durable click budget ran out
	// (M35). The subject is `link_click_budget.exhausted_at`, which the gate
	// stamps in the same transaction that spends the last click — not
	// `links.click_count`, which is the approximate counter the analytics
	// pipeline writes after the fact and which 02100 says out loud must never be
	// an authorization input.
	TriggerLinkMaxClicks = "link.max_clicks"
	// TriggerDestinationBlocked fires when somebody in the workspace was refused
	// a destination (M30). The subject is the audit record, because that is the
	// only durable trace a refusal leaves — `blocked_destinations` is the
	// operator's blocklist, not a log of attempts against it.
	TriggerDestinationBlocked = "destination.blocked"
)

// AutomationTriggers is the vocabulary, in the order a UI should list it.
//
// Ordered rather than a bare set, for the reason WebhookEvents is: the form's
// radio list and the API's advertised vocabulary agree without either sorting
// the other's output.
var AutomationTriggers = []string{
	TriggerLinkExpired,
	TriggerLinkMaxClicks,
	TriggerDestinationBlocked,
}

// The action vocabulary. Every value an entry in `automation_rules.actions` may
// name.
const (
	// ActionNotify writes one in-app notification per firing to the
	// organization's owners (M22). One per firing rather than one per subject:
	// a rule that matched forty expired links puts one item in an inbox saying
	// forty, because forty items in an inbox is how an inbox stops being read.
	ActionNotify = "notify"
	// ActionWebhook emits EventAutomationFired to the workspace's subscribed
	// webhooks (M42). It does not let the rule choose which event to emit, and
	// that restraint is the cascade guard doing its job — a rule that could emit
	// `destination.blocked` would be a rule that could manufacture the thing
	// another rule triggers on.
	ActionWebhook = "webhook"
	// ActionArchiveLink archives the matched links. Only legal on a trigger
	// whose subject is a link, and refused at write time otherwise rather than
	// silently doing nothing at evaluation time.
	//
	// **There is no `disable` action, by decision D10.** `archived` and
	// `disabled` produce the identical outcome on the redirect path — snapshot.go
	// maps both to OutcomeNotFound, deliberately, so a scanner cannot tell them
	// apart — and `disabled` has no restore affordance, so an automation writing
	// it would create links in a state the UI offers no way out of.
	ActionArchiveLink = "archive_link"
)

// AutomationActions is the vocabulary, in the order a UI should list it: the
// two that tell somebody, then the one that changes something.
var AutomationActions = []string{
	ActionNotify,
	ActionWebhook,
	ActionArchiveLink,
}

// AutomationSource names a thing in the database that a trigger reads or an
// action writes.
//
// The granularity is what makes the disjointness assertion below honest. Both
// `destination.blocked` and an archive touch `audit_log`, so a source named
// "audit_log" would either report a false intersection or force the test to be
// relaxed into meaninglessness. They are named at the granularity the queries
// actually filter at — one action of the log, not the log — and the queries are
// written to match.
type AutomationSource string

const (
	// SourceLinkExpiry is `links.expires_at`. Read by TriggerLinkExpired and
	// written by nothing here: an automation may archive a link, and archiving
	// must never move the expiry, or "link expired -> archive link" would re-arm
	// itself on every tick.
	SourceLinkExpiry AutomationSource = "links.expires_at"
	// SourceClickBudget is `link_click_budget.exhausted_at`, stamped by the gate.
	SourceClickBudget AutomationSource = "link_click_budget.exhausted_at"
	// SourceBlockedAudit is `audit_log` rows whose action is destination.blocked.
	SourceBlockedAudit AutomationSource = "audit_log(destination.blocked)"

	// SourceNotification is `notifications`. Nothing triggers on an inbox.
	SourceNotification AutomationSource = "notifications"
	// SourceWebhookQueue is `webhook_deliveries`. Nothing triggers on the queue,
	// which is the whole reason EventAutomationFired is not a trigger name.
	SourceWebhookQueue AutomationSource = "webhook_deliveries"
	// SourceLinkStatus is `links.status` and `links.archived_at`. Deliberately
	// *not* SourceLinkExpiry, and the split is the load-bearing part.
	SourceLinkStatus AutomationSource = "links.status"
	// SourceAutomationAudit is `audit_log` rows whose action is automation.fired.
	SourceAutomationAudit AutomationSource = "audit_log(automation.fired)"
)

// TriggerReads declares what each trigger looks at.
var TriggerReads = map[string][]AutomationSource{
	TriggerLinkExpired:        {SourceLinkExpiry},
	TriggerLinkMaxClicks:      {SourceClickBudget},
	TriggerDestinationBlocked: {SourceBlockedAudit},
}

// ActionWrites declares what each action produces.
//
// Every action writes SourceAutomationAudit as well as its own effect, because
// a firing is recorded whatever it did. Listed on each row rather than assumed,
// so the disjointness test sees it.
var ActionWrites = map[string][]AutomationSource{
	ActionNotify:      {SourceNotification, SourceAutomationAudit},
	ActionWebhook:     {SourceWebhookQueue, SourceAutomationAudit},
	ActionArchiveLink: {SourceLinkStatus, SourceAutomationAudit},
}

// LinkSubjectTriggers are the triggers whose subject is a link, and therefore
// the only ones ActionArchiveLink is legal on.
var LinkSubjectTriggers = map[string]bool{
	TriggerLinkExpired:   true,
	TriggerLinkMaxClicks: true,
}

var (
	automationTriggerSet = setOf(AutomationTriggers)
	automationActionSet  = setOf(AutomationActions)
)

func setOf(values []string) map[string]struct{} {
	m := make(map[string]struct{}, len(values))
	for _, v := range values {
		m[v] = struct{}{}
	}
	return m
}

// IsAutomationTrigger reports whether a name is in the trigger vocabulary.
func IsAutomationTrigger(name string) bool {
	_, ok := automationTriggerSet[name]
	return ok
}

// IsAutomationAction reports whether a name is in the action vocabulary.
func IsAutomationAction(name string) bool {
	_, ok := automationActionSet[name]
	return ok
}

// **The bounds on one evaluation run, in one place.**
//
// m43.md's Risks say trigger evaluation cost on the scheduler must be bounded,
// and a bound that is a product of four numbers spread over three packages is a
// bound nobody can state. These are all of them, and the evaluator takes its
// batch sizes from here rather than declaring its own.
//
// Worst case, one run costs:
//
//	AutomationRulesPerRun x (1 match query
//	                         + MaxAutomationActions actions
//	                         + AutomationMatchesPerRule archive statements)
//
// which at the values below is 100 x (1 + 3 + 25) = 2,900 statements, against a
// one-minute clock and a two-minute job timeout. The **expected** case is 100
// indexed range scans that return nothing, because the watermark means a rule
// only ever looks at what happened since it last fired.
const (
	// MaxAutomationRulesPerWorkspace bounds the list. Twenty, matching
	// MaxWebhooksPerWorkspace: a workspace wanting more standing instructions
	// than that wants a workflow engine.
	MaxAutomationRulesPerWorkspace = 20
	// AutomationRulesPerRun bounds how many enabled rules one run considers,
	// across every workspace on the instance. Rules are taken oldest-watermark
	// first, so a run that hits this cap starves nobody: the rules it skipped
	// have the oldest watermarks on the next run and go first.
	AutomationRulesPerRun = 100
	// AutomationMatchesPerRule bounds how many subjects one rule sees in one
	// run. A rule that matches more is truncated, logged, and its watermark
	// advances only to the last subject it actually handled — so the remainder
	// is picked up next run rather than skipped.
	AutomationMatchesPerRule = 25
	// MaxAutomationActions bounds one rule's action list. Three, because there
	// are three actions and repeating one is never useful — two `notify` entries
	// are two identical inbox items.
	MaxAutomationActions = 3
	// MaxAutomationRuleNameLength bounds the label, in runes.
	MaxAutomationRuleNameLength = 120
	// MaxAutomationMinCount bounds the one config key, and it is capped at the
	// per-run match cap rather than at a round number: a threshold larger than
	// one run can match is a rule that silently never fires, which is the
	// failure a closed vocabulary exists to prevent rather than to cause.
	// TestAThresholdIsAlwaysReachable holds the two together.
	MaxAutomationMinCount = AutomationMatchesPerRule
)

// AutomationInterval is how often the scheduler evaluates, and
// AutomationTimeout bounds one run.
//
// Here rather than in internal/automation, and the placement is load-bearing:
// the API and the page both advertise the interval, and a handler that had to
// import the evaluator to read a number would be a handler that imports the
// evaluator. TestNothingOnTheRequestPathImportsTheEvaluator asserts that none
// of them does, which is how "evaluation never runs on the request path" is
// enforced rather than promised.
//
// One minute, matching the rollup's clock rather than the outbox's thirty
// seconds: nothing here is something a person is sitting waiting for, and a link
// that expired is still expired a minute later. The timeout is twice the
// interval, so a slow run overlaps at most one tick and a stuck one is cut off
// rather than holding the scheduler.
const (
	AutomationInterval = time.Minute
	AutomationTimeout  = 2 * AutomationInterval
)

// AutomationRule is one standing instruction as the product understands it.
type AutomationRule struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Name        string    `json:"name"`
	Trigger     string    `json:"trigger"`
	// TriggerConfig is the threshold, and today it holds exactly one key. It
	// stays jsonb — 00600 put it there and M43 does not promote it to a column,
	// because the part of a rule most likely to grow is the part that decides
	// *which* subjects count, and that is the definition of what belongs in
	// jsonb under the rule every Phase 2 milestone inherits.
	TriggerConfig AutomationTriggerConfig `json:"trigger_config"`
	// Actions is the ordered list, run in order. Order is the caller's: a rule
	// that notifies and then archives reads better in an inbox than one that
	// archives and then notifies, and nothing here reorders it.
	Actions []string  `json:"actions"`
	Enabled bool      `json:"enabled"`
	Created time.Time `json:"created_at"`
	Updated time.Time `json:"updated_at"`

	// LastFiredAt is the **watermark**, and calling it a diagnostic would be a
	// misreading with consequences.
	//
	// A rule sees only subjects whose event time is strictly after this instant,
	// and the claim that fires a rule advances it in the same statement. That is
	// what stops a rule triggering itself: a link that expired at 09:00 is
	// matched once, the watermark moves past 09:00, and no later run can see it
	// again. Remove the advance and the rule fires on that same link on every
	// tick, forever — which is the runaway
	// TestAnAutomationDoesNotFireTwiceForOneSubject exists to catch.
	//
	// It is therefore set when a rule is created and when a disabled rule is
	// switched back on, not left NULL until the first firing. A NULL watermark
	// on a rule created today would mean "every link that ever expired", and a
	// rule re-enabled after a month would otherwise fire for the whole backlog
	// the moment somebody flipped the switch.
	LastFiredAt *time.Time `json:"last_fired_at"`
}

// AutomationTriggerConfig is the whole of `trigger_config` today.
//
// One key. It is a struct rather than a map so the API shape is discoverable and
// an unknown key is refused rather than stored and ignored, and it is marshalled
// into the existing jsonb column rather than given columns of its own.
type AutomationTriggerConfig struct {
	// MinCount is how many subjects must have accumulated before the rule fires
	// at all. Zero and one both mean "fire on the first one"; the zero value is
	// legal so an omitted config is a valid config.
	//
	// The subjects do not go away while the threshold is unmet: the watermark
	// does not advance on a run that did not fire, so they accumulate and the
	// rule fires when the count is reached. A threshold that discarded what it
	// counted would be a threshold nobody could reason about.
	MinCount int `json:"min_count"`
}

// Threshold is MinCount with its floor applied.
func (c AutomationTriggerConfig) Threshold() int {
	if c.MinCount < 1 {
		return 1
	}
	return c.MinCount
}

// ValidateAutomationTrigger checks a trigger name against the vocabulary.
func ValidateAutomationTrigger(name string) ValidationErrors {
	if !IsAutomationTrigger(name) {
		return ValidationErrors{{
			Field: "trigger", Code: "unknown_trigger",
			Message: fmt.Sprintf("%q is not something this product can trigger on; "+
				"the whole vocabulary is %v", name, AutomationTriggers),
		}}
	}
	return nil
}

// ValidateAutomationActions checks an action list against the vocabulary and
// against the trigger it is attached to.
//
// Deduplicated and sorted into the canonical vocabulary order, so two rules that
// mean the same thing compare equal and the stored array is stable. An unknown
// name is refused rather than dropped, for the reason ValidateWebhookEvents
// gives: silently ignoring one leaves somebody with a rule they believe does
// something and a scheduler that does nothing.
func ValidateAutomationActions(trigger string, actions []string) ([]string, ValidationErrors) {
	var errs ValidationErrors
	if len(actions) == 0 {
		return nil, append(errs, FieldError{
			Field: "actions", Code: "required",
			Message: "choose at least one action; a rule that does nothing when it " +
				"fires is a row nobody can tell from a broken one",
		})
	}
	if len(actions) > MaxAutomationActions {
		return nil, append(errs, FieldError{
			Field: "actions", Code: "too_many",
			Message: fmt.Sprintf("a rule may have at most %d actions", MaxAutomationActions),
		})
	}

	seen := make(map[string]struct{}, len(actions))
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		if !IsAutomationAction(a) {
			errs = append(errs, FieldError{
				Field: "actions", Code: "unknown_action",
				Message: fmt.Sprintf("%q is not an action this product can take; the "+
					"whole vocabulary is %v", a, AutomationActions),
			})
			continue
		}
		if a == ActionArchiveLink && !LinkSubjectTriggers[trigger] {
			// Refused at write time rather than skipped at evaluation time. A
			// rule saved with an action that can never run is a rule whose page
			// says it archives links and whose scheduler never does.
			errs = append(errs, FieldError{
				Field: "actions", Code: "no_link_subject",
				Message: fmt.Sprintf("%q has no link to archive; %q is only available on "+
					"%v", trigger, ActionArchiveLink, keysOf(LinkSubjectTriggers)),
			})
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	sort.Slice(out, func(i, j int) bool {
		return automationActionOrder(out[i]) < automationActionOrder(out[j])
	})
	return out, nil
}

func automationActionOrder(action string) int {
	for i, a := range AutomationActions {
		if a == action {
			return i
		}
	}
	return len(AutomationActions)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ValidateAutomationRuleName bounds the label.
func ValidateAutomationRuleName(s string) ValidationErrors {
	if s == "" {
		return ValidationErrors{{
			Field: "name", Code: "required",
			Message: "give the rule a name; it is what the list and the audit record show",
		}}
	}
	if utf8.RuneCountInString(s) > MaxAutomationRuleNameLength {
		return ValidationErrors{{
			Field: "name", Code: "too_long",
			Message: fmt.Sprintf("name must be at most %d characters",
				MaxAutomationRuleNameLength),
		}}
	}
	return nil
}

// ValidateAutomationTriggerConfig bounds the one key.
func ValidateAutomationTriggerConfig(c AutomationTriggerConfig) ValidationErrors {
	if c.MinCount < 0 {
		return ValidationErrors{{
			Field: "trigger_config.min_count", Code: "negative",
			Message: "min_count counts subjects, so it cannot be negative",
		}}
	}
	if c.MinCount > MaxAutomationMinCount {
		return ValidationErrors{{
			Field: "trigger_config.min_count", Code: "too_large",
			Message: fmt.Sprintf("min_count must be at most %d; a threshold larger than "+
				"one run can match is a rule that never fires", MaxAutomationMinCount),
		}}
	}
	return nil
}
