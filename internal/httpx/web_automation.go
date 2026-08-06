package httpx

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// The automation page (M43).
//
// Same shape as the webhooks page — forms that each submit on their own, htmx as
// the enhancement rather than the mechanism — so everything here works with
// scripting off.
//
// **The page shows when each rule last fired, and calls it that.** A standing
// instruction that runs on a clock nobody is watching has exactly one question
// people ask of it, and it is "did it do anything". The column is the watermark:
// it is what the evaluator reads to decide which subjects a rule has already
// seen, so a rule that has never fired and a rule armed a moment ago show the
// same value — which is the truth, and the page says so rather than pretending
// the two are different.
//
// **There is no run-now control.** Evaluation happens on the scheduler and
// nowhere else, and a button that ran it here would be the request path doing
// the work m43.md says it must not.

// automationRuleRow is one rendered rule.
type automationRuleRow struct {
	ID      string
	Name    string
	Trigger string
	Actions []string
	Enabled bool
	// MinCount is the threshold with its floor applied, so the page never shows
	// a bare zero for something that behaves like a one.
	MinCount  int
	LastFired string
	CreatedAt string

	// TriggerChoices and ActionChoices are the whole vocabularies with this
	// row's values selected, for the inline editor. Computed here rather than
	// with a template helper asking "is this name in that slice": a template
	// that can do set membership is a template that will grow logic.
	TriggerChoices []automationChoice
	ActionChoices  []automationChoice
	// Editing marks the row whose form is open.
	Editing bool
}

// automationChoice is one option on a form.
type automationChoice struct {
	Name    string
	Checked bool
	// LinkOnly marks an action that is refused on a trigger with no link
	// subject, so the form can say why rather than only failing on submit.
	LinkOnly bool
}

type automationPageData struct {
	shell
	Rows  []automationRuleRow
	Count int

	// The vocabularies, for the create form.
	Triggers []automationChoice
	Actions  []automationChoice

	// The bounds, rendered on the page. These are what answers "why has my rule
	// not fired yet" without anybody reading the source.
	MaxRules       int
	MaxActions     int
	RulesPerRun    int
	MatchesPerRule int
	IntervalMins   int

	// The create form's sticky values, so a refusal does not empty the boxes.
	FormName     string
	FormMinCount string

	EditingID string

	CanRead  bool
	CanWrite bool

	Notice string
	Error  string
}

func (h *Web) loadAutomationPage(w http.ResponseWriter, r *http.Request) (automationPageData, bool) {
	actor := IdentityFrom(r.Context())

	data := automationPageData{
		shell:          h.shell(r, "Automation", "automation"),
		MaxRules:       domain.MaxAutomationRulesPerWorkspace,
		MaxActions:     domain.MaxAutomationActions,
		RulesPerRun:    domain.AutomationRulesPerRun,
		MatchesPerRule: domain.AutomationMatchesPerRule,
		IntervalMins:   int(domain.AutomationInterval.Minutes()),
		FormName:       r.URL.Query().Get("name"),
		FormMinCount:   r.URL.Query().Get("min_count"),
		EditingID:      strings.TrimSpace(r.URL.Query().Get("edit")),
		CanRead:        actor.Can(link.PermAutomationRead),
		CanWrite:       actor.Can(link.PermAutomationWrite),
	}
	data.Notice = automationNotice(r.URL.Query().Get("rule"))

	// Selected on the create form when the query string carries them, so a
	// refusal keeps what the person chose.
	data.Triggers = automationTriggerChoices(r.URL.Query().Get("trigger"))
	chosen := map[string]bool{}
	for _, a := range r.URL.Query()["actions"] {
		chosen[a] = true
	}
	data.Actions = automationActionChoices(chosen)

	if !data.CanRead {
		// Rendered rather than refused, so somebody without the permission gets
		// the page explaining what it is instead of a 403 they cannot act on.
		return data, true
	}

	rules, err := h.Links.AutomationRules(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	data.Count = len(rules)
	for _, rule := range rules {
		data.Rows = append(data.Rows, automationRow(rule, data.EditingID))
	}
	return data, true
}

func automationRow(rule domain.AutomationRule, editingID string) automationRuleRow {
	row := automationRuleRow{
		ID: rule.ID.String(), Name: rule.Name, Trigger: rule.Trigger,
		Actions: rule.Actions, Enabled: rule.Enabled,
		MinCount:       rule.TriggerConfig.Threshold(),
		CreatedAt:      rule.Created.UTC().Format("2006-01-02"),
		Editing:        rule.ID.String() == editingID,
		TriggerChoices: automationTriggerChoices(rule.Trigger),
	}
	// "Never" is not a state this can be in: every write path arms the rule, so
	// the column always carries a time. It says "armed" rather than "fired" for
	// a rule that has not matched anything yet, which the page's own copy
	// explains — the value is a watermark, and the two are the same field.
	if rule.LastFiredAt != nil {
		row.LastFired = rule.LastFiredAt.UTC().Format("2006-01-02 15:04")
	} else {
		row.LastFired = "unarmed"
	}
	chosen := map[string]bool{}
	for _, a := range rule.Actions {
		chosen[a] = true
	}
	row.ActionChoices = automationActionChoices(chosen)
	return row
}

func automationTriggerChoices(selected string) []automationChoice {
	out := make([]automationChoice, 0, len(domain.AutomationTriggers))
	for _, t := range domain.AutomationTriggers {
		out = append(out, automationChoice{Name: t, Checked: t == selected})
	}
	return out
}

func automationActionChoices(chosen map[string]bool) []automationChoice {
	out := make([]automationChoice, 0, len(domain.AutomationActions))
	for _, a := range domain.AutomationActions {
		out = append(out, automationChoice{
			Name: a, Checked: chosen[a], LinkOnly: a == domain.ActionArchiveLink,
		})
	}
	return out
}

func (h *Web) AutomationPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadAutomationPage(w, r)
	if !ok {
		return
	}
	h.renderAutomation(w, r, http.StatusOK, data)
}

func (h *Web) renderAutomation(
	w http.ResponseWriter, r *http.Request, status int, data automationPageData,
) {
	if isHTMX(r) {
		h.renderPartial(w, r, "automation", "automation_panel", data)
		return
	}
	h.render(w, r, status, "automation", data)
}

func (h *Web) AutomationCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	_, err := h.Links.CreateAutomationRule(r.Context(), IdentityFrom(r.Context()),
		link.CreateAutomationRuleInput{
			Name:          r.PostFormValue("name"),
			Trigger:       r.PostFormValue("trigger"),
			TriggerConfig: domain.AutomationTriggerConfig{MinCount: formMinCount(r)},
			Actions:       r.PostForm["actions"],
			Enabled:       true,
		})
	h.finishAutomationAction(w, r, "created", err)
}

func (h *Web) AutomationUpdate(w http.ResponseWriter, r *http.Request) {
	h.automationAction(w, r, "updated", func(ctx context.Context, id uuid.UUID) error {
		if err := parseForm(w, r); err != nil {
			return err
		}
		name := r.PostFormValue("name")
		trigger := r.PostFormValue("trigger")
		config := domain.AutomationTriggerConfig{MinCount: formMinCount(r)}
		// A form always posts every checkbox it rendered, so an empty set is a
		// real request for a rule with no actions — which the service refuses
		// with a message rather than storing.
		actions := r.PostForm["actions"]
		if actions == nil {
			actions = []string{}
		}
		_, err := h.Links.UpdateAutomationRule(ctx, IdentityFrom(ctx), id,
			link.UpdateAutomationRuleInput{
				Name: &name, Trigger: &trigger, TriggerConfig: &config, Actions: actions,
			})
		return err
	})
}

// AutomationToggle is the pause switch, on its own form.
//
// Its own action rather than a field on the edit form, for the reason the
// webhook, rule and split toggles have one: switching a misbehaving instruction
// off is what somebody reaches for first, and it must not require opening an
// editor.
//
// Switching one back **on** re-arms it — the watermark moves to now — so a rule
// paused for a month does not fire for a month of backlog the instant somebody
// unpauses it. That happens inside UpdateAutomationRule's statement; the notice
// below is where a person is told.
func (h *Web) AutomationToggle(w http.ResponseWriter, r *http.Request) {
	h.automationAction(w, r, "toggled", func(ctx context.Context, id uuid.UUID) error {
		existing, err := h.Links.AutomationRule(ctx, IdentityFrom(ctx), id)
		if err != nil {
			return err
		}
		flipped := !existing.Enabled
		_, err = h.Links.UpdateAutomationRule(ctx, IdentityFrom(ctx), id,
			link.UpdateAutomationRuleInput{Enabled: &flipped})
		return err
	})
}

func (h *Web) AutomationDelete(w http.ResponseWriter, r *http.Request) {
	h.automationAction(w, r, "deleted", func(ctx context.Context, id uuid.UUID) error {
		return h.Links.DeleteAutomationRule(ctx, IdentityFrom(ctx), id)
	})
}

func (h *Web) automationAction(
	w http.ResponseWriter, r *http.Request, marker string,
	do func(ctx context.Context, id uuid.UUID) error,
) {
	id, err := pathUUID(r, "automationID")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	h.finishAutomationAction(w, r, marker, do(r.Context(), id))
}

// finishAutomationAction is the one place an automation write turns into a
// response, on the shape finishWebhookAction uses: a refusal comes back on the
// page it was made from with the form still filled in, and anything that is not
// a validation error is a genuine failure.
func (h *Web) finishAutomationAction(
	w http.ResponseWriter, r *http.Request, marker string, err error,
) {
	if err != nil {
		var ve domain.ValidationErrors
		if !errors.As(err, &ve) {
			h.webError(w, r, err)
			return
		}
		data, ok := h.loadAutomationPage(w, r)
		if !ok {
			return
		}
		data.Error = ve[0].Message
		data.Notice = ""
		data.FormName = r.PostFormValue("name")
		data.FormMinCount = r.PostFormValue("min_count")
		data.Triggers = automationTriggerChoices(r.PostFormValue("trigger"))
		chosen := map[string]bool{}
		for _, a := range r.PostForm["actions"] {
			chosen[a] = true
		}
		data.Actions = automationActionChoices(chosen)
		h.renderAutomation(w, r, http.StatusUnprocessableEntity, data)
		return
	}

	if isHTMX(r) {
		r2 := r.Clone(r.Context())
		r2.URL.RawQuery = ""
		data, ok := h.loadAutomationPage(w, r2)
		if !ok {
			return
		}
		data.Notice = automationNotice(marker)
		h.renderAutomation(w, r, http.StatusOK, data)
		return
	}
	seeOther(w, r, "/automation?rule="+marker)
}

// formMinCount reads the threshold, treating anything unreadable as the floor.
//
// An unparseable value becomes 1 rather than an error, because the field is a
// number box with a default and the validation that matters — negative, or
// larger than one run can match — is in internal/domain where the API meets it
// too.
func formMinCount(r *http.Request) int {
	raw := strings.TrimSpace(r.PostFormValue("min_count"))
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 1
	}
	return n
}

func automationNotice(marker string) string {
	switch marker {
	case "created":
		return "Rule created and armed. It acts on what happens from now on, " +
			"never on what already happened."
	case "updated":
		return "Rule updated."
	case "toggled":
		return "Rule switched. Switching one back on re-arms it, so it does not " +
			"fire for everything that happened while it was off."
	case "deleted":
		return "Rule removed."
	default:
		return ""
	}
}
