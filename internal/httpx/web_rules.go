package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// The routing-rule half of the link detail page (M34).
//
// A form of typed boxes rather than a JSON textarea, because the dashboard has
// to be usable by somebody who has not read the API document — and because `ui`
// is stdlib-only with no Node and no CDN, so there is no rule builder to reach
// for. Each condition is one input, comma-separated where it takes a list, and
// the whole thing posts as an ordinary form.
//
// The one thing the form does *not* offer is a cookies condition, and it says
// so on the page rather than leaving a gap somebody has to infer. That is the
// same refusal the API answers with `cookies_not_supported`; a control absent
// with no explanation reads as an oversight, and this one is a decision.

// ruleView is a rule as the page shows it.
type ruleView struct {
	Rule domain.RoutingRule
	// Summary is the condition set in a sentence, because a person scanning a
	// list of rules needs to see what each one matches without opening it.
	Summary string
}

// ruleViews renders a rule list for the template.
func ruleViews(rules []domain.RoutingRule) []ruleView {
	out := make([]ruleView, 0, len(rules))
	for _, r := range rules {
		out = append(out, ruleView{Rule: r, Summary: summarizeConditions(r.Conditions)})
	}
	return out
}

// summarizeConditions describes a condition set in one line.
//
// Sorted, so the same rule reads the same way every time it is rendered — a Go
// map has no order, and a summary that shuffled between page loads would look
// like the rule was changing.
func summarizeConditions(c domain.RuleConditions) string {
	var parts []string
	add := func(label string, values []string) {
		if len(values) > 0 {
			parts = append(parts, label+" "+strings.Join(values, ", "))
		}
	}
	add("country", c.Country)
	add("region", c.Region)
	add("city", c.City)
	add("language", c.Language)
	add("browser", c.Browser)
	add("OS", c.OS)
	add("device", c.Device)
	add("referrer", c.Referrer)

	for _, pair := range []struct {
		label  string
		prefix string
		params map[string][]string
	}{{"query", "", c.Query}, {"utm", "utm_", c.UTM}} {
		names := make([]string, 0, len(pair.params))
		for name := range pair.params {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			values := pair.params[name]
			if len(values) == 0 {
				parts = append(parts, pair.label+" "+pair.prefix+name+" present")
				continue
			}
			parts = append(parts, pair.label+" "+pair.prefix+name+" = "+strings.Join(values, ", "))
		}
	}

	if c.Time != nil {
		var when []string
		if len(c.Time.Days) > 0 {
			when = append(when, strings.Join(c.Time.Days, ", "))
		}
		if c.Time.From != "" || c.Time.To != "" {
			from, to := c.Time.From, c.Time.To
			if from == "" {
				from = "00:00"
			}
			if to == "" {
				to = "24:00"
			}
			when = append(when, from+"–"+to)
		}
		tz := c.Time.TZ
		if tz == "" {
			tz = "UTC"
		}
		parts = append(parts, "time "+strings.Join(when, " ")+" ("+tz+")")
	}

	if c.Returning != nil {
		if *c.Returning {
			parts = append(parts, "seen earlier today")
		} else {
			parts = append(parts, "not seen earlier today")
		}
	}

	if len(parts) == 0 {
		return "matches everybody"
	}
	return strings.Join(parts, " · ")
}

// RuleCreate adds a rule from the link detail page.
func (h *Web) RuleCreate(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	in := link.CreateRuleInput{
		URL:        strings.TrimSpace(r.PostFormValue("rule_url")),
		Conditions: conditionsFromForm(r),
		Enabled:    r.PostFormValue("rule_enabled") != "",
	}
	if raw := strings.TrimSpace(r.PostFormValue("rule_priority")); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 || n > 100000 {
			h.rerenderWithRuleError(w, r, "Priority must be a whole number between 0 and 100000.")
			return
		}
		in.Priority = int32(n) //nolint:gosec // G109: range-checked on the line above
	}

	if _, err := h.Links.CreateRule(r.Context(), IdentityFrom(r.Context()), id, in); err != nil {
		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			h.rerenderWithRuleError(w, r, ruleErrorText(ve))
			return
		}
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/links/"+id.String()+"?rule=added")
}

// RuleToggle switches a rule on or off.
//
// A separate action from editing, because "stop this rule for now" is the thing
// somebody reaches for when a campaign misfires, and making them open a form to
// do it is the difference between a control and a control they will use.
func (h *Web) RuleToggle(w http.ResponseWriter, r *http.Request) {
	h.ruleAction(w, r, "toggled", func(ctx context.Context, linkID, ruleID uuid.UUID) error {
		if err := parseForm(w, r); err != nil {
			return err
		}
		enabled := r.PostFormValue("enabled") == "1"
		_, err := h.Links.UpdateRule(ctx, IdentityFrom(ctx),
			linkID, ruleID, link.UpdateRuleInput{Enabled: &enabled})
		return err
	})
}

// RuleDelete removes a rule.
func (h *Web) RuleDelete(w http.ResponseWriter, r *http.Request) {
	h.ruleAction(w, r, "deleted", func(ctx context.Context, linkID, ruleID uuid.UUID) error {
		return h.Links.DeleteRule(ctx, IdentityFrom(ctx), linkID, ruleID)
	})
}

func (h *Web) ruleAction(
	w http.ResponseWriter, r *http.Request, notice string,
	do func(ctx context.Context, linkID, ruleID uuid.UUID) error,
) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	ruleID, err := pathUUID(r, "ruleID")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := do(r.Context(), id, ruleID); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/links/"+id.String()+"?rule="+notice)
}

// rerenderWithRuleError puts the page back with the refusal above the rule
// form.
//
// The rule form's own values are not preserved, and that is worth being honest
// about rather than hiding: this page's other form re-renders its inputs, and
// doing the same for two dozen condition boxes would mean threading every one
// of them through the page data. The refusal names the field, the boxes are
// still filled in by the browser's own back behaviour on a 422, and the
// alternative was a form that could not be refused clearly.
func (h *Web) rerenderWithRuleError(w http.ResponseWriter, r *http.Request, message string) {
	data, ok := h.loadLinkDetail(w, r)
	if !ok {
		return
	}
	data.Error = message
	h.render(w, r, http.StatusUnprocessableEntity, "link_detail", data)
}

// ruleErrorText flattens a validation failure into one line.
func ruleErrorText(ve domain.ValidationErrors) string {
	msgs := make([]string, 0, len(ve))
	for _, e := range ve {
		msgs = append(msgs, e.Message)
	}
	if len(msgs) == 0 {
		return "The rule could not be saved."
	}
	return strings.Join(msgs, " ")
}

// conditionsFromForm reads a condition set out of the posted form.
//
// Anything left blank is simply absent, which is what makes a form with two
// dozen boxes usable: somebody filling in one of them gets a rule with one
// condition. The service refuses the case where they fill in none.
func conditionsFromForm(r *http.Request) domain.RuleConditions {
	list := func(name string) []string { return splitTags(r.PostFormValue(name)) }

	c := domain.RuleConditions{
		Country:  upperAll(list("cond_country")),
		Region:   upperAll(list("cond_region")),
		City:     list("cond_city"),
		Language: lowerAll(list("cond_language")),
		Browser:  list("cond_browser"),
		OS:       list("cond_os"),
		Device:   lowerAll(list("cond_device")),
		Referrer: lowerAll(list("cond_referrer")),
	}

	// One UTM box per convention-carrying parameter, because those three are
	// what campaign tracking actually uses and a generic key/value builder for
	// them would be more work to fill in for the common case.
	utm := map[string][]string{}
	for _, name := range []string{"source", "medium", "campaign"} {
		if values := list("cond_utm_" + name); len(values) > 0 {
			utm[name] = values
		}
	}
	if len(utm) > 0 {
		c.UTM = utm
	}

	// One general query parameter, for everything UTM does not cover. An empty
	// value list is a legitimate condition — "the parameter is present at all" —
	// so the name alone is enough to make one.
	if name := strings.TrimSpace(r.PostFormValue("cond_query_name")); name != "" {
		c.Query = map[string][]string{name: list("cond_query_values")}
	}

	days := r.PostForm["cond_time_days"]
	from := strings.TrimSpace(r.PostFormValue("cond_time_from"))
	to := strings.TrimSpace(r.PostFormValue("cond_time_to"))
	if len(days) > 0 || from != "" || to != "" {
		c.Time = &domain.RuleTime{
			Days: days, From: from, To: to,
			TZ: strings.TrimSpace(r.PostFormValue("cond_time_tz")),
		}
	}

	switch r.PostFormValue("cond_returning") {
	case "yes":
		yes := true
		c.Returning = &yes
	case "no":
		no := false
		c.Returning = &no
	}

	return c
}

func upperAll(in []string) []string {
	for i := range in {
		in[i] = strings.ToUpper(in[i])
	}
	return in
}

func lowerAll(in []string) []string {
	for i := range in {
		in[i] = strings.ToLower(in[i])
	}
	return in
}

// ruleNotice turns the ?rule= marker into a sentence.
func ruleNotice(marker string) string {
	switch marker {
	case "added":
		return "Routing rule added. It takes effect on the next request; cached snapshots for this link were cleared on every replica."
	case "deleted":
		return "Routing rule removed."
	case "toggled":
		return "Routing rule updated."
	default:
		return ""
	}
}

// ruleConditionHelp is the sentence the page carries about what is not here.
var ruleConditionHelp = fmt.Sprintf(
	"%d conditions. There is no cookies condition and that is deliberate: this "+
		"redirect path sets no cookie and reads none, and adding one would mean "+
		"storing a per-visitor identifier the rest of the product does not keep.",
	len(domain.RuleConditionKinds))
