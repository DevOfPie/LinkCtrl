// Package sdk is what an add-on imports to reach LinkCtrl.
//
// It is generated from the host's own definition of the ABI and it is the only
// thing an add-on needs: it depends on nothing but the Go standard library, and
// on no LinkCtrl package at all. That is deliberate and it is asserted by a test
// — an add-on lives in its own repository, on its own release cycle, and a
// dependency on this product's internals would make every add-on a fork of it.
//
// # Building an add-on
//
// A module is a Go program compiled for wasip1 as a *reactor*, which is what
// -buildmode=c-shared produces: package initialization runs when the host
// instantiates it and the module then stays alive to be called into, rather than
// running main and exiting.
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o myaddon.wasm .
//
// Beside it goes an addon.json naming the module, its sha256 and the ABI
// generation it was built against — [ABIGeneration], which this SDK carries so
// that the number in the manifest and the number the code was compiled against
// come from one place. docs/configuration.md in the LinkCtrl repository documents
// every manifest field; docs/addon-abi.md documents this ABI and the deprecation
// policy that governs it.
//
// # What the host grants
//
// Only what is in this package. A module is instantiated with no filesystem, no
// environment, no arguments and its output discarded.
//
// **The clock and the random source are this machine's**, which is worth stating
// because the runtime this host is built on defaults to fakes for both and this
// paragraph said so until ABI 0.1.1. time.Now inside a module is the host's wall
// clock and crypto/rand reads the operating system's entropy, so the standard
// library does what you expect and code you wrote against it needs no change.
// [TimeNow] and [RandomBytes] are the same two sources with a documented shape —
// RFC 3339 in UTC, and a count you name — and they exist so a nonce, a `state`
// parameter or a PKCE verifier has an answer in the published contract rather
// than only in a runtime detail. Neither costs a permission.
//
// If you are reading this because you found the old sentence: a module built
// against an earlier SDK is unaffected and does not need rebuilding. The fix is
// underneath crypto/rand and time.Now, not in the two functions.
//
// A module also gets a bounded amount of memory — 8 MiB of linear memory, with a
// fresh instance per request — and growing past it traps, which the host answers
// as a 502 for that one request. It is room for a request's work rather than for
// a cache: what an add-on wants to keep goes in the schema its storage grant
// gives it, which is also the only thing that outlives the instance.
//
// **On the redirect path the instance is reused, and it makes no difference to
// what you may keep.** Building one per redirect cost the visitor 11 ms on a path
// whose target is 20 ms, so the host keeps instances and hands them on — after
// restoring your module's memory to exactly what your package initialization left.
// A package-level variable you write during one redirect is empty on the next, the
// same as if the instance had been destroyed, and the schema is still the only
// thing that survives. What *is* different is that your `init` runs once per
// instance rather than once per invocation, so anything it does outside memory —
// a log line, a storage write — happens once for many redirects rather than once
// each. A module
// whose memory section *demands* more than the bound as its minimum is refused at
// load, with the add-on named. A toolchain that pins a larger maximum instead
// costs nothing and changes nothing: the runtime substitutes its own limit for
// that declaration, and the instance gets 8 MiB either way.
//
// Cookies are bounded in a way worth knowing before you design a flow. You name
// them and read them back by name, but the host carries the whole set inside one
// cookie of its own, so an add-on's share of a browser's cookie store is fixed
// rather than chosen — about 3 KiB, past which the oldest values are dropped. A
// key to your own storage fits; a flow's state does not belong there.
//
// **Your routes are rate limited, and the budget is the instance's sign-in
// budget.** Every request that reaches a route your add-on serves is charged
// against LINKCTRL_LOGIN_RATE_PER_MIN — the operator's number, and tens of
// requests a minute per client address rather than thousands — which is the same
// allowance the login form spends. It applies to every add-on and not only one that can mint a session,
// and there is no per-add-on budget to raise instead. So a provider's
// server-to-server callback that retries hard, and a page of yours a browser
// polls, are both spending an allowance somebody else's sign-in also needs;
// a refusal is a 429 the host answers, and your module is not entered. A path
// under /addons/ naming no installed add-on is a 404 refused on shape and is
// charged to nobody. docs/addon-abi.md states it in full.
//
// Every function returns an error from the closed set in this package. A function
// this ABI declares and this host has not implemented yet answers
// [ErrNotAvailable], which is a fact a module may branch on: the ABI is complete
// as a contract one release before it is complete as behaviour, and probing for a
// capability is how a single module works on two hosts.
//
// # Off wasm
//
// Every function in this package compiles for any GOOS, so an add-on's own tests
// build and run natively. Off wasip1 each one returns an error saying so, which
// is the honest answer: the host is not there.
package sdk
