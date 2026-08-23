//go:build race

package addon

// raceDetector is whether this binary was built with `-race`, and it exists for
// exactly one test: [TestAnInlineInvocationCostsAnInstantiation], which compares a
// measured invocation against the deadline this product ships.
//
// **The detector moves that number by an order of magnitude**, which is not a
// guess — the same effect is already recorded for the load path (D225): one
// fixture loads in 380 ms here and in 3.9 s under `-race` alongside the rest of
// this package. An inline invocation measures ~3 ms plainly and ~20 ms under the
// detector, and past 25 ms when the whole package is running in parallel around
// it. Asserting the shipped default against that would be asserting about a
// configuration no instance runs in, and it fails intermittently on how busy the
// machine is, which is the worst of both.
//
// So the *measurement* is taken either way and reported either way — a number
// nobody can see is not measured into anything — and only the comparison against
// [DefaultInlineDeadline] is skipped here. `make check` runs with the detector, so
// what checks the default is a plain `go test`, and the test says so when it
// skips.
const raceDetector = true
