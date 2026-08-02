package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Routing rules over the API (M34).
//
// Nested under the link, because a rule has no meaning apart from one and
// because "which link is this about" is then in the URL rather than in a body
// somebody has to get right. The service still scopes every read and write by
// workspace; the nesting adds the second check that a rule addressed through
// link A really belongs to link A.
//
// Thin, like every other handler here: parse, call the service, map the result.
// Every authorization and validation decision is in internal/link, so the
// dashboard forms get identical behaviour by calling the same methods rather
// than by remembering to repeat the checks.

type createRuleRequest struct {
	URL      string `json:"url"`
	Priority int32  `json:"priority"`
	// Conditions is taken as raw JSON rather than as the parsed struct, so that
	// the condition parser sees the keys the client actually sent. Decoding
	// straight into domain.RuleConditions would silently drop `cookies` — the one
	// condition this product refuses by name — and answer 201 as though the rule
	// had been created with it.
	Conditions json.RawMessage `json:"conditions"`
	// Enabled defaults to true when omitted, which is what a rule somebody just
	// wrote should be. A pointer, so "false" and "absent" stay different.
	Enabled *bool `json:"enabled"`
}

func (a *LinkAPI) CreateRule(w http.ResponseWriter, r *http.Request) {
	linkID, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req createRuleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	conds, err := parseConditions(req.Conditions)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	rule, err := a.Links.CreateRule(r.Context(), IdentityFrom(r.Context()), linkID, link.CreateRuleInput{
		URL: req.URL, Priority: req.Priority, Conditions: conds, Enabled: enabled,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, rule)
}

func (a *LinkAPI) ListRules(w http.ResponseWriter, r *http.Request) {
	linkID, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rules, err := a.Links.ListRules(r.Context(), IdentityFrom(r.Context()), linkID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The refused condition is advertised beside the supported ones rather than
	// only in the docs. A client building a rule editor discovers from this
	// response that there are twelve conditions and that the thirteenth is a
	// decision — which is the difference between "cookies are missing" and
	// "cookies are refused, here is the reason code".
	WriteJSON(w, http.StatusOK, map[string]any{
		"items":      rules,
		"conditions": domain.RuleConditionKinds,
		"refused": []map[string]string{{
			"condition": "cookies",
			"code":      domain.CodeCookiesRefused,
			"reason": "this redirect path sets no cookie and reads none; a cookie " +
				"condition would mean storing a per-visitor identifier the rest of " +
				"the product deliberately does not keep",
		}},
	})
}

type updateRuleRequest struct {
	URL        *string         `json:"url"`
	Priority   *int32          `json:"priority"`
	Conditions json.RawMessage `json:"conditions"`
	Enabled    *bool           `json:"enabled"`
}

func (a *LinkAPI) UpdateRule(w http.ResponseWriter, r *http.Request) {
	linkID, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ruleID, err := pathUUID(r, "ruleID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateRuleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	in := link.UpdateRuleInput{URL: req.URL, Priority: req.Priority, Enabled: req.Enabled}
	if len(req.Conditions) > 0 {
		conds, err := parseConditions(req.Conditions)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		in.Conditions = &conds
	}

	rule, err := a.Links.UpdateRule(r.Context(), IdentityFrom(r.Context()), linkID, ruleID, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, rule)
}

func (a *LinkAPI) DeleteRule(w http.ResponseWriter, r *http.Request) {
	linkID, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ruleID, err := pathUUID(r, "ruleID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.DeleteRule(r.Context(), IdentityFrom(r.Context()), linkID, ruleID); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseConditions turns the request's raw conditions into a condition set.
//
// An absent or empty body is passed to the parser rather than short-circuited,
// so that "no conditions" is refused by the one place that decides what a valid
// rule is — a rule with no conditions matches everybody and short-circuits every
// rule beneath it, and the message explaining that belongs in one file.
func parseConditions(raw json.RawMessage) (domain.RuleConditions, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	return domain.ParseRuleConditions(raw)
}
