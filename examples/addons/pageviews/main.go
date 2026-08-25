//go:build wasip1

// Command pageviews is the first-party sample add-on the demo instance runs.
//
// # Why it exists
//
// [M68]'s manager is a page about add-ons, and a page about add-ons on an
// instance that runs none is an empty table. D265 deferred that problem here and
// said what it was: showing an add-on on the demo means *building a wasm module
// and shipping it into the image*, which is a decision about what the demo is
// rather than a seeder change. This is that module, and the Dockerfile is where
// the decision is carried out.
//
// # What it does, and what it deliberately does not
//
// It observes redirects — `redirect.observe`, the out-of-band class, which runs
// after the visitor has already been answered and can therefore cost them
// nothing — and counts them, per day, in a table in the schema the host gives it.
// That is enough to make every column of the manager real: a declaration class
// that is not `none`, a permission list, a schema with a size, per-module latency
// on the redirect path, and settings of both kinds to render.
//
// It does not hold `redirect.inline`, does not serve pages, and does not mint
// sessions. A sample that ran inside the redirect would be a sample that could
// slow a demo down, and the point of the class split (M66) is that observing is
// the safe half.
//
// # It is an example, and it is written to be read
//
// This is the add-on somebody evaluating LinkCtrl copies. So it uses the
// published SDK and nothing else, it declares exactly the permissions it uses,
// and every host call it makes checks its error — including the ones that cannot
// fail today, because an example that ignores an error teaches that.
package main

import (
	"encoding/json"
	"strconv"

	"github.com/DevOfPie/LinkCtrl/sdk"
)

// event is the RedirectEvent record, guest-side.
//
// Written out here rather than imported, and that is a publisher's position
// rather than an inconvenience: the ABI's records are a documented JSON shape, so
// an add-on in another repository declares the fields it reads and ignores the
// rest. This one reads three of eleven.
type event struct {
	WorkspaceID  string `json:"workspace_id"`
	OccurredAt   string `json:"occurred_at"`
	IsBot        bool   `json:"is_bot"`
	ReferrerHost string `json:"referrer_host"`
}

// setting reads one of this add-on's declared settings, falling back to a literal
// when nothing is configured. `config_get` answers the manifest's default when
// there is one, so the fallback here only covers a setting whose default is empty.
func setting(key, fallback string) string {
	v, err := sdk.ConfigGet(key)
	if err != nil || v == "" {
		return fallback
	}
	return v
}

//go:wasmexport linkctrl_redirect_observe
func observe() int32 {
	raw, err := sdk.RedirectEventRead()
	if err != nil {
		_ = sdk.Log(sdk.LevelWarn, "pageviews: no event to read: "+err.Error())
		return -1
	}
	var e event
	if err := json.Unmarshal(raw, &e); err != nil {
		_ = sdk.Log(sdk.LevelWarn, "pageviews: could not parse the event: "+err.Error())
		return -1
	}

	// The operator's answer about bots, as a declared toggle so the Add-on manager
	// can render it. Counting a crawler as a page view is a choice somebody should
	// be able to make on the page rather than by rebuilding this module.
	if e.IsBot && setting("count_bots", "false") != "true" {
		return 0
	}

	// The day, taken off the record's own timestamp rather than off the clock: the
	// event says when it happened, and an out-of-band observer runs some time
	// afterwards.
	day := e.OccurredAt
	if len(day) >= 10 {
		day = day[:10]
	}

	// One statement, parameterised. The host runs it as this add-on's own
	// confined role, inside its own schema, so there is nothing here that could
	// reach another add-on's data or this product's — which is the property the
	// example is meant to demonstrate as much as the counting is.
	//
	// The arguments travel as a JSON array, which is the ABI's shape: the guest
	// and the host share no type system, so a positional argument list is a
	// document rather than a slice of values.
	args, err := json.Marshal([]string{day, e.WorkspaceID, e.ReferrerHost})
	if err != nil {
		return -1
	}
	err = sdk.StorageExec(
		`INSERT INTO views (day, workspace_id, referrer_host, hits)
		 VALUES ($1, $2, $3, 1)
		 ON CONFLICT (day, workspace_id, referrer_host)
		 DO UPDATE SET hits = views.hits + 1`, args)
	if err != nil {
		_ = sdk.Log(sdk.LevelWarn, "pageviews: could not record a view: "+err.Error())
		return -1
	}
	return 0
}

// init creates the table this add-on writes to, once per load.
//
// **DDL from the module rather than a shipped migration**, deliberately, and it
// is the shape M67 documents: an add-on whose manifest declares `.sql` files
// cannot be installed through the API at all, because those files are not part of
// the upload. An example that could only be installed by hand would be an example
// of the route this product recommends against.
func init() {
	if err := sdk.StorageExec(`
		CREATE TABLE IF NOT EXISTS views (
			day           date   NOT NULL,
			workspace_id  uuid   NOT NULL,
			referrer_host text   NOT NULL DEFAULT '',
			hits          bigint NOT NULL DEFAULT 0,
			PRIMARY KEY (day, workspace_id, referrer_host)
		)`, nil); err != nil {
		// Logged, not fatal. A `degrade`-class add-on that cannot start leaves the
		// instance serving, which is what this one wants: a demo must not fail to
		// boot because a sample could not create a table.
		_ = sdk.Log(sdk.LevelWarn, "pageviews: could not create its table: "+err.Error())
		return
	}
	_ = sdk.Log(sdk.LevelInfo, "pageviews: ready, retaining "+
		strconv.Itoa(days())+" days")
}

// days is the `retention_days` setting, read once at load like every other
// configured value. Nothing prunes yet — a sample that swept its own table would
// need a schedule the ABI does not offer — so it is read to be rendered and
// logged, which is honest about what a declared setting costs.
func days() int {
	n, err := strconv.Atoi(setting("retention_days", "30"))
	if err != nil || n <= 0 {
		return 30
	}
	return n
}

// Required by the toolchain even for a reactor module: with -buildmode=c-shared
// the entry point is _initialize, which runs package initialization and returns.
func main() {}
