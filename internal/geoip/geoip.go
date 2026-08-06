// Package geoip resolves a client address to a country, region or city.
//
// **It resolves all three and stores only the country.** That is the whole
// shape of the decision, and it changed in M34 in one direction only: routing
// rules can ask about a region or a city, so those values are now *resolvable*
// on the redirect path — and `click_events.region` and `click_events.city`
// remain null, asserted by test rather than promised here. Region and city are
// dramatically more identifying than a country; city plus timestamp is close to
// a location history, and a column holding one is a column somebody eventually
// reports on. A value that exists for the microseconds it takes to decide a
// redirect is not the same thing as a value in a row.
//
// Region and City are therefore called on the redirect path only when a link's
// rules need them — see domain.RuleNeeds — and never by the click ingester,
// which asks for a country and nothing else.
//
// The database itself is never redistributed in the image: MaxMind's licence
// does not allow it, so geographic reporting is off unless an operator supplies
// a file. That is why every method tolerates a nil Resolver — "no database" is
// the default state, not an error. Region and city additionally need a *City*
// database: a Country database carries neither, and asking it for one returns
// the same empty answer as having no database at all.
package geoip

import (
	"fmt"
	"net/netip"

	maxminddb "github.com/oschwald/maxminddb-golang/v2"
)

// Resolver looks up countries in a MaxMind DB file.
//
// Lookups are safe to call concurrently, which is what lets the ingester share
// one Resolver across a batch without copying or locking.
type Resolver struct {
	db *maxminddb.Reader
}

// Open loads a MaxMind DB file. An empty path returns a nil Resolver, which is
// valid and resolves nothing.
//
// The file is validated by opening it rather than by trusting the path: a
// truncated or wrong-format database fails here, at startup, instead of
// returning empty countries for the life of the process.
func Open(path string) (*Resolver, error) {
	if path == "" {
		return nil, nil
	}
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("geoip: open %q: %w", path, err)
	}
	return &Resolver{db: db}, nil
}

// Country returns the ISO 3166-1 alpha-2 code for an address, or "" when it is
// unknown.
//
// Every failure — no database, unroutable address, address absent from the
// database, a record without a country — is the same empty answer. A caller has
// nothing different to do about them, and an error return would put a branch on
// the path that enriches every click.
func (r *Resolver) Country(addr netip.Addr) string {
	if r == nil || r.db == nil || !addr.IsValid() {
		return ""
	}

	// DecodePath rather than decoding a struct: it reads the one field out of the
	// record instead of walking the whole thing, and a City database record is
	// large.
	var iso string
	res := r.db.Lookup(addr.Unmap())
	if !res.Found() {
		return ""
	}
	if err := res.DecodePath(&iso, "country", "iso_code"); err != nil {
		return ""
	}

	// Length-checked before it reaches a text column. A database with unexpected
	// contents should produce no country, not an arbitrary string that then has
	// to be cleaned out of a year of analytics.
	if len(iso) != 2 {
		return ""
	}
	for i := range iso {
		if iso[i] < 'A' || iso[i] > 'Z' {
			return ""
		}
	}
	return iso
}

// Region returns the ISO 3166-2 subdivision code for an address — "ENG",
// "CA" — or "" when it is unknown (M34).
//
// The most specific subdivision available is *not* what this returns. MaxMind
// stores subdivisions outermost-first, and the first entry is the one whose
// vocabulary is stable across countries and across database releases; the
// deeper entries are a district in some countries, a county in others, and
// absent in most. A routing rule is written once and evaluated for a year, so
// the value it matches on has to be the one that does not move.
//
// Same contract as Country: every failure is the same empty answer, because a
// caller on the redirect path has nothing different to do about them.
func (r *Resolver) Region(addr netip.Addr) string {
	if r == nil || r.db == nil || !addr.IsValid() {
		return ""
	}
	res := r.db.Lookup(addr.Unmap())
	if !res.Found() {
		return ""
	}
	var code string
	if err := res.DecodePath(&code, "subdivisions", 0, "iso_code"); err != nil {
		return ""
	}
	// A subdivision code is letters and digits, up to three characters in the
	// standard. Length- and charset-checked for the same reason Country is: a
	// database with unexpected contents should produce no region rather than an
	// arbitrary string that a rule then silently matches on.
	if code == "" || len(code) > 3 {
		return ""
	}
	for i := range code {
		c := code[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return code
}

// City returns the English city name for an address, or "" when it is unknown
// (M34).
//
// English rather than the visitor's language, and that is a matching decision
// rather than a display one: the value is compared against a name somebody
// typed into a rule, so it has to be the same name every time regardless of who
// is visiting. Nothing renders it.
//
// This is the lookup the milestone required a *measurement* for rather than an
// assumption — see docs/slo.md and D48. It is one mmap walk of the same tree
// Country reads, followed by decoding one string out of the record; DecodePath
// is what keeps that from being a decode of the whole City record, which is
// large.
func (r *Resolver) City(addr netip.Addr) string {
	if r == nil || r.db == nil || !addr.IsValid() {
		return ""
	}
	res := r.db.Lookup(addr.Unmap())
	if !res.Found() {
		return ""
	}
	var name string
	if err := res.DecodePath(&name, "city", "names", "en"); err != nil {
		return ""
	}
	// Bounded and control-character-free before it is compared against anything.
	// Nothing stores this, but it does reach a log line on the refusal path and
	// an unbounded string from a file an operator mounted is not a value to pass
	// around unexamined.
	if name == "" || len(name) > 128 {
		return ""
	}
	for _, ch := range name {
		if ch < 0x20 || ch == 0x7f {
			return ""
		}
	}
	return name
}

// Enabled reports whether a database is loaded.
func (r *Resolver) Enabled() bool { return r != nil && r.db != nil }

// Description returns the database's own type and build date, for the startup
// log. An operator who mounted the wrong file should be able to see that from
// the log rather than from empty charts.
func (r *Resolver) Description() string {
	if !r.Enabled() {
		return "disabled"
	}
	m := r.db.Metadata
	return fmt.Sprintf("%s built %s (%d nodes)",
		m.DatabaseType, m.BuildTime().UTC().Format("2006-01-02"), m.NodeCount)
}

// Close releases the mapped file.
func (r *Resolver) Close() error {
	if !r.Enabled() {
		return nil
	}
	return r.db.Close()
}
