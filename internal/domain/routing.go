package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Routing rules (M34).
//
// The whole of what a rule *means* lives here, for the same reason bots.go
// holds the whole of bot precedence: the redirect path asks "which destination
// does this request get", the API and the dashboard ask "is this a rule
// somebody can save", and a copy on each side is two answers that agree until
// somebody edits one.
//
// Nothing here reads a database, a cache, a request or a clock. Everything a
// condition needs arrives through RuleSubject, which is what lets the same
// evaluation run against a live request on the hot path and against a table of
// cases in a unit test.

// RuleConditions is the `conditions` jsonb of a routing_rules row.
//
// **Every present condition must hold, and within one condition any listed
// value matches.** AND across the keys, OR inside them. That is the reading
// people expect from a rule builder and it is the only one that composes: a
// rule that matched on *any* key could never be narrowed, because adding a
// condition would widen it.
//
// The zero value matches everything, which is why the validator refuses to
// store it — see ValidateRuleConditions. A rule that matches everything
// short-circuits every rule below it, and a person who wrote one by accident
// would see the rules underneath simply stop working.
//
// Every field is omitempty, so a stored row carries only the conditions
// somebody actually set, and a snapshot carries the same bytes rather than a
// dozen nulls per rule on the hottest path in the product.
type RuleConditions struct {
	// Geographic. Resolved transiently on the redirect path and never stored
	// against a click — see internal/geoip and the Analytics scope row.
	Country []string `json:"country,omitempty"`
	Region  []string `json:"region,omitempty"`
	City    []string `json:"city,omitempty"`

	// Derived from the request itself.
	Language []string `json:"language,omitempty"`
	Browser  []string `json:"browser,omitempty"`
	OS       []string `json:"os,omitempty"`
	Device   []string `json:"device,omitempty"`
	Referrer []string `json:"referrer,omitempty"`

	// Query is matched against the visitor's query string: the key is a
	// parameter name, the values are what that parameter may be.
	Query map[string][]string `json:"query,omitempty"`
	// UTM is the same test against `utm_`-prefixed parameters, with the prefix
	// left off the key. Separate from Query because it is one of the conditions
	// the scope row names, it has its own control in the dashboard, and writing
	// `utm_source` into a general query condition puts the campaign vocabulary
	// somewhere nothing can find it again.
	UTM map[string][]string `json:"utm,omitempty"`

	// Time is evaluated against the clock at request time, never at cache time.
	Time *RuleTime `json:"time,omitempty"`

	// Returning is the within-day returning-visitor test (D2). True requires a
	// visitor seen earlier today, false requires one that was not. A pointer
	// because "not set" and "must be new" are different conditions.
	Returning *bool `json:"returning,omitempty"`
}

// RuleTime is a date/time condition.
//
// Deliberately a weekday set and a time window rather than a calendar range.
// A campaign that runs "weekday office hours in London" is the thing people
// actually ask for; a fixed date range is expressed by the link's own expiry,
// which already exists and which the redirect path already honours.
type RuleTime struct {
	// Days are lowercase three-letter weekday names — mon, tue, wed, thu, fri,
	// sat, sun. Empty means every day.
	Days []string `json:"days,omitempty"`
	// From and To are "HH:MM" in TZ. A window whose To is before its From wraps
	// past midnight, which is what somebody writing 22:00–06:00 means.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// TZ is an IANA name. Empty is UTC.
	//
	// Stored as the name rather than as an offset, because an offset is wrong
	// twice a year: a rule written as +01:00 in July starts firing an hour late
	// in November, silently, on exactly the campaign somebody set up in summer.
	TZ string `json:"tz,omitempty"`
}

// RuleSubject is everything a rule may ask about a request.
//
// An interface rather than a struct of values, and that is the milestone's
// hot-path budget showing up in a type. A city lookup is an mmap walk and the
// returning-visitor test is a Redis round trip; neither may happen for a link
// whose rules do not ask. So the caller supplies something that resolves each
// answer when it is first wanted and remembers it, and Match asks for nothing
// it does not need.
//
// Every method returns the zero value when the answer is unknown — no database,
// an address that resolves to nothing, a header that was not sent. A condition
// tested against an unknown value does not match, which is the safe direction:
// an unresolvable request falls through to the link's own destination rather
// than being routed somewhere on the strength of a blank.
type RuleSubject interface {
	Country() string
	Region() string
	City() string
	Language() string
	Browser() string
	OS() string
	Device() string
	ReferrerHost() string
	// QueryParam returns every value the visitor sent for a parameter.
	QueryParam(name string) []string
	// Returning reports whether this visitor was seen earlier today. False when
	// there is no Redis to ask — see D2 and the returning-visitor docs.
	Returning() bool
	// Now is the instant the request is being decided at, which is the request's
	// own clock reading and never the one the snapshot was cached at.
	Now() time.Time
}

// Match reports whether a request satisfies every condition set on a rule.
//
// Order is by cost, not by correctness: an AND of independent tests gives the
// same answer whatever order it runs in, so the cheap ones — string comparisons
// against values the request already carries — run first and the two that can
// cost a lookup run last. A rule that pairs "mobile" with "city is London"
// therefore resolves no city at all for the ninety-odd percent of traffic that
// is not mobile.
func Match(c RuleConditions, s RuleSubject) bool {
	// Free: already parsed off the request line and headers.
	if len(c.Language) > 0 && !matchOne(c.Language, s.Language()) {
		return false
	}
	if len(c.Referrer) > 0 && !matchOne(c.Referrer, s.ReferrerHost()) {
		return false
	}
	if !matchQuery(c.Query, "", s) {
		return false
	}
	if !matchQuery(c.UTM, "utm_", s) {
		return false
	}

	// Cheap: one pass over the user agent, memoized by the subject, and only
	// for links whose rules mention it at all.
	if len(c.Device) > 0 && !matchOne(c.Device, s.Device()) {
		return false
	}
	if len(c.Browser) > 0 && !matchOne(c.Browser, s.Browser()) {
		return false
	}
	if len(c.OS) > 0 && !matchOne(c.OS, s.OS()) {
		return false
	}

	// Arithmetic, plus a timezone lookup that is cached after the first request
	// that uses it.
	if c.Time != nil && !c.Time.matches(s.Now()) {
		return false
	}

	// An mmap walk each, on a database an operator supplied.
	if len(c.Country) > 0 && !matchOne(c.Country, s.Country()) {
		return false
	}
	if len(c.Region) > 0 && !matchOne(c.Region, s.Region()) {
		return false
	}
	if len(c.City) > 0 && !matchOne(c.City, s.City()) {
		return false
	}

	// A Redis round trip, so it is last and it is only reached by a request
	// that has satisfied everything else on the rule.
	if c.Returning != nil && *c.Returning != s.Returning() {
		return false
	}
	return true
}

// matchOne reports whether a listed value equals the subject's,
// case-insensitively.
//
// The empty subject never matches, including against an empty string in the
// list. "No country resolved" is not the country "", and a rule that fired for
// every unresolvable request would route traffic on the strength of a missing
// database. Callers check for an unset condition before calling, so that the
// subject is never asked for an answer no rule wants.
func matchOne(want []string, have string) bool {
	if have == "" {
		return false
	}
	for _, w := range want {
		if strings.EqualFold(w, have) {
			return true
		}
	}
	return false
}

// matchQuery tests parameter conditions, optionally under a prefix.
//
// A condition with an empty value list means "the parameter is present at all",
// which is how somebody expresses "arrived from any campaign" without listing
// every campaign they will ever run.
func matchQuery(want map[string][]string, prefix string, s RuleSubject) bool {
	for name, values := range want {
		got := s.QueryParam(prefix + name)
		if len(got) == 0 {
			return false
		}
		if len(values) == 0 {
			continue
		}
		if !anyEqualFold(values, got) {
			return false
		}
	}
	return true
}

func anyEqualFold(want, got []string) bool {
	for _, w := range want {
		for _, g := range got {
			if strings.EqualFold(w, g) {
				return true
			}
		}
	}
	return false
}

// matches evaluates a time window against an instant.
//
// The instant is the caller's, always. Nothing here reads time.Now, which is
// what makes "never baked into the cached value" testable rather than promised:
// the same snapshot evaluated at two instants gives two answers.
func (t *RuleTime) matches(now time.Time) bool {
	local := now.In(t.location())

	if len(t.Days) > 0 {
		day := strings.ToLower(local.Format("Mon"))
		found := false
		for _, d := range t.Days {
			if strings.EqualFold(strings.TrimSpace(d), day) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	if t.From == "" && t.To == "" {
		return true
	}
	from, okFrom := parseClock(t.From)
	to, okTo := parseClock(t.To)
	if !okFrom {
		from = 0
	}
	if !okTo {
		to = 24 * 60
	}
	mins := local.Hour()*60 + local.Minute()

	// A window that ends before it starts is an overnight one. Without this,
	// 22:00–06:00 is an empty window and the rule never fires — which looks
	// exactly like the rule being broken rather than like the window being
	// misread.
	if to < from {
		return mins >= from || mins < to
	}
	return mins >= from && mins < to
}

// locations memoizes IANA lookups.
//
// time.LoadLocation reads the zoneinfo database on every call, and this runs on
// the redirect path. A sync.Map keyed by name is enough: the set of timezones a
// deployment's rules name is small, bounded by what people typed, and never
// invalidated because a zone's identity does not change even when its rules do.
var locations sync.Map

// location resolves the condition's timezone, falling back to UTC.
//
// A name this binary cannot resolve becomes UTC rather than an error, because
// there is no error return on the redirect path and refusing to redirect over a
// timezone would be worse than evaluating the window an hour off. The validator
// is where an unresolvable name is refused, so a rule that reaches here with one
// was written by a build that could resolve it — which is why cmd/linkctrl
// embeds the zoneinfo database rather than trusting the base image to carry it.
func (t *RuleTime) location() *time.Location {
	if t.TZ == "" {
		return time.UTC
	}
	if v, ok := locations.Load(t.TZ); ok {
		if loc, ok := v.(*time.Location); ok {
			return loc
		}
		return time.UTC
	}
	loc, err := time.LoadLocation(t.TZ)
	if err != nil {
		loc = time.UTC
	}
	locations.Store(t.TZ, loc)
	return loc
}

// parseClock reads "HH:MM" into minutes past midnight.
func parseClock(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	hh, err := strconv.Atoi(h)
	if err != nil || hh < 0 || hh > 23 {
		return 0, false
	}
	mm, err := strconv.Atoi(m)
	if err != nil || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// --- what a set of rules needs ----------------------------------------------

// RuleNeeds is which expensive lookups a link's rules can ask for.
//
// Computed once per request from the snapshot's rules — a walk over a handful
// of structs — so that a link whose rules never mention a city resolves no city,
// and a link whose rules never mention a returning visitor makes no Redis call.
// The alternative is asking the subject and letting it be lazy, which works for
// the redirect path and does not work for the click recorder: whether to
// *maintain* the returning-visitor set is a decision taken before any condition
// is evaluated.
type RuleNeeds struct {
	Country   bool
	Region    bool
	City      bool
	UserAgent bool
	Returning bool
}

// Geo reports whether any geographic lookup is wanted.
func (n RuleNeeds) Geo() bool { return n.Country || n.Region || n.City }

// NeedsOf summarizes what a whole rule list can ask for.
func NeedsOf(conds []RuleConditions) RuleNeeds {
	var n RuleNeeds
	for _, c := range conds {
		n.Country = n.Country || len(c.Country) > 0
		n.Region = n.Region || len(c.Region) > 0
		n.City = n.City || len(c.City) > 0
		n.UserAgent = n.UserAgent ||
			len(c.Device) > 0 || len(c.Browser) > 0 || len(c.OS) > 0
		n.Returning = n.Returning || c.Returning != nil
	}
	return n
}

// --- validation --------------------------------------------------------------

// CodeCookiesRefused is the reason code a cookies condition is refused with
// (D2).
//
// A code rather than only a message, because this refusal is a documented
// product decision and not a typo: the redirect path sets no cookies and reads
// none, so a cookie condition would either be a lie or would make the shortener
// start storing a per-visitor identifier — which is the thing the whole
// analytics design is built to avoid. Twelve conditions ship; the thirteenth is
// refused by name so that nobody has to guess whether it was forgotten.
const CodeCookiesRefused = "cookies_not_supported"

// RuleConditionKinds are the condition names accepted in the jsonb, in the
// order the dashboard presents them.
var RuleConditionKinds = []string{
	"country", "region", "city", "language", "browser", "os", "device",
	"time", "referrer", "query", "utm", "returning",
}

// ruleDevices are the buckets analytics.Classify produces. Listed here rather
// than imported so that domain stays free of the analytics package; the two are
// pinned together by TestRuleDeviceVocabularyMatchesTheClassifier.
var ruleDevices = []string{"desktop", "mobile", "tablet", "bot", "unknown"}

// RuleWeekdays is the weekday vocabulary a time condition may use.
//
// Exported because the dashboard's rule form renders the same list. Two copies
// of it would be a form offering a day the validator refuses, which reads as the
// form being broken.
var RuleWeekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// ParseRuleConditions reads conditions from the wire, refusing anything it does
// not understand.
//
// Strict about unknown keys for the reason decodeJSON is strict about unknown
// fields: a client that misspells `contry` and gets a 200 believes it has a
// geographic rule, and what it has is a rule that matches everybody. The one
// unknown key with a message of its own is `cookies`.
func ParseRuleConditions(raw []byte) (RuleConditions, error) {
	var c RuleConditions

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return c, ValidationErrors{{
			Field: "conditions", Code: "invalid",
			Message: "conditions must be a JSON object",
		}}
	}

	var errs ValidationErrors
	known := map[string]bool{}
	for _, k := range RuleConditionKinds {
		known[k] = true
	}
	for k := range keys {
		switch {
		case known[k]:
		case k == "cookie" || k == "cookies":
			errs = append(errs, FieldError{
				Field: "conditions.cookies", Code: CodeCookiesRefused,
				Message: "conditions on cookies are not supported: this redirect " +
					"path sets no cookie and reads none, and adding one would mean " +
					"storing a per-visitor identifier the rest of the product " +
					"deliberately does not keep",
			})
		default:
			errs = append(errs, FieldError{
				Field: "conditions." + k, Code: "unknown",
				Message: fmt.Sprintf("%q is not a condition; the supported ones are %s",
					k, strings.Join(RuleConditionKinds, ", ")),
			})
		}
	}
	if len(errs) > 0 {
		return c, errs
	}

	if err := json.Unmarshal(raw, &c); err != nil {
		return c, ValidationErrors{{
			Field: "conditions", Code: "invalid",
			Message: "conditions are not in the expected shape: " + err.Error(),
		}}
	}
	return c, ValidateRuleConditions(c)
}

// ValidateRuleConditions checks a decoded condition set, returning nil or
// ValidationErrors.
//
//nolint:gocyclo // one branch per condition; splitting it hides the list.
func ValidateRuleConditions(c RuleConditions) error {
	var errs ValidationErrors
	field := func(name, code, msg string) {
		errs = append(errs, FieldError{Field: "conditions." + name, Code: code, Message: msg})
	}

	if IsEmptyRuleConditions(c) {
		field("", "required",
			"a rule needs at least one condition; a rule with none matches every "+
				"visitor and stops every rule below it from being reached")
		return errs
	}

	for _, iso := range c.Country {
		if len(iso) != 2 || !isAlpha(iso) {
			field("country", "invalid",
				fmt.Sprintf("%q is not an ISO 3166-1 alpha-2 country code", iso))
		}
	}
	for _, d := range c.Device {
		if !containsFold(ruleDevices, d) {
			field("device", "invalid",
				fmt.Sprintf("%q is not a device; use one of %s", d, strings.Join(ruleDevices, ", ")))
		}
	}
	for _, l := range c.Language {
		if l == "" || len(l) > 8 {
			field("language", "invalid",
				fmt.Sprintf("%q is not a language subtag", l))
		}
	}
	// Every free-text list is length-capped, and so is the number of entries.
	// These travel inside the cached snapshot on the redirect path, so an
	// unbounded list is an unbounded payload on the hottest read in the product.
	for name, list := range map[string][]string{
		"country": c.Country, "region": c.Region, "city": c.City,
		"language": c.Language, "browser": c.Browser, "os": c.OS,
		"device": c.Device, "referrer": c.Referrer,
	} {
		if len(list) > MaxRuleConditionValues {
			field(name, "too_many", fmt.Sprintf(
				"at most %d values per condition", MaxRuleConditionValues))
		}
		for _, v := range list {
			if len(v) > MaxRuleValueLength {
				field(name, "too_long", fmt.Sprintf(
					"each value must be at most %d characters", MaxRuleValueLength))
				break
			}
		}
	}
	for _, params := range []map[string][]string{c.Query, c.UTM} {
		for name, values := range params {
			if name == "" || len(name) > MaxRuleValueLength {
				field("query", "invalid", "a parameter name is required")
			}
			if len(values) > MaxRuleConditionValues {
				field("query", "too_many", fmt.Sprintf(
					"at most %d values per parameter", MaxRuleConditionValues))
			}
		}
	}

	if c.Time != nil {
		for _, d := range c.Time.Days {
			if !containsFold(RuleWeekdays, d) {
				field("time.days", "invalid",
					fmt.Sprintf("%q is not a weekday; use %s", d, strings.Join(RuleWeekdays, ", ")))
			}
		}
		if c.Time.From != "" {
			if _, ok := parseClock(c.Time.From); !ok {
				field("time.from", "invalid", "a time must be written as HH:MM")
			}
		}
		if c.Time.To != "" {
			if _, ok := parseClock(c.Time.To); !ok {
				field("time.to", "invalid", "a time must be written as HH:MM")
			}
		}
		if c.Time.TZ != "" {
			if _, err := time.LoadLocation(c.Time.TZ); err != nil {
				field("time.tz", "invalid",
					fmt.Sprintf("%q is not an IANA timezone name, such as Europe/London", c.Time.TZ))
			}
		}
		if len(c.Time.Days) == 0 && c.Time.From == "" && c.Time.To == "" {
			field("time", "required", "a time condition needs days, a window, or both")
		}
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// MaxRuleConditionValues and MaxRuleValueLength bound what one condition may
// hold. Both are about the snapshot rather than about the form: these bytes are
// serialized and parsed on every cache miss for the link that carries them.
const (
	MaxRuleConditionValues = 50
	MaxRuleValueLength     = 128
)

// IsEmptyRuleConditions reports whether nothing at all is set.
func IsEmptyRuleConditions(c RuleConditions) bool {
	return len(c.Country) == 0 && len(c.Region) == 0 && len(c.City) == 0 &&
		len(c.Language) == 0 && len(c.Browser) == 0 && len(c.OS) == 0 &&
		len(c.Device) == 0 && len(c.Referrer) == 0 &&
		len(c.Query) == 0 && len(c.UTM) == 0 &&
		c.Time == nil && c.Returning == nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

func isAlpha(s string) bool {
	for i := range s {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// --- the rule itself ---------------------------------------------------------

// RoutingRule is a rule as the API and the dashboard see it.
type RoutingRule struct {
	ID     uuid.UUID `json:"id"`
	LinkID uuid.UUID `json:"link_id"`
	// Priority orders evaluation: lower wins, and the first rule that matches
	// short-circuits. Ties are broken by creation order so that two rules with
	// the same priority still evaluate deterministically.
	Priority   int32          `json:"priority"`
	URL        string         `json:"url"`
	Conditions RuleConditions `json:"conditions"`
	Enabled    bool           `json:"enabled"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// The four rule kinds. `match` is M34's and is the only one that reads a
// visitor's conditions; the other three are M36's and are chosen rather than
// matched.
//
// The separation is what keeps M34's promise that shipping M36 could not
// retroactively change what a match rule does: every query M34 wrote filters on
// RuleKindMatch, every query M36 wrote excludes it, and no query reads both.
const (
	RuleKindMatch = "match"
	// RuleKindWeighted is one arm of a weighted split. Its share of the traffic
	// is its destination's `weight` over the sum of the enabled arms' weights,
	// which is what makes "60/40" and "600/400" the same test.
	RuleKindWeighted = "weighted"
	// RuleKindSequential is one arm of a strict rotation (D8). Arms are visited
	// in creation order, once each, forever, using a durable counter rather than
	// anything held in a process.
	RuleKindSequential = "sequential"
	// RuleKindFallback is where a link sends anybody no rule claimed. At most one
	// per link, and it stands in for the link's own destination without changing
	// it — which is what makes turning it off a reversible act rather than an
	// edit to the link.
	RuleKindFallback = "fallback"
)

// SplitKinds are the kinds that make a link a split test. Order is the order the
// dashboard offers them in.
var SplitKinds = []string{RuleKindWeighted, RuleKindSequential}

// IsVariantKind reports whether a kind is one M36 manages.
func IsVariantKind(kind string) bool {
	switch kind {
	case RuleKindWeighted, RuleKindSequential, RuleKindFallback:
		return true
	default:
		return false
	}
}

// MaxDestinationWeight bounds one arm's weight.
//
// A ceiling rather than none, because the weights of a link's arms are summed on
// the redirect path and the sum has to stay somewhere an int32 can hold it
// however many arms there are. Ten thousand is four decimal places of a
// percentage split, which is more resolution than a test with a readable result
// will ever need.
const MaxDestinationWeight = 10_000

// MaxVariantsPerLink bounds a split.
//
// Below MaxRulesPerLink deliberately: a link may carry match rules *and* a
// split, both travel in the same cached snapshot, and the two ceilings together
// are what bound the payload. Eight arms is past the point where a split test
// produces a result anybody can act on.
const MaxVariantsPerLink = 8

// Variant is one arm of a split test, as the API and the dashboard see it.
//
// The same shape as RoutingRule with the condition set replaced by a weight,
// because it is the same row: a rule, a destination, an enabled flag. Kept as its
// own type rather than adding two nullable fields to RoutingRule, so that a
// client reading a rule list cannot be handed a weight that means nothing and a
// client reading a split cannot be handed conditions that are never evaluated.
type Variant struct {
	ID     uuid.UUID `json:"id"`
	LinkID uuid.UUID `json:"link_id"`
	Kind   string    `json:"kind"`
	URL    string    `json:"url"`
	// Weight is the arm's share, and it is meaningful only for `weighted`. A
	// sequential arm carries whatever weight its destination row has and nothing
	// reads it, which is the honest encoding of "this kind does not use weights"
	// — the alternative is a column that is NULL for half the rows in a table
	// whose CHECK says it cannot be.
	Weight int32 `json:"weight"`
	// Share is Weight over the sum of the link's enabled weighted arms, as a
	// percentage, or zero when the link's split is not weighted. Computed for
	// the reader rather than stored, because it changes whenever any *other* arm
	// changes and a stored copy would be wrong the moment one did.
	Share     float64   `json:"share"`
	Enabled   bool      `json:"enabled"`
	Position  int32     `json:"position"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Split is a link's whole split test.
type Split struct {
	// Kind is the kind of the link's arms — weighted or sequential — or "" when
	// the link has none. A link's arms are all one kind; see ValidateSplitKind.
	Kind     string    `json:"kind"`
	Variants []Variant `json:"variants"`
	// Fallback is the link's fallback rule, if it has one.
	Fallback *Variant `json:"fallback,omitempty"`
}

// ValidateWeight refuses a weight the redirect path could not honour.
//
// Zero is permitted and means "this arm receives nothing" — a way to park an arm
// of a running test without deleting it and losing the clicks already attributed
// to its destination. Every arm at zero is refused by ValidateSplit, because a
// split that can choose nothing is a link whose destination silently reverts.
func ValidateWeight(weight int32) ValidationErrors {
	if weight < 0 || weight > MaxDestinationWeight {
		return ValidationErrors{{
			Field: "weight", Code: "out_of_range",
			Message: fmt.Sprintf("a weight must be between 0 and %d", MaxDestinationWeight),
		}}
	}
	return nil
}

// ValidateSplitKind refuses a kind, and refuses mixing two.
//
// A link's arms are all weighted or all sequential, and that is a product
// decision rather than a limitation: "40% of visitors, in rotation" has no
// meaning, and letting the two kinds coexist would mean the redirect path
// deciding which one wins — a precedence rule nobody asked for, applied to a
// state nobody meant to create.
func ValidateSplitKind(kind, existing string) ValidationErrors {
	switch kind {
	case RuleKindWeighted, RuleKindSequential:
	default:
		return ValidationErrors{{
			Field: "kind", Code: "unsupported",
			Message: "a split arm is `weighted` or `sequential`",
		}}
	}
	if existing != "" && existing != kind {
		return ValidationErrors{{
			Field: "kind", Code: "conflict",
			Message: fmt.Sprintf(
				"this link's split is already %s; a link's arms are all one kind, "+
					"because \"a share of the traffic, in rotation\" has no meaning. "+
					"Remove the %s arms first.", existing, existing),
		}}
	}
	return nil
}

// Shares fills in each arm's percentage of a weighted split.
//
// Returns the variants unchanged for a sequential split, where every enabled arm
// receives one visitor in N and a percentage would be a second, less exact way of
// saying so.
func Shares(kind string, variants []Variant) []Variant {
	if kind != RuleKindWeighted {
		return variants
	}
	var total int32
	for _, v := range variants {
		if v.Enabled {
			total += v.Weight
		}
	}
	if total <= 0 {
		return variants
	}
	for i := range variants {
		if variants[i].Enabled {
			variants[i].Share = float64(variants[i].Weight) * 100 / float64(total)
		}
	}
	return variants
}

// MaxRulesPerLink bounds a link's rule list.
//
// A ceiling rather than no ceiling, because the list is evaluated in order on
// the redirect path and travels inside the cached snapshot. Twenty is well
// past what a rule builder is usable at and far short of what would be
// measurable against a 20ms budget.
const MaxRulesPerLink = 20
