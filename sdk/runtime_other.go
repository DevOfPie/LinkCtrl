//go:build !(wasip1 && wasm)

package sdk

import "errors"

// errNotWasm is what every function answers off wasip1.
//
// The package builds for every platform so that an add-on's own test suite
// compiles and runs with `go test` — and so that this repository's linters and
// `go vet ./...` see the SDK at all, which they would not if its only files were
// behind a wasip1 build tag. What it cannot do is fake a host: there is no
// instance on the other side of the call, and returning a plausible zero value
// would turn a test that never reached LinkCtrl into a test that appeared to.
var errNotWasm = errors.New("linkctrl: the host ABI is only reachable from a module built for GOOS=wasip1 GOARCH=wasm")
