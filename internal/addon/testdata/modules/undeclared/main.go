//go:build wasip1

// Command undeclared is the permission model's conformance fixture: it declares
// no permissions and then calls every function that costs one.
//
// It is the test m62.md names — "asserted by a test module that requests what it
// did not declare" — and it is a module rather than a host-side unit test for the
// reason probe is: an assertion made only on the host side proves the host's own
// map, while this is a real consumer of the generated SDK, compiled the way a
// published add-on is compiled, so what it reports is what a badly written add-on
// in another repository would actually see.
//
// **Every gated call must answer ErrDenied, including the ones no host implements
// yet.** That ordering is the point rather than an accident: a module that
// declared nothing must not be able to use the ABI's availability status to
// enumerate which limbs a host has. So `storage_query` here is Denied, while the
// same call from probe — whose manifest declares the grant — is NotAvailable.
//
// It reports the same two ways probe does: one `undeclared: <check>=<outcome>`
// line per check through the ABI's own log function, and a **panic** on a
// mismatch, which fails instantiation and therefore fails any test that loads
// this module at all.
//
// The manifest the test installs gives it a `retention_days` setting and no
// permissions. The setting is deliberate: a declared key that is still refused is
// what separates the two questions config_get answers — may this add-on read
// settings at all, and is this key one of its own.
package main

import (
	"errors"

	"github.com/DevOfPie/LinkCtrl/sdk"
)

func check(name string, ok bool, detail string) {
	outcome := "ok"
	if !ok {
		outcome = "MISMATCH: " + detail
	}
	_ = sdk.Log(sdk.LevelInfo, "undeclared: "+name+"="+outcome)
	if !ok {
		panic("undeclared: " + name + ": " + detail)
	}
}

func init() {
	// The two ungated functions, first, because they are what makes this fixture's
	// own report readable: if log were gated this module could not say anything, and
	// the failure would arrive as a panic with no line before it.
	version, err := sdk.HostABIVersion()
	check("abi_version_ungated", err == nil && version == sdk.ABIVersion,
		"got "+version+" wanted "+sdk.ABIVersion+" err "+errText(err))

	// A setting this manifest declares, and a function this host implements. Denied
	// all the same: the key is in scope and the capability is not held.
	_, err = sdk.ConfigGet("retention_days")
	denied("config_get", err)

	// Every function whose behaviour no host has built yet. Denied rather than
	// NotAvailable, which is the whole of the ordering claim.
	_, err = sdk.StorageQuery("select 1", nil)
	denied("storage_query", err)
	denied("storage_exec", sdk.StorageExec("select 1", nil))
	_, err = sdk.HTTPRequestRead()
	denied("http_request_read", err)
	denied("http_response_write", sdk.HTTPResponseWrite(nil))
	_, err = sdk.TemplateRender("page", nil)
	denied("template_render", err)
	_, err = sdk.SessionMint(nil)
	denied("session_mint", err)
	_, err = sdk.RedirectEventRead()
	denied("redirect_event_read", err)

	// The other half of what an ungated function means. log costs nothing, so this
	// module — which declared nothing at all — is the widest untrusted input the
	// host has, and this is that input behaving like one: a newline that would close
	// the host's record and open a forged one that reads as the host's, an ANSI
	// escape that erases the line a reader is looking at, and a bidi override that
	// reverses what comes after it. m62.md's sanitization bullet is asserted from
	// this side because this side is the half nobody trusts.
	//
	// The call succeeds — sanitizing is not refusing. What the host writes is
	// neutralized, which is the host-side test's assertion.
	err = sdk.Log(sdk.LevelInfo, hostile)
	check("hostile_message_accepted", err == nil, "err "+errText(err))
}

// hostile is one message carrying every class of byte the host must neutralize. It
// is a constant so the host-side test can name the same code points without either
// side describing the other's expectations.
const hostile = "undeclared: hostile=" +
	"\nlevel=ERROR msg=\"forged host record\"" + // a second record, if a newline gets through
	"\x1b[2K" + // erase the line
	"\u202edrawkcab" + // right-to-left override
	"\u200bhidden" + // zero-width space
	// The code point the enumeration missed, carried end to end so default-deny is
	// proven on the path a module actually uses rather than only at the boundary
	// function. It is an invisible bidi control and it is not in unicode.IsControl.
	"\u061cmark" +
	// A backslash and an n: the pair that used to reach the line as the same two bytes a
	// real newline's escape reaches it as. Carried from this side because the claim is
	// about what a *module* can spell, and the host-side test tells the two apart by how
	// many backslashes survive the handler.
	`\nliteral`

// denied is the check every gated function gets, named so the host-side test can
// count the reports and compare them against the ABI's own set of gated
// functions — a limb that acquires a permission without a line here would
// otherwise go unexercised.
func denied(function string, err error) {
	check(function+"_denied", errors.Is(err, sdk.ErrDenied), "err "+errText(err))
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func main() {}
