package geoip

import (
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testDB is a synthetic database built by testdata/gen_mmdb.go. See that file
// for what is in it and how to rebuild it.
func testDB(t *testing.T) *Resolver {
	t.Helper()
	r, err := Open(filepath.Join("testdata", "country-test.mmdb"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return r
}

func TestCountryLookup(t *testing.T) {
	r := testDB(t)

	cases := []struct {
		name string
		addr string
		want string
	}{
		{"ipv4", "203.0.113.5", "GB"},
		{"ipv4 other network", "198.51.100.7", "DE"},
		{"ipv6", "2001:db8::1", "JP"},

		// An IPv4-mapped IPv6 address must find the IPv4 record. Without the
		// Unmap it would miss, and a proxy handing us ::ffff: addresses would
		// silently produce no countries at all.
		{"ipv4-mapped ipv6", "::ffff:203.0.113.5", "GB"},

		// Present in the database, but the record carries no country.
		{"record without a country", "192.0.2.1", ""},

		// Present, with a code that is not two uppercase letters. Rejected rather
		// than written to a text column that a year of analytics groups by.
		{"malformed country code", "100.64.0.1", ""},

		// Absent from the database entirely.
		{"unknown address", "8.8.8.8", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.addr, err)
			}
			if got := r.Country(addr); got != tc.want {
				t.Errorf("Country(%s) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

func TestInvalidAddressResolvesToNothing(t *testing.T) {
	r := testDB(t)
	if got := r.Country(netip.Addr{}); got != "" {
		t.Errorf("Country(invalid) = %q, want empty", got)
	}
}

// The zero state is "no database", so a nil Resolver has to be as usable as a
// real one. Otherwise every call site needs to know whether GeoIP is configured.
func TestNilResolverIsUsable(t *testing.T) {
	var r *Resolver
	if got := r.Country(netip.MustParseAddr("203.0.113.5")); got != "" {
		t.Errorf("Country on nil resolver = %q, want empty", got)
	}
	if r.Enabled() {
		t.Error("Enabled() on nil resolver = true")
	}
	if r.Description() != "disabled" {
		t.Errorf("Description() = %q, want disabled", r.Description())
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close() on nil resolver = %v", err)
	}
}

func TestOpenEmptyPathDisablesResolution(t *testing.T) {
	r, err := Open("")
	if err != nil {
		t.Fatalf("Open(\"\") = %v, want no error: an unset path is the default", err)
	}
	if r.Enabled() {
		t.Error("Open(\"\") returned an enabled resolver")
	}
}

// A wrong path must fail at startup. Config validation checks the file exists;
// this is the check that it is actually a database, which is the failure that
// would otherwise show up as permanently empty countries.
func TestOpenRejectsSomethingThatIsNotADatabase(t *testing.T) {
	if _, err := Open(filepath.Join("testdata", "gen_mmdb.go")); err == nil {
		t.Fatal("Open accepted a Go source file as a MaxMind database")
	}
	if _, err := Open(filepath.Join("testdata", "does-not-exist.mmdb")); err == nil {
		t.Fatal("Open accepted a missing file")
	}
}

func TestDescriptionNamesTheDatabase(t *testing.T) {
	r := testDB(t)
	got := r.Description()
	if !strings.Contains(got, "LinkCtrl-Test-Country") {
		t.Errorf("Description() = %q, want it to name the database type", got)
	}
	if !strings.Contains(got, "built ") {
		t.Errorf("Description() = %q, want a build date", got)
	}
}

// The ingester shares one Resolver across the goroutine that writes batches
// while lookups happen; concurrency safety is a documented property of the
// reader, so this is the test that we are relying on it correctly.
func TestConcurrentLookups(t *testing.T) {
	r := testDB(t)
	addrs := []netip.Addr{
		netip.MustParseAddr("203.0.113.5"),
		netip.MustParseAddr("198.51.100.7"),
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("8.8.8.8"),
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := range 500 {
				r.Country(addrs[(i+n)%len(addrs)])
			}
		}(i)
	}
	wg.Wait()
}
