//go:build wasip1

// Command minimal is the smallest thing this repository will call an add-on: a
// reactor module that instantiates, exports one function, and imports nothing
// beyond what the Go runtime needs to start.
//
// It exists to be loaded, not to do anything. M60 has no ABI — the host
// resolves no imports of its own — so the only thing a fixture can prove is
// that the lifecycle in internal/addon runs end to end against a module the
// standard toolchain produced. The exported function is here so the test can
// show the instance is *live* after Open returns rather than merely accepted:
// a command module would have run main and exited, and the difference is the
// whole reason the build uses -buildmode=c-shared.
//
// The build tag keeps it out of every native build and every linter run while
// leaving `go build` on an explicit path able to reach it. `//go:build ignore`
// is what internal/geoip/testdata uses for a generator nothing compiles; this
// file is compiled, for one GOOS, which is what the tag says instead.
package main

// The name is prefixed because a module's export namespace is flat and shared
// with whatever the toolchain puts there. Nothing consumes it yet; M61 is where
// exported names become a contract.
//
//go:wasmexport linkctrl_fixture_ok
func fixtureOK() int32 { return 1 }

// Required by the toolchain even for a reactor module, and never called: with
// -buildmode=c-shared the entry point is _initialize, which runs package
// initialization and returns.
func main() {}
