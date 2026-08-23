//go:build wasip1

// Command slow is the module the redirect deadline exists for: it loads
// perfectly well and then never finishes an inline invocation.
//
// It is `spinning`'s sibling and the distinction between them is the whole of
// what M66 added. `spinning` never finishes *loading*, which the load timeout
// bounds; this one loads in microseconds and hangs on the redirect path, holding
// a visitor's request open, which nothing bounded before there was a deadline.
// So it is the fixture behind three claims at once — that an overrun is killed,
// that the redirect completes without it, and that
// `linkctrl_addon_redirect_kills_total` moves — and it is what the slow-module k6
// run in docs/slo.md drives.
//
// The loop is deliberately not a bare `for {}`, for `spinning`'s reason: an empty
// loop is a shape a compiler is free to reason about, and what the host has to
// interrupt is a guest genuinely executing wasm instructions. The accumulator is
// exported so nothing may conclude the work is dead.
package main

import "github.com/DevOfPie/LinkCtrl/sdk"

//go:wasmexport linkctrl_fixture_spun
func spun() int32 { return int32(work) }

var work uint32

//go:wasmexport linkctrl_redirect_inline
func inline() int32 {
	// Read first, so the invocation has genuinely started and the host is timing a
	// module that was answering rather than one that never woke up. The answer is
	// discarded: this module's whole behaviour is what it does next.
	_, _ = sdk.RedirectDecisionRead()
	for i := uint32(1); ; i++ {
		work = work*31 + i
	}
}

//go:wasmexport linkctrl_redirect_observe
func observe() int32 {
	// The same hang in the other class, so the deadline can be shown to bound an
	// observing invocation too — off the path, where what it protects is the
	// worker rather than a visitor.
	_, _ = sdk.RedirectEventRead()
	for i := uint32(1); ; i++ {
		work = work*31 + i
	}
}

func main() {}
