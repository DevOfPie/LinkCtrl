//go:build wasip1

// Command minimal is the smallest thing this repository will call an add-on: a
// reactor module that instantiates, exports one function, and reaches the host
// through the published SDK and nothing else.
//
// It exists to be loaded. Since M61 it is also the first proof the SDK compiles
// a working consumer: the import below is the same one an add-on in another
// repository writes, and the log line it produces during instantiation is a host
// function answering a guest for real. The exported function is here so a test
// can show the instance is *live* after Open returns rather than merely accepted
// — a command module would have run main and exited, and the difference is the
// whole reason the build uses -buildmode=c-shared.
//
// The build tag keeps it out of every native build and every linter run while
// leaving `go build` on an explicit path able to reach it. `//go:build ignore`
// is what internal/geoip/testdata uses for a generator nothing compiles; this
// file is compiled, for one GOOS, which is what the tag says instead.
package main

import "github.com/DevOfPie/LinkCtrl/sdk"

// The name is prefixed because a module's export namespace is flat and shared
// with whatever the toolchain puts there. Nothing consumes it yet; the ABI's own
// exported names — the host calling *into* a module — are M64's and M66's.
//
//go:wasmexport linkctrl_fixture_ok
func fixtureOK() int32 { return 1 }

// init runs during instantiation, which is what makes this a load-time call. The
// error is deliberately ignored: this fixture's job is to load, and probe is
// where every answer is checked.
func init() {
	_ = sdk.Log(sdk.LevelInfo, "minimal fixture initialized against ABI "+sdk.ABIVersion)
}

// Required by the toolchain even for a reactor module, and never called: with
// -buildmode=c-shared the entry point is _initialize, which runs package
// initialization and returns.
func main() {}
