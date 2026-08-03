package httpx

import (
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Automation rules over the API (M43).
//
// A sibling collection rather than a subresource of a link, exactly as webhooks
// and campaigns are: a rule belongs to the workspace and watches every link in
// it, so nesting it under one link would be a lie about what it watches.
//
// Thin, like every other handler here — every authorization and validation
// decision is in internal/link, so the dashboard forms get identical behaviour
// by calling the same methods.
//
// **There is no "run now" endpoint**, and its absence is the milestone's first
// claim showing through: evaluation runs on the leader-elected scheduler and
// nowhere else. An endpoint that evaluated on demand would put trigger matching,
// notification writes and link archiving on the request path of whoever pressed
// it, which is exactly what m43.md says must not happen.

type createAutomationRuleRequest struct {
	Name          string                         `json:"name"`
	Trigger       string                         `json:"trigger"`
	TriggerConfig domain.AutomationTriggerConfig `json:"trigger_config"`
	Actions       []string                       `json:"actions"`
	// Enabled defaults to true when omitted, which is what a rule somebody just
	// wrote should be. A pointer, so "false" and "absent" stay different.
	Enabled *bool `json:"enabled"`
}

type updateAutomationRuleRequest struct {
	Name          *string                         `json:"name"`
	Trigger       *string                         `json:"trigger"`
	TriggerConfig *domain.AutomationTriggerConfig `json:"trigger_config"`
	Actions       []string                        `json:"actions"`
	Enabled       *bool                           `json:"enabled"`
}

func (a *LinkAPI) ListAutomationRules(w http.ResponseWriter, r *http.Request) {
	rules, err := a.Links.AutomationRules(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The vocabulary travels with the answer, the way ListWebhooks advertises its
	// events and ListRules its conditions: a client building a rule editor
	// discovers what it may write from the response rather than from this file.
	//
	// The bounds travel too, and they are not decoration. `evaluation` is what
	// makes "why has my rule not fired yet" answerable without reading the
	// source: it says how often the scheduler looks, how many subjects one rule
	// handles per look, and that evaluation happens there rather than here.
	WriteJSON(w, http.StatusOK, map[string]any{
		"rules":    rules,
		"triggers": domain.AutomationTriggers,
		"actions":  domain.AutomationActions,
		"evaluation": map[string]any{
			"runs_on":              "the leader-elected scheduler, never a request",
			"interval_seconds":     int(domain.AutomationInterval.Seconds()),
			"rules_per_run":        domain.AutomationRulesPerRun,
			"matches_per_rule":     domain.AutomationMatchesPerRule,
			"max_rules_per_space":  domain.MaxAutomationRulesPerWorkspace,
			"max_actions_per_rule": domain.MaxAutomationActions,
			"documentation_url":    "/docs/usage.md#automation-rules",
		},
	})
}

func (a *LinkAPI) CreateAutomationRule(w http.ResponseWriter, r *http.Request) {
	var req createAutomationRuleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rule, err := a.Links.CreateAutomationRule(r.Context(), IdentityFrom(r.Context()),
		link.CreateAutomationRuleInput{
			Name: req.Name, Trigger: req.Trigger, TriggerConfig: req.TriggerConfig,
			Actions: req.Actions, Enabled: enabled,
		})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, rule)
}

func (a *LinkAPI) GetAutomationRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "automationID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rule, err := a.Links.AutomationRule(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, rule)
}

func (a *LinkAPI) UpdateAutomationRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "automationID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateAutomationRuleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	rule, err := a.Links.UpdateAutomationRule(r.Context(), IdentityFrom(r.Context()), id,
		link.UpdateAutomationRuleInput{
			Name: req.Name, Trigger: req.Trigger, TriggerConfig: req.TriggerConfig,
			Actions: req.Actions, Enabled: req.Enabled,
		})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, rule)
}

func (a *LinkAPI) DeleteAutomationRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "automationID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.DeleteAutomationRule(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
