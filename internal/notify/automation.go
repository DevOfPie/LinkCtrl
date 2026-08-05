package notify

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Automation firings (M43).
//
// **One notification per firing, never one per subject.** A rule that matched
// forty expired links puts one item in an inbox saying forty, with the first few
// named. Forty items is how an inbox stops being read, and the whole reason M22
// exists is that somebody reads it.
//
// There is no NotifiedSince guard here, and its absence is deliberate. The
// audit-growth warning needs one because its condition stays true between runs
// and would re-raise hourly; an automation firing is an *edge* — the watermark
// means each subject is seen exactly once — so a second notification means a
// second set of subjects, and suppressing it would suppress news.

// KindAutomationFired is one rule firing. The rule is named in the data, so an
// inbox filtered to this kind reads as a list of what the scheduler did.
const KindAutomationFired = "automation.fired"

// AutomationFired tells a workspace's owners that one of its rules ran.
//
// Addressed to the owners this workspace has, which is who OwnersOf answers
// with: the organization-wide ones, plus anybody holding owner scoped to this
// workspace. A rule is a workspace object and its firing names the links it
// matched, so an owner scoped to a *different* workspace is not a recipient —
// they hold no membership through which those links are theirs to read.
// The notification carries the owning workspace so it appears in that
// workspace's inbox rather than wherever the reader happens to be standing.
//
// True since 0.2.0 and not before (F105, D102): the column was written from M40
// onward and read by nothing — no query selected it, the domain type had no
// field for it — so this sentence described an inbox scope that did not exist,
// in the one place a reader would look before adding the filter themselves.
//
// `subjects` is the human list, already bounded by the caller. `matched` is the
// real count, which can be larger when a run was truncated at its per-rule cap
// — and printing the count separately from the list is what stops a truncated
// firing reading like a complete one.
func (s *Service) AutomationFired(
	ctx context.Context, orgID uuid.UUID, workspaceID uuid.UUID,
	ruleID uuid.UUID, ruleName, trigger string, matched int, subjects []string,
) error {
	ws := workspaceID
	owners, err := s.OwnersOf(ctx, orgID, &ws)
	if err != nil {
		return fmt.Errorf("notify: owners of %s: %w", orgID, err)
	}

	body := fmt.Sprintf("%q matched %s in this workspace.",
		ruleName, plural(matched, "subject", "subjects"))
	if len(subjects) > 0 {
		body += " " + summarize(subjects, matched)
	}

	var errs []error
	for _, owner := range owners {
		if err := s.Notify(ctx, owner.UserID, Event{
			Kind:  KindAutomationFired,
			Title: "Automation rule fired: " + ruleName,
			Body:  body,
			Data: map[string]any{
				"rule_id":  ruleID,
				"rule":     ruleName,
				"trigger":  trigger,
				"matched":  matched,
				"subjects": subjects,
			},
			WorkspaceID: &ws,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// summarize renders the named subjects and says so when there are more.
func summarize(subjects []string, matched int) string {
	out := "The first are: "
	if len(subjects) == matched {
		out = "They are: "
	}
	for i, s := range subjects {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out + "."
}
