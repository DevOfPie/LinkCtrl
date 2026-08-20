//go:build wasip1

// Command spinning is a module that never finishes loading. It is the fourth way
// a load can go wrong and the only one that is not a failure from the guest's
// point of view: nothing panics, nothing is malformed, and the module is running
// perfectly well — forever.
//
// This is F287's reproduction, kept as a fixture. Package initialization runs
// *during* instantiation under -buildmode=c-shared, so a loop here means
// InstantiateModule never returns; before the deadline in host.go's Open, that
// meant addon.Open never returned either, and the instance never reached its
// listener. There was no log line naming the add-on, no metric and no error —
// the failure class the manifest declared decided nothing, because nothing had
// failed yet.
//
// The loop is deliberately not a busy `for {}`. A bare empty loop is the shape a
// compiler is free to reason about, and this fixture has to be a module that is
// genuinely executing wasm instructions for as long as it is allowed to: what the
// host interrupts is a running guest, so a fixture that got optimized into
// something else would be testing a different thing. The accumulator is exported
// so nothing may conclude the work is dead.
package main

//go:wasmexport linkctrl_fixture_spun
func spun() int32 { return int32(work) }

var work uint32

func init() {
	for i := uint32(1); ; i++ {
		work = work*31 + i
	}
}

func main() {}
