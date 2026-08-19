//go:build wasip1 && wasm

package sdk

import "unsafe"

// The generated wrappers hand a host function a buffer of their own and grow it
// when the value does not fit. The host writes nothing when it does not fit and
// answers the size instead, so the second attempt is exact — see
// docs/addon-abi.md's calling convention.
//
// growthAttempts is a bound rather than a loop, because a value that grows
// between two calls could otherwise spin here forever. Four is generous: the
// second attempt is already sized from the host's own answer, so reaching the
// fourth means the value changed three times while being read.
const (
	initialBuffer  = 512
	growthAttempts = 4
)

// stringPtr and bytesPtr are the whole of this package's unsafe surface.
//
// A zero-length value crosses as a nil pointer, which the host reads as empty
// rather than as an out-of-bounds offset; taking the address of the first element
// of an empty slice would panic, and passing an offset the host then bounds-checks
// against guest memory would be a different failure for the same argument.
//
// The imports these feed are declared //go:noescape, so the pointer must not
// outlive the call. Every generated wrapper keeps its argument alive across the
// call with runtime.KeepAlive for that reason.
func stringPtr(s string) unsafe.Pointer {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Pointer(unsafe.StringData(s))
}

func bytesPtr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(unsafe.SliceData(b))
}
