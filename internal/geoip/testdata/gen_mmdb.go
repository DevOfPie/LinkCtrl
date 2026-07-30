//go:build ignore

// Command gen_mmdb writes the synthetic MaxMind DB used by the geoip tests.
//
// The fixture is committed rather than generated during the test run, so the
// writer is not a dependency of the module. MaxMind's own databases cannot be
// redistributed, and this one is not one: every network below is a documentation
// range and every country is made up.
//
// To regenerate:
//
//	go get github.com/maxmind/mmdbwriter
//	go run ./internal/geoip/testdata/gen_mmdb.go
//	go mod tidy   # drops the writer again
package main

import (
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

func main() {
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
		_, network, err := net.ParseCIDR(e.cidr)
		if err != nil {
			log.Fatal(err)
		}
		if err := w.Insert(network, e.record); err != nil {
			log.Fatal(err)
		}
	}

	out := filepath.Join("internal", "geoip", "testdata", "country-test.mmdb")
	fh, err := os.Create(out)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := fh.Close(); err != nil {
			log.Fatal(err)
		}
	}()
	if _, err := w.WriteTo(fh); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", out)
}
