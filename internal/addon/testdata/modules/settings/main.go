//go:build wasip1

// Command settings is M68's fixture: a module that reads its declared settings at
// load and says what it got.
//
// It exists because "a value an operator typed into the Add-on manager is what the
// module reads" is a claim about the *guest*, and every other way of checking it
// stops at the database. The host resolves settings once at load, so a test that
// wants the module's own answer loads a host, saves a value, and loads again —
// and this file is what makes the second load say something a test can read.
//
// **It is not `probe`, and the reason is the database.** `probe` asserts that
// storage answers `ErrInternal`, which is true of a host built without one and
// false of the host M68's tests need; loading it against a real Postgres panics
// during initialization. This fixture asserts nothing and panics never: it logs
// one line per declared setting, in a fixed shape, and leaves the judgement to
// the test.
//
// The manifest a test writes for it declares `retention_days` (text, with a
// default) and `api_token` (secret, which a manifest may not give a default), and
// it holds `config.read`. Anything else is a test asserting about a module that
// was not built for it.
//
// **It also exports the inline redirect entry point**, which is what lets one test
// drive it on a *live* host instead of reading the load-time log. That test is the
// one that holds the product's own sentence — *the add-on reads the new values on
// its next invocation* — against a module that cached a value at start-up, which
// is the case a pooled instance makes wrong.
package main

import (
	"errors"

	"github.com/DevOfPie/LinkCtrl/sdk"
)

// report writes one line a test greps for. The shape is fixed —
// `settings: <key>=<value>` — and an absent value is spelled rather than left
// empty, because an empty right-hand side and a value of "" are the two states
// this fixture exists to tell apart.
func report(key string) {
	value, err := sdk.ConfigGet(key)
	switch {
	case err == nil:
		_ = sdk.Log(sdk.LevelInfo, "settings: "+key+"="+value)
	case errors.Is(err, sdk.ErrNotFound):
		_ = sdk.Log(sdk.LevelInfo, "settings: "+key+"=<unset>")
	case errors.Is(err, sdk.ErrDenied):
		_ = sdk.Log(sdk.LevelInfo, "settings: "+key+"=<denied>")
	default:
		_ = sdk.Log(sdk.LevelInfo, "settings: "+key+"=<error>")
	}
}

// cached is what package initialization read, kept the way a real add-on keeps a
// configured value: read once, at start-up, and used from then on.
//
// It is the half of *the add-on reads the new values on its next invocation* that
// the settings holder cannot answer. `config_get` goes through a pointer the host
// swaps on a save, so an instance that asks again sees the new value immediately —
// but an instance that asked once and remembered holds the old one for as long as
// the instance lives, and M66.5's pool makes instances live. Reporting both on
// every invocation is what lets a test tell those two apart.
var cached string

func init() {
	report("retention_days")
	report("api_token")
	// A key the manifest does not declare, reported for the same reason `probe`
	// checks it: the scoping is what stops one add-on reading another's
	// configuration, and a fixture that never asked would not notice it lapsing.
	report("some_other_addons_key")
	cached, _ = sdk.ConfigGet("retention_days")
}

// The redirect entry point, exported so a test can invoke this module on a live
// host rather than only observe what it logged at load.
//
// Reached only by an add-on whose manifest declares `redirect.inline`; the grant
// is what decides, so every other test that loads this fixture is unaffected by
// the export existing. The name is the literal `linkctrl_redirect_inline` because
// a //go:wasmexport directive cannot take a constant — the same reason the
// `redirect` fixture writes it out.
//
// It writes no answer, so the redirect it is invoked on is allowed unchanged. What
// it does is report the pair: what this instance cached at initialization, and
// what `config_get` says right now.
//
//go:wasmexport linkctrl_redirect_inline
func inline() int32 {
	if cached == "" {
		_ = sdk.Log(sdk.LevelInfo, "invoked: cached=<unset>")
	} else {
		_ = sdk.Log(sdk.LevelInfo, "invoked: cached="+cached)
	}
	live, err := sdk.ConfigGet("retention_days")
	if err != nil {
		_ = sdk.Log(sdk.LevelInfo, "invoked: live=<error>")
		return 0
	}
	if live == "" {
		live = "<unset>"
	}
	_ = sdk.Log(sdk.LevelInfo, "invoked: live="+live)
	return 0
}

// Required by the toolchain even for a reactor module. See the minimal fixture.
func main() {}
