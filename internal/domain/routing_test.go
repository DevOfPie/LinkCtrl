package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// testSubject is a request as a rule sees it, with every answer supplied.
//
// It also counts what was asked, because half of what M34 promises is about
// *not* asking: a city lookup is an mmap walk and the returning-visitor test is
// a Redis round trip, and neither may happen for a rule that does not mention
// them.
type testSubject struct {
	country, region, city                string
	language, browser, os, device, refer string
	query                                map[string][]string
	now                                  time.Time
	returning                            bool

	asked map[string]int
}

func newSubject() *testSubject {
	return &testSubject{
		now:   time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
		asked: map[string]int{},
	}
}

func (s *testSubject) count(what string) { s.asked[what]++ }

func (s *testSubject) Country() string  { s.count("country"); return s.country }
func (s *testSubject) Region() string   { s.count("region"); return s.region }
func (s *testSubject) City() string     { s.count("city"); return s.city }
func (s *testSubject) Language() string { s.count("language"); return s.language }
func (s *testSubject) Browser() string  { s.count("browser"); return s.browser }
func (s *testSubject) OS() string       { s.count("os"); return s.os }
func (s *testSubject) Device() string   { s.count("device"); return s.device }
func (s *testSubject) ReferrerHost() string {
	s.count("referrer")
	return s.refer
}

func (s *testSubject) QueryParam(name string) []string {
	s.count("query")
	return s.query[name]
}
func (s *testSubject) Returning() bool { s.count("returning"); return s.returning }
func (s *testSubject) Now() time.Time  { return s.now }

func ptr[T any](v T) *T { return &v }

// TestMatchCoversEveryCondition walks all twelve, in both directions.
//
// A table rather than one test per condition, because the property that matters
// is that they behave the same way as each other: present-and-satisfied
// matches, present-and-unsatisfied does not, and absent-from-the-subject never
// matches a condition that was set.
func TestMatchCoversEveryCondition(t *testing.T) {
	base := func(mut func(s *testSubject)) *testSubject {
		s := newSubject()
		s.country, s.region, s.city = "GB", "ENG", "Fictionbury"
		s.language, s.browser, s.os, s.device = "en", "Chrome", "iOS", "mobile"
		s.refer = "news.example.com"
		s.query = map[string][]string{"plan": {"pro"}, "utm_source": {"newsletter"}}
		s.returning = true
		if mut != nil {
			mut(s)
		}
		return s
	}

	cases := []struct {
		name string
		cond RuleConditions
		mut  func(*testSubject)
		want bool
	}{
		{"country matches", RuleConditions{Country: []string{"IE", "GB"}}, nil, true},
		{"country does not", RuleConditions{Country: []string{"IE"}}, nil, false},
		{"country is case-insensitive", RuleConditions{Country: []string{"gb"}}, nil, true},
		{"country unresolved never matches", RuleConditions{Country: []string{"GB"}},
			func(s *testSubject) { s.country = "" }, false},

		{"region matches", RuleConditions{Region: []string{"ENG"}}, nil, true},
		{"region does not", RuleConditions{Region: []string{"SCT"}}, nil, false},

		{"city matches", RuleConditions{City: []string{"Fictionbury"}}, nil, true},
		{"city does not", RuleConditions{City: []string{"Beispielstadt"}}, nil, false},

		{"language matches", RuleConditions{Language: []string{"en"}}, nil, true},
		{"language does not", RuleConditions{Language: []string{"de"}}, nil, false},

		{"browser matches", RuleConditions{Browser: []string{"Chrome"}}, nil, true},
		{"browser does not", RuleConditions{Browser: []string{"Firefox"}}, nil, false},

		{"os matches", RuleConditions{OS: []string{"iOS"}}, nil, true},
		{"os does not", RuleConditions{OS: []string{"Android"}}, nil, false},

		{"device matches", RuleConditions{Device: []string{"mobile"}}, nil, true},
		{"device does not", RuleConditions{Device: []string{"desktop"}}, nil, false},

		{"referrer matches", RuleConditions{Referrer: []string{"news.example.com"}}, nil, true},
		{"referrer does not", RuleConditions{Referrer: []string{"other.example.com"}}, nil, false},
		{"referrer absent never matches", RuleConditions{Referrer: []string{"news.example.com"}},
			func(s *testSubject) { s.refer = "" }, false},

		{"query value matches", RuleConditions{Query: map[string][]string{"plan": {"pro", "team"}}}, nil, true},
		{"query value does not", RuleConditions{Query: map[string][]string{"plan": {"free"}}}, nil, false},
		{"query presence alone matches", RuleConditions{Query: map[string][]string{"plan": nil}}, nil, true},
		{"query absent parameter", RuleConditions{Query: map[string][]string{"coupon": nil}}, nil, false},

		{"utm matches through the prefix", RuleConditions{UTM: map[string][]string{"source": {"newsletter"}}}, nil, true},
		{"utm does not", RuleConditions{UTM: map[string][]string{"source": {"twitter"}}}, nil, false},

		{"returning matches", RuleConditions{Returning: ptr(true)}, nil, true},
		{"returning is not new", RuleConditions{Returning: ptr(false)}, nil, false},
		{"new visitor matches new", RuleConditions{Returning: ptr(false)},
			func(s *testSubject) { s.returning = false }, true},

		{"time inside the window", RuleConditions{
			Time: &RuleTime{Days: []string{"sun"}, From: "09:00", To: "17:00"}}, nil, true},
		{"time outside the window", RuleConditions{
			Time: &RuleTime{Days: []string{"sun"}, From: "13:00", To: "17:00"}}, nil, false},
		{"time on the wrong day", RuleConditions{
			Time: &RuleTime{Days: []string{"mon"}}}, nil, false},

		// AND across keys: every one present must hold.
		{"two conditions, both hold", RuleConditions{
			Country: []string{"GB"}, Device: []string{"mobile"}}, nil, true},
		{"two conditions, one fails", RuleConditions{
			Country: []string{"GB"}, Device: []string{"desktop"}}, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.cond, base(tc.mut)); got != tc.want {
				t.Errorf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMatchAsksOnlyWhatTheRuleNeeds is the hot-path claim, tested by counting.
//
// The city lookup and the returning-visitor round trip are the two expensive
// answers on this path. A rule that does not mention them must not cause them,
// and a rule that fails on a cheap condition must not reach them either —
// otherwise every mobile-only rule pays for a geo lookup on desktop traffic.
func TestMatchAsksOnlyWhatTheRuleNeeds(t *testing.T) {
	t.Run("a rule that names nothing expensive asks nothing expensive", func(t *testing.T) {
		s := newSubject()
		s.device = "mobile"
		Match(RuleConditions{Device: []string{"mobile"}}, s)
		for _, expensive := range []string{"country", "region", "city", "returning"} {
			if s.asked[expensive] != 0 {
				t.Errorf("a device-only rule asked for %s %d times", expensive, s.asked[expensive])
			}
		}
	})

	t.Run("a failed cheap condition short-circuits the expensive ones", func(t *testing.T) {
		s := newSubject()
		s.device = "desktop"
		s.country = "GB"
		if Match(RuleConditions{Device: []string{"mobile"}, Country: []string{"GB"}, Returning: ptr(true)}, s) {
			t.Fatal("a rule matched despite the device condition failing")
		}
		if s.asked["country"] != 0 || s.asked["returning"] != 0 {
			t.Errorf("the expensive conditions were evaluated after a cheap one failed: "+
				"country=%d returning=%d", s.asked["country"], s.asked["returning"])
		}
	})

	t.Run("returning is asked last and only once", func(t *testing.T) {
		s := newSubject()
		s.country, s.device, s.returning = "GB", "mobile", true
		if !Match(RuleConditions{
			Country: []string{"GB"}, Device: []string{"mobile"}, Returning: ptr(true)}, s) {
			t.Fatal("the rule should have matched")
		}
		if s.asked["returning"] != 1 {
			t.Errorf("returning was asked %d times, want exactly 1", s.asked["returning"])
		}
	})
}

// TestTimeIsEvaluatedAgainstTheClock is the "never baked into the cached value"
// bullet, tested across a boundary.
//
// The *same* condition value is evaluated at two instants either side of 17:00
// and gives two answers. A condition that had been resolved when the snapshot
// was built could not do that, so this is the shape of the claim rather than a
// paraphrase of it.
func TestTimeIsEvaluatedAgainstTheClock(t *testing.T) {
	cond := RuleConditions{Time: &RuleTime{From: "09:00", To: "17:00", TZ: "Europe/London"}}

	// 15:59 and 16:01 UTC, which are 16:59 and 17:01 in London during BST — so
	// the boundary is crossed by two minutes of wall clock, and the timezone is
	// doing real work rather than being decoration.
	inside := newSubject()
	inside.now = time.Date(2026, 8, 2, 15, 59, 0, 0, time.UTC)
	outside := newSubject()
	outside.now = time.Date(2026, 8, 2, 16, 1, 0, 0, time.UTC)

	if !Match(cond, inside) {
		t.Errorf("16:59 Europe/London is inside 09:00–17:00 and did not match")
	}
	if Match(cond, outside) {
		t.Errorf("17:01 Europe/London is outside 09:00–17:00 and matched")
	}
}

func TestOvernightWindowWraps(t *testing.T) {
	cond := RuleConditions{Time: &RuleTime{From: "22:00", To: "06:00"}}
	for _, tc := range []struct {
		hour int
		want bool
	}{{23, true}, {2, true}, {5, true}, {6, false}, {12, false}, {21, false}, {22, true}} {
		s := newSubject()
		s.now = time.Date(2026, 8, 2, tc.hour, 30, 0, 0, time.UTC)
		if got := Match(cond, s); got != tc.want {
			t.Errorf("%02d:30 against 22:00–06:00 = %v, want %v", tc.hour, got, tc.want)
		}
	}
}

// TestCookiesConditionIsRefusedByName is decision D2 made observable.
//
// Not "an unknown key is rejected" — that is the generic branch and it is
// tested beside this one. The point is that `cookies` gets a reason code of its
// own, so a client can tell "you misspelled something" from "this product does
// not do that, and here is why".
func TestCookiesConditionIsRefusedByName(t *testing.T) {
	for _, spelling := range []string{`{"cookies":["session"]}`, `{"cookie":["session"]}`} {
		_, err := ParseRuleConditions([]byte(spelling))
		var ve ValidationErrors
		if !errors.As(err, &ve) {
			t.Fatalf("%s was accepted; the cookies condition must be refused", spelling)
		}
		found := false
		for _, e := range ve {
			if e.Code == CodeCookiesRefused {
				found = true
				if !strings.Contains(e.Message, "identifier") {
					t.Errorf("the refusal does not say why: %q", e.Message)
				}
			}
		}
		if !found {
			t.Errorf("%s was refused without the %s code: %+v", spelling, CodeCookiesRefused, ve)
		}
	}
}

func TestUnknownConditionIsRefusedGenerically(t *testing.T) {
	_, err := ParseRuleConditions([]byte(`{"contry":["GB"]}`))
	var ve ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatal("a misspelled condition was accepted")
	}
	if ve[0].Code == CodeCookiesRefused {
		t.Errorf("a misspelled condition was reported as the cookies refusal")
	}
	if !strings.Contains(ve[0].Message, "country") {
		t.Errorf("the refusal does not list the supported conditions: %q", ve[0].Message)
	}
}

// A rule with no conditions matches everybody and short-circuits every rule
// beneath it. Storing one is how somebody's other rules silently stop working.
func TestEmptyConditionsAreRefused(t *testing.T) {
	if _, err := ParseRuleConditions([]byte(`{}`)); err == nil {
		t.Fatal("a rule with no conditions was accepted")
	}
	if err := ValidateRuleConditions(RuleConditions{}); err == nil {
		t.Fatal("an empty condition set validated")
	}
}

func TestConditionValidation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"a country code", `{"country":["GB"]}`, true},
		{"a three-letter country", `{"country":["GBR"]}`, false},
		{"a numeric country", `{"country":["G1"]}`, false},
		{"a known device", `{"device":["tablet"]}`, true},
		{"an invented device", `{"device":["phablet"]}`, false},
		{"a weekday", `{"time":{"days":["mon","fri"]}}`, true},
		{"a misspelled weekday", `{"time":{"days":["monday"]}}`, false},
		{"a clock time", `{"time":{"from":"09:00","to":"17:30"}}`, true},
		{"a nonsense clock time", `{"time":{"from":"25:00"}}`, false},
		{"an IANA zone", `{"time":{"days":["mon"],"tz":"Europe/London"}}`, true},
		{"an offset instead of a zone", `{"time":{"days":["mon"],"tz":"+01:00"}}`, false},
		{"a time condition with nothing in it", `{"time":{}}`, false},
		{"the returning flag", `{"returning":false}`, true},
		{"a query condition", `{"query":{"plan":["pro"]}}`, true},
		{"a utm condition", `{"utm":{"source":["newsletter"]}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRuleConditions([]byte(tc.raw))
			if tc.ok && err != nil {
				t.Errorf("%s was refused: %v", tc.raw, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("%s was accepted", tc.raw)
			}
		})
	}
}

// The condition list travels inside the cached snapshot on the redirect path,
// so an unbounded one is an unbounded payload on the hottest read in the
// product.
func TestConditionListsAreBounded(t *testing.T) {
	many := make([]string, MaxRuleConditionValues+1)
	for i := range many {
		many[i] = "GB"
	}
	if err := ValidateRuleConditions(RuleConditions{Country: many}); err == nil {
		t.Errorf("a condition with %d values was accepted", len(many))
	}
	long := strings.Repeat("x", MaxRuleValueLength+1)
	if err := ValidateRuleConditions(RuleConditions{City: []string{long}}); err == nil {
		t.Errorf("a %d-character condition value was accepted", len(long))
	}
}

// NeedsOf is what the click recorder reads to decide whether the
// returning-visitor set has to be maintained for a link at all, so a wrong
// answer here means either a set nobody reads or a condition that never fires.
func TestNeedsOfSummarizesARuleList(t *testing.T) {
	n := NeedsOf([]RuleConditions{
		{Device: []string{"mobile"}},
		{City: []string{"Fictionbury"}},
	})
	if !n.UserAgent || !n.City || !n.Geo() {
		t.Errorf("NeedsOf missed a lookup: %+v", n)
	}
	if n.Country || n.Region || n.Returning {
		t.Errorf("NeedsOf claimed a lookup no rule asked for: %+v", n)
	}

	empty := NeedsOf(nil)
	if empty.Geo() || empty.Returning || empty.UserAgent {
		t.Errorf("a link with no rules needs nothing, got %+v", empty)
	}
}

// The condition set is stored as jsonb and travels inside the cached snapshot,
// so it has to survive a round trip through both.
func TestConditionsRoundTrip(t *testing.T) {
	in := RuleConditions{
		Country:   []string{"GB"},
		Query:     map[string][]string{"plan": {"pro"}},
		UTM:       map[string][]string{"source": {"newsletter"}},
		Time:      &RuleTime{Days: []string{"mon"}, From: "09:00", To: "17:00", TZ: "Europe/London"},
		Returning: ptr(true),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseRuleConditions(b)
	if err != nil {
		t.Fatalf("a condition set this program wrote was refused on the way back in: %v", err)
	}
	if out.Returning == nil || !*out.Returning || out.Time == nil || out.Time.TZ != "Europe/London" {
		t.Errorf("round trip lost something: %+v", out)
	}

	// Only what was set is in the bytes. Every field is omitempty because these
	// are serialized on every cache write for the link that carries them.
	if strings.Contains(string(b), "city") || strings.Contains(string(b), "browser") {
		t.Errorf("unset conditions are in the payload: %s", b)
	}
}
