//go:build wasip1

// Command probe is the ABI's conformance fixture: it calls the host across every
// class of answer the ABI can give and states what it got.
//
// It exists because m61.md requires the declared-but-refused pattern to be
// *asserted* rather than assumed, and an assertion made only on the host side
// proves the host's own map. This module is a real consumer of the generated SDK,
// compiled the way a published add-on is compiled, so what it reports is what an
// add-on in another repository would see.
//
// Two ways it reports, deliberately:
//
//   - every check logs one `probe: <check>=<outcome>` line through the ABI's own
//     log function, which a test reads back;
//   - a mismatch **panics**, which fails instantiation and therefore fails any
//     test that loads this module at all, including one that captures no logs.
//
// The manifest this expects is written by the test that installs it: a declared
// `retention_days` text setting defaulting to 30, and a declared `api_token`
// secret, which a manifest may not give a default.
package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"github.com/DevOfPie/LinkCtrl/sdk"
)

func check(name string, ok bool, detail string) {
	outcome := "ok"
	if !ok {
		outcome = "MISMATCH: " + detail
	}
	_ = sdk.Log(sdk.LevelInfo, "probe: "+name+"="+outcome)
	if !ok {
		panic("probe: " + name + ": " + detail)
	}
}

func init() {
	// A live function that answers with a value: the host's own ABI version, which
	// must be the one this module was generated against — the module was refused
	// at load otherwise.
	version, err := sdk.HostABIVersion()
	check("abi_version", err == nil && version == sdk.ABIVersion,
		"got "+version+" wanted "+sdk.ABIVersion+" err "+errText(err))

	// A declared setting with a default. Values arrive with the Add-on manager;
	// until then the declared default is the answer.
	//
	// The value is logged rather than compared against a literal, because the test
	// installs this same module twice under two names with two different defaults —
	// which is how it proves a host function answers the *calling* add-on and not
	// whichever one happens to be first.
	value, err := sdk.ConfigGet("retention_days")
	check("config_declared", err == nil && value != "", "got "+value+" err "+errText(err))
	_ = sdk.Log(sdk.LevelInfo, "probe: retention_days="+value)

	// A declared setting with nothing behind it. A secret may not carry a default,
	// so this is the shape every secret has before somebody sets one.
	_, err = sdk.ConfigGet("api_token")
	check("config_empty", errors.Is(err, sdk.ErrNotFound), "err "+errText(err))

	// A key this add-on's manifest does not declare. Denied, not missing: the
	// manifest is what scopes the function, so there is no key outside it to be
	// absent.
	_, err = sdk.ConfigGet("some_other_addons_key")
	check("config_undeclared", errors.Is(err, sdk.ErrDenied), "err "+errText(err))

	// Storage is implemented (M63) and this fixture is loaded by a host that was
	// constructed **without a database**, which is what every test in
	// internal/addon has. So the answer is neither the refusal a declared-but-
	// unimplemented function gives nor a result: it is ErrInternal, the status that
	// means the host failed at something that is not the module's fault. The
	// confined, working form is asserted against a real Postgres by the `storage`
	// fixture and test/integration.
	//
	// It is checked here rather than dropped because the alternative is a fixture
	// that calls every ABI function except two, and the two it skipped are the two
	// that changed.
	_, err = sdk.StorageQuery("select 1", nil)
	check("storage_query_no_database", errors.Is(err, sdk.ErrInternal), "err "+errText(err))
	err = sdk.StorageExec("select 1", nil)
	check("storage_exec_no_database", errors.Is(err, sdk.ErrInternal), "err "+errText(err))

	// The three functions M64 implemented, called from *outside* a request — which
	// is where package initialization always is, because an instance is made per
	// request and the request is attached to it after this runs. So the answer is
	// neither a refusal nor a record: it is ErrNotFound, and that is the honest
	// answer to "read the request" when there is not one.
	//
	// They are checked here rather than only in the routing tests because this
	// fixture's job is to call every function in the ABI: a limb that landed and
	// left this file alone would be a limb nothing probes from the guest side.
	_, err = sdk.HTTPRequestRead()
	check("http_request_outside_request", errors.Is(err, sdk.ErrNotFound), "err "+errText(err))
	err = sdk.HTTPResponseWrite(nil)
	check("http_response_outside_request", errors.Is(err, sdk.ErrNotFound), "err "+errText(err))
	_, err = sdk.SessionContextRead()
	check("session_context_outside_request", errors.Is(err, sdk.ErrNotFound), "err "+errText(err))

	// D292's two functions, called from the **load-time** instance — the other of
	// the two places this host builds a module config, and the one a fix applied
	// only to the request path would miss.
	//
	// The comparison this module *can* make is the one that mattered: wazero's
	// default random source is a compile-time constant, so before D292 a draw here
	// was byte-identical to a draw anywhere else, and its default clock starts at
	// 2022-01-01. So a 32-byte draw of all zeroes is impossible either way and is
	// not what is checked; what is checked is that the clock is past the fake one's
	// origin, which no seeded stream and no frozen clock can fake.
	drawn, err := sdk.RandomBytes(32)
	check("random_bytes", err == nil && len(drawn) == 32, "err "+errText(err))
	std := make([]byte, 32)
	_, err = rand.Read(std)
	check("crypto_rand", err == nil, "err "+errText(err))
	check("random_differs_from_stdlib", !bytes.Equal(drawn, std),
		"the ABI and crypto/rand returned the same 32 bytes, which one stream cannot do twice")
	stamp, err := sdk.TimeNow()
	check("time_now", err == nil, "err "+errText(err))
	at, perr := time.Parse(time.RFC3339Nano, stamp)
	check("time_now_parses", perr == nil, "got "+stamp+" err "+errText(perr))
	// 2023 rather than "near now": a fixture that asserts a tight window against
	// the host's clock is a fixture that fails when a machine's clock is wrong,
	// and the fake clock this is about begins at 2022-01-01T00:00:00Z.
	check("time_now_is_not_the_fake_clock", perr == nil && at.Year() >= 2023,
		"got "+stamp)
	// **UTC, checked as the spelling and not as the instant.** The ABI's published
	// promise is *one spelling to parse and no zone to guess*, and every check
	// around this one passes on `2026-08-22T12:00:00+02:00`: it parses, its year is
	// right, and it names the same instant the standard library does. A `Z` is the
	// only thing that distinguishes the promise from a host that happened to format
	// in local time, so it is what is asserted.
	check("time_now_is_utc", strings.HasSuffix(stamp, "Z"), "got "+stamp)
	// The standard library's clock is the same source, and it is the one a
	// publisher writes. Compared against the ABI's answer rather than against a
	// literal, so the check is "these two agree" and not "this machine is in 2026".
	check("std_clock_agrees", time.Since(at) < time.Minute && time.Since(at) > -time.Minute,
		"time.Now is "+time.Now().UTC().Format(time.RFC3339Nano)+" and the host says "+stamp)
	// Named `_invalid` rather than `_refused`: the host-side test counts checks
	// whose name ends in `_refused` against the ABI's set of declared-but-refused
	// functions, and random_bytes is live. A bound it enforces is not a limb it
	// does not have.
	_, err = sdk.RandomBytes(0)
	check("random_zero_invalid", errors.Is(err, sdk.ErrInvalid), "err "+errText(err))
	_, err = sdk.RandomBytes(4097)
	check("random_over_bound_invalid", errors.Is(err, sdk.ErrInvalid), "err "+errText(err))

	// session_mint went live at M65, and this instance is not answering a request —
	// package initialization never is. So the answer is ErrNotFound, which is the
	// same "there is no request here" every M64 function gives from init, and the
	// point of checking it is that a mint reached from outside a request is a mint
	// with no visitor to sign in.
	_, err = sdk.SessionMint([]byte(`{"subject":"s","issuer":"https://idp.test"}`))
	check("session_mint_outside_request", errors.Is(err, sdk.ErrNotFound), "err "+errText(err))
	// Its mirror, and the same answer for the same reason: linking is something
	// that happens to the person in front of the browser, and package
	// initialization has no browser in front of it.
	err = sdk.IdentityLink([]byte(`{"subject":"s","issuer":"https://idp.test"}`))
	check("identity_link_outside_request", errors.Is(err, sdk.ErrNotFound), "err "+errText(err))

	// The ones this fixture exists for. Each is declared by the ABI and implemented
	// by no host yet, so it resolves as an import — the module links — and answers a
	// status the module can branch on. That the refusal is the whole set's property
	// and not one function's is why every one of them is here.
	_, err = sdk.TemplateRender("page", nil)
	check("template_refused", errors.Is(err, sdk.ErrNotAvailable), "err "+errText(err))
	_, err = sdk.RedirectEventRead()
	check("redirect_refused", errors.Is(err, sdk.ErrNotAvailable), "err "+errText(err))

	// The guest's own fault, answered as such rather than defaulted. A level
	// nobody spelled correctly would otherwise become a line nobody greps for.
	err = sdk.Log("shout", "this level does not exist")
	check("bad_level", errors.Is(err, sdk.ErrInvalid), "err "+errText(err))
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func main() {}
