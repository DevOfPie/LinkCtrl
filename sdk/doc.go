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
