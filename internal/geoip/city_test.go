package geoip

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cityDB is the synthetic City database built by testdata/gen_mmdb.go.
//
// Synthetic, and that word carries a decision rather than an apology (D48).
// There is no GeoLite2-City on this project's machines and one cannot be
// committed — it is ~60MB and MaxMind's to licence — so both the correctness
// tests below and the cost measurement read a database this repository builds
// out of documentation, reserved and private ranges. What that buys is a
// fixture nobody can mistake for a claim about a real place, and what it costs
// is stated wherever the number appears.
func cityDB(t testing.TB) *Resolver {
	t.Helper()
	r, err := Open(filepath.Join("testdata", "city-test.mmdb"))
	if err != nil {
		t.Fatalf("open city test database: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return r
}

func TestRegionAndCityLookup(t *testing.T) {
	r := cityDB(t)

	cases := []struct {
		name             string
		addr             string
		country          string
		region, cityName string
	}{
		{"ipv4", "203.0.113.5", "GB", "ENG", "Fictionbury"},
		{"ipv4 other network", "198.51.100.7", "DE", "BE", "Beispielstadt"},
		{"ipv6", "2001:db8::1", "JP", "13", "Reidoshi"},

		// The same Unmap the country reader needs. A proxy handing us ::ffff:
		// addresses must not silently stop matching city rules.
		{"ipv4-mapped ipv6", "::ffff:203.0.113.5", "GB", "ENG", "Fictionbury"},

		// A country with nothing beneath it. Neither value may be inherited from
		// somewhere else in the tree.
		{"country only", "192.0.2.1", "FR", "", ""},

		// Contents no real database has. A subdivision code too long to be one,
		// and a city name carrying a control character: both rejected rather than
		// handed to a rule to match on.
		{"malformed record", "198.18.0.1", "NL", "", ""},

		// Absent from the database entirely.
		{"unknown address", "8.8.8.8", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.addr)
			if got := r.Country(addr); got != tc.country {
				t.Errorf("Country(%s) = %q, want %q", tc.addr, got, tc.country)
			}
			if got := r.Region(addr); got != tc.region {
				t.Errorf("Region(%s) = %q, want %q", tc.addr, got, tc.region)
			}
			if got := r.City(addr); got != tc.cityName {
				t.Errorf("City(%s) = %q, want %q", tc.addr, got, tc.cityName)
			}
		})
	}
}

// A Country database carries no subdivisions and no city names, and an operator
// who mounted one and then wrote a city rule must get "no answer" rather than a
// crash or a wrong one.
func TestCountryDatabaseAnswersNoRegionOrCity(t *testing.T) {
	r := testDB(t)
	addr := netip.MustParseAddr("203.0.113.5")
	if got := r.Country(addr); got != "GB" {
		t.Fatalf("Country = %q, want GB: the fixture is not what this test assumes", got)
	}
	if got := r.Region(addr); got != "" {
		t.Errorf("Region against a Country database = %q, want empty", got)
	}
	if got := r.City(addr); got != "" {
		t.Errorf("City against a Country database = %q, want empty", got)
	}
}

// "No database" is the default state on every instance that has not supplied
// one, so the nil resolver has to answer these two as readily as Country.
func TestNilResolverResolvesNoRegionOrCity(t *testing.T) {
	var r *Resolver
	addr := netip.MustParseAddr("203.0.113.5")
	if got := r.Region(addr); got != "" {
		t.Errorf("Region on nil resolver = %q, want empty", got)
	}
	if got := r.City(addr); got != "" {
		t.Errorf("City on nil resolver = %q, want empty", got)
	}
	if got := r.Region(netip.Addr{}); got != "" {
		t.Errorf("Region(invalid) = %q, want empty", got)
	}
	if got := r.City(netip.Addr{}); got != "" {
		t.Errorf("City(invalid) = %q, want empty", got)
	}
}

// TestCityLookupCostFitsTheRedirectBudget is the measurement m34.md asked for
// rather than an assumption, and it is the reason the fixture above is large.
//
// **What it measures, exactly.** The wall clock of one City lookup — an mmap
// walk of the search tree followed by decoding one string out of the record —
// against `city-test.mmdb`, whose contents and size are printed by the test so
// that the number can never be read without them. It is asserted against a
// small fraction of the 20ms redirect budget rather than against a tight bound,
// because the property that matters is "this is not a millisecond-scale
// operation" and a tight bound here would be a measurement of whichever machine
// ran the tests.
//
// **What it does not measure**, and this is the residue D48 refused to pretend
// away: the cost against the GeoLite2-City database an operator actually
// deploys. That file is roughly twenty times this one and does not fit in a CPU
// cache, so a walk of it will fault more often. The figure below is a
// representative floor.
func TestCityLookupCostFitsTheRedirectBudget(t *testing.T) {
	r := cityDB(t)

	info, err := os.Stat(filepath.Join("testdata", "city-test.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	nodes := r.db.Metadata.NodeCount
	t.Logf("database: %s, %d bytes, %d nodes", r.Description(), info.Size(), nodes)

	// Addresses spread across all three bulk families and both address widths,
	// so the timing is not one cached path walked ten thousand times.
	addrs := benchAddrs()

	// Warm the mapping, so the figure is a lookup rather than the first page
	// faults of a file that was just opened.
	for _, a := range addrs {
		r.City(a)
		r.Region(a)
	}

	const iterations = 20000
	start := time.Now()
	for i := range iterations {
		r.City(addrs[i%len(addrs)])
	}
	per := time.Since(start) / iterations

	start = time.Now()
	for i := range iterations {
		r.Region(addrs[i%len(addrs)])
	}
	perRegion := time.Since(start) / iterations

	t.Logf("City():   %s per lookup over %d lookups", per, iterations)
	t.Logf("Region(): %s per lookup over %d lookups", perRegion, iterations)

	// One twentieth of the whole redirect budget, for a lookup that happens at
	// most once per request and only for links whose rules ask for a city. A
	// failure here is not "the machine was busy" — it is three orders of
	// magnitude away from what was measured, which means the lookup stopped
	// being an mmap walk.
	const budget = time.Millisecond
	if per > budget {
		t.Errorf("City() took %s per lookup, over the %s this test holds it to; "+
			"the redirect budget is 20ms in total and this is one step of it", per, budget)
	}
	if perRegion > budget {
		t.Errorf("Region() took %s per lookup, over the %s this test holds it to",
			perRegion, budget)
	}
}

func BenchmarkCityLookup(b *testing.B) {
	r := cityDB(b)
	addrs := benchAddrs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		r.City(addrs[i%len(addrs)])
	}
}

func BenchmarkRegionLookup(b *testing.B) {
	r := cityDB(b)
	addrs := benchAddrs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		r.Region(addrs[i%len(addrs)])
	}
}

// benchAddrs spreads lookups over the fixture's three bulk families — reserved
// IPv4 at /32 depth, carrier-grade NAT at /24, unique-local IPv6 at /48 — plus
// the named documentation networks and one address that is in none of them, so
// a miss is timed too.
func benchAddrs() []netip.Addr {
	out := make([]netip.Addr, 0, 512)
	for i := range 128 {
		out = append(out,
			netip.AddrFrom4([4]byte{0xF0 + byte(i%16), byte(i * 7), byte(i * 13), byte(i)}),
			netip.AddrFrom4([4]byte{100, 64 + byte(i%64), byte(i * 3), 1}),
			netip.AddrFrom16([16]byte{0xFC, byte(i), byte(i * 5), byte(i * 11), byte(i), byte(i * 3)}),
		)
	}
	out = append(out,
		netip.MustParseAddr("203.0.113.5"),
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("8.8.8.8"),
	)
	return out
}
