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
// environment, no arguments, its output discarded, and the runtime's *fake*
// clock and random source rather than this machine's — so time.Now and
// crypto/rand inside a module are not what they are on a server, and anything
// needing either has to ask the host for it through a function here. There is no
// such function yet, which is a real limitation and not an omission from this
// paragraph.
//
// A module also gets a bounded amount of memory — 8 MiB of linear memory, with a
// fresh instance per request — and growing past it traps, which the host answers
// as a 502 for that one request. It is room for a request's work rather than for
// a cache: what an add-on wants to keep goes in the schema its storage grant
// gives it, which is also the only thing that outlives the instance. A module
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
