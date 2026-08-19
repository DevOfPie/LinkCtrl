//go:build wasip1

// Command failing is a module that compiles, hashes and parses correctly and
// then refuses to instantiate. It is the third of the three ways a load can go
// wrong, and the only one that needs a real module to demonstrate: a bad
// manifest is caught before any wasm is read, and a checksum mismatch is caught
// before the runtime is asked for anything.
//
// The panic is in package initialization deliberately. -buildmode=c-shared
// makes _initialize the start function, so package init runs *during*
// instantiation — which is what makes an add-on able to fail at load time at
// all, and therefore what the failure class in the manifest is deciding about.
package main

func init() { panic("this fixture exists to fail at instantiation") }

func main() {}
