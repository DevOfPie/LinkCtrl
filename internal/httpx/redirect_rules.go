package httpx

import (
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

// Routing-rule evaluation on the redirect path (M34).
//
// The rule that shapes this file is the one m34.md states as a bullet:
// **rule evaluation adds no database query per request.** Everything a
// condition can ask about comes from three places and no fourth — the request
// itself, the cached snapshot, and, for the returning-visitor condition alone,
// one Redis set-membership test. There is no lookup of the rules, no lookup of
// their destinations, and no lookup of the link.
//
// The second rule is the one the SLO cares about: a link with no rules must
// cost what it cost yesterday. Snapshot.Route returns on a length check before
// touching anything, and nothing below it is constructed for such a link — the
// subject is a stack value with no lookups performed, and the geo and Redis
// dependencies are never consulted.

// GeoResolver is what routing rules need from a MaxMind database.
//
// Three methods rather than the geoip package's whole Resolver, so the redirect
// tree does not depend on how geography is looked up and so a test can route on
// a city without a database on disk. It is deliberately a *wider* interface than
// analytics.CountryResolver: the click pipeline may ask for a country and
// nothing else, this path may ask for all three, and the difference between the
// two interfaces is where "region and city are resolvable and never stored"
// lives.
type GeoResolver interface {
	Country(netip.Addr) string
	Region(netip.Addr) string
	City(netip.Addr) string
}

// ruleSubject answers a rule's questions about one request.
//
// Every expensive answer is resolved at most once and remembered, because a
// link with several rules asking about the same country must not walk the mmap
// tree several times — and because the returning-visitor test is a network
// round trip that must happen once or not at all.
//
// Nothing is resolved eagerly. domain.Match asks only about conditions a rule
// actually sets, so a link whose rules never name a city never resolves one —
// even on an instance with a City database mounted and other links routing on
// it. That is the same property RuleNeeds reports statically, arrived at here by
// simply not asking.
type ruleSubject struct {
	req  *http.Request
	snap *redirect.Snapshot
	now  time.Time

	geo       GeoResolver
	returning *analytics.ReturningSet

	addr netip.Addr

	// Each answer plus whether it has been worked out yet. A bool beside the
	// value rather than a pointer, because "" is a legitimate answer — no
	// database, an address in nobody's range — and re-resolving it on every rule
	// would be the exact cost this memoization exists to avoid.
	country, region, city             string
	haveCountry, haveRegion, haveCity bool

	browser, os, device string
	haveUA              bool

	language, referrer string
	haveLanguage       bool
	haveReferrer       bool

	query    url.Values
	haveQ    bool
	seen     bool
	haveSeen bool
}

func newRuleSubject(
	r *http.Request, snap *redirect.Snapshot, now time.Time,
	geo GeoResolver, returning *analytics.ReturningSet,
) *ruleSubject {
	return &ruleSubject{
		req: r, snap: snap, now: now,
		geo: geo, returning: returning,
		addr: ClientIPFrom(r.Context()),
	}
}

func (s *ruleSubject) Now() time.Time { return s.now }

func (s *ruleSubject) Country() string {
	if !s.haveCountry {
		s.haveCountry = true
		if s.geo != nil {
			s.country = s.geo.Country(s.addr)
		}
	}
	return s.country
}

func (s *ruleSubject) Region() string {
	if !s.haveRegion {
		s.haveRegion = true
		if s.geo != nil {
			s.region = s.geo.Region(s.addr)
		}
	}
	return s.region
}

func (s *ruleSubject) City() string {
	if !s.haveCity {
		s.haveCity = true
		if s.geo != nil {
			s.city = s.geo.City(s.addr)
		}
	}
	return s.city
}

// classify runs the user-agent classifier once.
//
// analytics.Classify, called rather than copied, for the reason M32.5's bot
// gate calls it: a second classifier here would drift from the one the click
// recorder uses, and the analytics would then insist the traffic was a browser
// while the rules had routed it as a bot.
func (s *ruleSubject) classify() {
	if s.haveUA {
		return
	}
	s.haveUA = true
	cls := analytics.Classify(s.req.UserAgent())
	s.browser, s.os, s.device = cls.Browser, cls.OS, string(cls.Device)
}

func (s *ruleSubject) Browser() string { s.classify(); return s.browser }
func (s *ruleSubject) OS() string      { s.classify(); return s.os }
func (s *ruleSubject) Device() string  { s.classify(); return s.device }

// Language is the same first-subtag reading the click recorder stores, so a
// rule matching "en" matches exactly the clicks the analytics page shows under
// "en".
func (s *ruleSubject) Language() string {
	if !s.haveLanguage {
		s.haveLanguage = true
		s.language = analytics.PrimaryLanguage(s.req.Header.Get("Accept-Language"))
	}
	return s.language
}

// ReferrerHost is the host and nothing else, for the same reason the click
// recorder keeps only the host: a full referrer routinely carries session
// tokens and search terms, and a rule that matched on one would put them in the
// database that stores the rule.
func (s *ruleSubject) ReferrerHost() string {
	if !s.haveReferrer {
		s.haveReferrer = true
		s.referrer = analytics.ReferrerHost(s.req.Referer())
	}
	return s.referrer
}

func (s *ruleSubject) QueryParam(name string) []string {
	if !s.haveQ {
		s.haveQ = true
		s.query = s.req.URL.Query()
	}
	return s.query[name]
}

// Returning asks Redis whether this visitor was seen on this link earlier
// today.
//
// One round trip per request at most, and only for a request that has already
// satisfied every other condition on some rule — domain.Match evaluates this
// last for exactly that reason. False whenever there is nothing to ask: no
// Redis, no salt in memory, a failed or timed-out command. See
// analytics.ReturningSet for why each of those is the honest answer rather than
// a silent degradation.
func (s *ruleSubject) Returning() bool {
	if !s.haveSeen {
		s.haveSeen = true
		if s.returning.Enabled() && s.snap != nil {
			s.seen = s.returning.Seen(s.req.Context(),
				s.snap.LinkID, s.snap.WorkspaceID,
				s.addr, s.req.UserAgent(), s.now)
		}
	}
	return s.seen
}

// route decides which destination a request gets, and reports whether a rule
// decided it.
//
// Returns early — before allocating a subject, before touching geo or Redis —
// for a snapshot with no rules, which is every link on a default instance. That
// is the "links without rules resolve through the unchanged fast path" bullet,
// and it is structural: there is no branch below this one for such a link to
// take.
func (h *RedirectHandler) route(
	r *http.Request, snap *redirect.Snapshot, now time.Time,
) (redirect.Choice, bool) {
	if snap == nil || len(snap.Rules) == 0 {
		return redirect.Choice{}, false
	}
	subject := newRuleSubject(r, snap, now, h.Geo, h.Returning)
	return snap.Route(subject)
}

// tracksReturning reports whether a click on this link should be remembered in
// the within-day set.
//
// Read off the link's own rules rather than from a column, so switching the
// last returning-visitor rule off stops the set being maintained on the next
// cache refresh without anything else having to notice.
func tracksReturning(snap *redirect.Snapshot) bool {
	return snap.RuleNeeds().Returning
}

// ensure the subject really satisfies the interface the domain package
// evaluates against; a missing method would otherwise only show up where it is
// passed.
var _ domain.RuleSubject = (*ruleSubject)(nil)
