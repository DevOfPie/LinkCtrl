// Package geoip resolves a client address to a country code.
//
// It resolves a country and nothing else, which is a privacy decision rather
// than an unfinished one. The same database carries region and city, and both
// are dramatically more identifying than a country — city plus timestamp is
// close to a location history. There is nowhere in the product that shows them,
// so storing them would be collecting personal data for no purpose, which is
// exactly what the rest of the analytics design goes out of its way not to do.
// The columns exist and stay null; adding them is a Phase 2 decision that would
// need a UI and a reason.
//
// The database itself is never redistributed in the image: MaxMind's licence
// does not allow it, so geographic reporting is off unless an operator supplies
// a file. That is why every method tolerates a nil Resolver — "no database" is
// the default state, not an error.
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
