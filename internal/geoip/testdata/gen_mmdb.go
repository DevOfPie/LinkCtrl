//go:build ignore

// Command gen_mmdb writes the synthetic MaxMind DBs used by the geoip tests.
//
// Two files. `country-test.mmdb` is the small correctness fixture the Country
// reader has always been tested against. `city-test.mmdb` is M34's addition and
// exists for two jobs at once: it is what Region and City are tested against,
// and it is what their lookup *cost* is measured against — see D48 and
// docs/slo.md.
//
// The fixtures are committed rather than generated during the test run, so the
// writer is not a dependency of the module. MaxMind's own databases cannot be
// redistributed, and neither of these is one: **every network below is a
// documentation, reserved or private range, and every place name is made up.**
// That property is the reason `IncludeReservedNetworks` is set — the writer
// refuses those ranges by default, and using them is the point. A fixture built
// on real allocations would be a claim about somewhere that exists, and a
// measurement taken against it would look like a measurement of the real world.
//
// The city fixture is deliberately large. A handful of records would make the
// lookup a two-node walk and produce a timing that says nothing about a real
// database, so the bulk entries below scatter tens of thousands of networks
// across reserved space at IPv4 /24 and /32 and IPv6 /48 depth. What that still
// does not reproduce is GeoLite2-City's *size*: the real file is ~60MB and does
// not fit in a CPU cache, so the figure this fixture produces is a
// representative floor rather than a reading of the database an operator
// deploys. That caveat is carried into every place the number is written.
//
// To regenerate:
//
//	go get github.com/maxmind/mmdbwriter
//	go run ./internal/geoip/testdata/gen_mmdb.go
//	go mod tidy   # drops the writer again
//
// One run writes **both** files, and each carries a build timestamp in its
// metadata — so a regeneration always shows up as a diff even when no record
// changed. Check the size and the node count before committing a churned
// fixture: if only three bytes moved they are the epoch, and the file did not
// need to be in the commit at all.
package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

func main() {
	writeCountry()
	writeCity()
}

// --- the country fixture -----------------------------------------------------

func writeCountry() {
	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "LinkCtrl-Test-Country",
		RecordSize:   24,
		Languages:    []string{"en"},
		// Every network below is a documentation or shared-address range, all of
		// which the writer treats as reserved and refuses by default. Using them
		// is the point: a fixture built on real allocations would be a claim
		// about somewhere that exists.
		IncludeReservedNetworks: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	country := func(iso string) mmdbtype.Map {
		return mmdbtype.Map{
			"country": mmdbtype.Map{"iso_code": mmdbtype.String(iso)},
		}
	}

	entries := []struct {
		cidr   string
		record mmdbtype.DataType
	}{
		// Documentation ranges, so nothing here resembles a real allocation.
		{"203.0.113.0/24", country("GB")},
		{"198.51.100.0/24", country("DE")},
		{"2001:db8::/32", country("JP")},

		// A record with location data but no country: the reader must answer ""
		// rather than inventing one.
		{"192.0.2.0/24", mmdbtype.Map{
			"city": mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String("Nowhere")}},
		}},

		// A malformed code. Real databases do not contain these, which is exactly
		// why the reader's length check needs something to check.
		{"100.64.0.0/24", country("XYZ")},
	}

	for _, e := range entries {
		insert(w, e.cidr, e.record)
	}
	write(w, "country-test.mmdb")
}

// --- the city fixture --------------------------------------------------------

// bulkNetworks is how many scattered networks the city fixture holds in each of
// its three families.
//
// Chosen against a file size rather than against a tree statistic, because the
// file is committed: three times this many networks produces a couple of
// megabytes, which is a reasonable thing to keep in a repository, and ten times
// it is not. The exact network and node counts the run produces are printed and
// are what docs/slo.md quotes — a figure nobody can reproduce is worse than no
// figure.
const bulkNetworks = 11000

func writeCity() {
	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "LinkCtrl-Test-City",
		RecordSize:              24,
		Languages:               []string{"en"},
		IncludeReservedNetworks: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	// The named entries, which the correctness tests read. Documentation ranges
	// only, and every city name is invented.
	for _, e := range []struct {
		cidr   string
		record mmdbtype.DataType
	}{
		{"203.0.113.0/24", cityRecord("GB", "ENG", "Fictionbury")},
		{"198.51.100.0/24", cityRecord("DE", "BE", "Beispielstadt")},
		{"2001:db8::/32", cityRecord("JP", "13", "Reidoshi")},

		// A country and nothing beneath it. Region and City must answer "" here
		// rather than inheriting from somewhere.
		{"192.0.2.0/24", mmdbtype.Map{
			"country": mmdbtype.Map{"iso_code": mmdbtype.String("FR")},
		}},

		// Contents no real database has, which is exactly why the readers'
		// checks need something to check: a subdivision code too long to be one,
		// and a city name carrying a control character.
		{"198.18.0.0/24", mmdbtype.Map{
			"country":      mmdbtype.Map{"iso_code": mmdbtype.String("NL")},
			"subdivisions": mmdbtype.Slice{mmdbtype.Map{"iso_code": mmdbtype.String("TOOLONG")}},
			"city":         mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String("Bad\x07Name")}},
		}},
	} {
		insert(w, e.cidr, e.record)
	}

	// The bulk, which the cost measurement reads.
	//
	// Three families so the walk reaches three different depths: IPv4 /32 is the
	// deepest an address can go, IPv4 /24 is where most of a real database's
	// entries sit, and IPv6 /48 is deeper still. Scattered with a deterministic
	// generator rather than laid out consecutively — a contiguous block collapses
	// into a handful of nodes and would make the tree shallow in exactly the way
	// this fixture exists to avoid.
	//
	// 240.0.0.0/4 is reserved for future use, 100.64.0.0/10 is carrier-grade NAT
	// and fc00::/7 is unique-local. None of the three is allocated to anybody, so
	// none of the records below can be read as a claim about a real place.
	var rng uint64 = 0x5eed_1cec_0ffe_e123
	next := func() uint64 {
		// splitmix64: deterministic, seedless in effect, and small enough to read.
		rng += 0x9e3779b97f4a7c15
		z := rng
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		return z ^ (z >> 31)
	}

	for i := range bulkNetworks {
		// 240.0.0.0/4, single addresses: the full 32-bit walk.
		v4 := uint32(0xF000_0000) | uint32(next()&0x0FFF_FFFF) //nolint:gosec
		insertIP4(w, v4, 32, bulkRecord(i))
	}
	for i := range bulkNetworks {
		// 100.64.0.0/10, /24 networks: the depth most of a real database sits at.
		v4 := uint32(0x6440_0000) | uint32(next()&0x003F_FF00) //nolint:gosec
		insertIP4(w, v4, 24, bulkRecord(i))
	}
	for i := range bulkNetworks {
		// fc00::/7, /48 networks.
		ip := make(net.IP, 16)
		ip[0] = 0xFC
		binary.BigEndian.PutUint64(ip[1:9], next())
		// Zero everything past the /48 so the CIDR is canonical.
		for b := 6; b < 16; b++ {
			ip[b] = 0
		}
		insertNet(w, &net.IPNet{IP: ip, Mask: net.CIDRMask(48, 128)}, bulkRecord(i))
	}

	fmt.Printf("city fixture: %d bulk networks in three families\n", 3*bulkNetworks)
	write(w, "city-test.mmdb")
}

// bulkCities is how many distinct records the bulk networks share between them.
//
// Shared rather than unique so the data section stays small and the *tree* is
// what the file is mostly made of — which is what a lookup actually walks. A
// unique record per network would inflate the file without deepening anything.
const bulkCities = 256

func bulkRecord(i int) mmdbtype.DataType {
	// Names built from an index, so no entry resembles a place. The country
	// codes are the ISO user-assigned range (AA, QM–QZ, XA–XZ, ZZ), which is
	// permanently unassigned.
	iso := [...]string{"AA", "QM", "QN", "XA", "XB", "XC", "ZZ"}[i%7]
	return cityRecord(iso,
		fmt.Sprintf("R%02d", i%bulkCities%100),
		fmt.Sprintf("Testville-%03d", i%bulkCities))
}

func cityRecord(iso, subdivision, city string) mmdbtype.Map {
	return mmdbtype.Map{
		"country": mmdbtype.Map{"iso_code": mmdbtype.String(iso)},
		"subdivisions": mmdbtype.Slice{
			mmdbtype.Map{"iso_code": mmdbtype.String(subdivision)},
		},
		"city": mmdbtype.Map{
			"names": mmdbtype.Map{"en": mmdbtype.String(city)},
		},
	}
}

// --- plumbing ----------------------------------------------------------------

func insert(w *mmdbwriter.Tree, cidr string, record mmdbtype.DataType) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		log.Fatal(err)
	}
	insertNet(w, network, record)
}

func insertIP4(w *mmdbwriter.Tree, addr uint32, bits int, record mmdbtype.DataType) {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, addr)
	insertNet(w, &net.IPNet{IP: ip.Mask(net.CIDRMask(bits, 32)), Mask: net.CIDRMask(bits, 32)}, record)
}

func insertNet(w *mmdbwriter.Tree, network *net.IPNet, record mmdbtype.DataType) {
	if err := w.Insert(network, record); err != nil {
		log.Fatal(err)
	}
}

func write(w *mmdbwriter.Tree, name string) {
	out := filepath.Join("internal", "geoip", "testdata", name)
	fh, err := os.Create(out)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := fh.Close(); err != nil {
			log.Fatal(err)
		}
	}()
	n, err := w.WriteTo(fh)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%d bytes)", out, n)
}
