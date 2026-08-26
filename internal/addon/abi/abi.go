// Package abi is the add-on ABI: the complete set of functions a module may
// import from this host, the version that set is published under, and the rule
// for deciding whether a change to it is breaking.
//
// # The set of imports is the ABI
//
// Owner-set 2026-08-18, in those words. There is no second surface: an add-on
// reaches this product through the functions in [Functions] and through nothing
// else, so enumerating them enumerates the whole contract. The host owns the
// definition and add-ons consume a generated SDK, which is why this package —
// not the SDK, and not any document — is the authoring point. Everything else
// that describes the ABI is generated from here:
//
//   - the SDK an add-on imports, in ../../../sdk, by `make abi-sdk`;
//   - the function table in docs/addon-abi.md, by the same target;
//   - the host module the runtime registers, in ../hostabi.go, which builds
//     wazero's parameter types from [Function.Params] rather than restating
//     them.
//
// The last of those is what makes "one authoring point" structural instead of a
// convention: the host and the guest derive their signatures from the same
// slice, so they cannot disagree about one.
//
// # This package holds no behaviour
//
// Deliberately. It is imported by the host, by the generator and by tests, and
// it must stay importable without dragging a runtime, a database or a logger
// behind it — the privacy assertion in abi_test.go is a test over the ABI
// *surface*, and a surface that can only be examined by starting a host is not
// one.
package abi

import (
	"errors"
	"fmt"
)

// The ABI's own version, which is not the product's.
//
// SemVer with deprecation windows, owner-set 2026-08-18 against a
// recommendation of path versioning like /api/v1. docs/addon-abi.md is the
// policy that answers "is this change minor or major"; this is only the number.
//
// It is 0.x, and stays 0.x for this phase. 1.0 would *mean* the contract is
// stable, which is the phase close's to state and not this milestone's to claim
// early — m61.md says so in as many words.
const (
	VersionMajor = 0
	VersionMinor = 1
	VersionPatch = 4
)

// Version is the SemVer string the ABI publishes, and what the abi_version host
// function hands a module that asks. Asserted against the three integers above
// by test, because two spellings of one number is how they come to differ.
//
// The patch moved at M65, at M66 and again at M68, and which component moved is
// the whole of what docs/addon-abi.md's table decides. Adding a function is
// **additive**, and while the major is zero the *minor* is the breaking axis — so
// additive is the patch, and [Generation] does not move. `redirect_decision_read`
// and `redirect_answer_write` are therefore importable by a module built against
// 0.1.1 that is rebuilt against 0.1.2, and invisible to one that is not; the one
// failure mode is the documented patch case, a module built against 0.1.2 loaded
// on a 0.1.1 host, where the import does not resolve and instantiation fails
// naming the function.
//
// **M68 moved it without adding a function**, which is the first time that has
// happened and is why the policy gained a row for it. `config_get` answered from
// the manifest default and the environment; it now answers from a value an
// operator saved in the Add-on manager as well. Nothing narrowed, no parameter
// moved and no status changed for a case that already had one, so the table's
// *adding a source an answer may come from* row applies and it is additive —
// docs/addon-abi.md's "An answer that gains a source" is the reasoning and fixes
// the part a publisher actually needs, which is when the answer is re-read. This
// patch is invisible in the way the two above are not: nothing new is importable,
// so a module built against 0.1.3 loads on a 0.1.2 host unchanged.
//
// **M68.5 moved it the ordinary way**, which after M68's exception is worth
// saying: `network_fetch` is a function this ABI did not have, so the *adding a
// function* row applies unchanged — additive, the patch moves, [Generation] does
// not, and a module built against 0.1.3 and rebuilt against 0.1.4 can import it
// while one that is not rebuilt cannot see it. It is the same shape as
// `redirect_decision_read` at 0.1.2, and the same one failure mode: a module
// built against 0.1.4 loaded on a 0.1.3 host fails to instantiate, naming the
// import.
//
// **Making `redirect_event_read` live moved nothing**, and that is the policy
// working rather than an omission: implementing a function this ABI already
// declared is not a change to the contract, which docs/addon-abi.md states in as
// many words and is the whole reason the declared-but-refused pattern costs no
// version at all.
const Version = "0.1.4"

// Generation is the integer axis a breaking change moves along, and it is what a
// manifest's abi_version field names.
//
// SemVer puts the breaking axis in a different component before and after 1.0 —
// under 0.x "anything may change at any time" and the practice every consumer
// expects is that the **minor** is where a break lands, while from 1.0 on it is
// the major. A manifest declares one integer (M60 fixed the field, and the
// schema is already public), so that integer names whichever component is
// currently load-bearing. [GenerationOf] is the rule; this constant is the
// current answer and the test holds the two together.
//
// The consequence for a publisher is stated in docs/addon-abi.md: abi_version 1
// means "built against the ABI's first generation", which is 0.1.x today and
// becomes 1.x when the contract stabilises without any break in between.
const Generation = VersionMinor

// MinimumGeneration is the oldest generation this host still loads. Anything
// below it is past the end of its deprecation window.
//
// One, because there has only ever been one. It moves when a window closes, and
// the window is what docs/addon-abi.md fixes: a generation stays loadable for at
// least two of this product's minor releases and at least 90 days after the
// release that announced its retirement, whichever ends later.
const MinimumGeneration = 1

// GenerationOf is the mapping from a SemVer pair to the integer a manifest
// declares. Stated as code rather than as prose in the policy document, so the
// claim "the minor is the breaking axis while the major is zero" is executable.
func GenerationOf(major, minor int) int {
	if major == 0 {
		return minor
	}
	return major
}

// Errors a load-time ABI check can produce. Both are refusals; they are
// distinguishable because the operator's fix differs — one waits for a newer
// LinkCtrl, the other for a rebuilt add-on.
var (
	// ErrTooNew is an add-on built against a generation this host has not
	// reached: it may import functions that do not exist here.
	ErrTooNew = errors.New("add-on was built against a newer ABI generation")
	// ErrRetired is an add-on built against a generation whose deprecation
	// window has closed.
	ErrRetired = errors.New("add-on was built against a retired ABI generation")
)

// CheckGeneration reports whether a module declaring this abi_version may load.
//
// The two refusals are the whole of the compatibility promise as it can be
// checked at load time. What cannot be checked here is the *minor within a
// generation*: a module built against 0.1.3 and loaded on 0.1.0 would import a
// function added in 0.1.2, and the manifest does not carry the patch it was built
// against. That failure is not silent — the import does not resolve and
// instantiation fails, naming the function — which is why the manifest was not
// grown a second version field for it. docs/addon-abi.md states this as the one
// case a publisher has to read a version number for.
func CheckGeneration(declared int) error {
	switch {
	case declared > Generation:
		return fmt.Errorf("%w: it declares abi_version %d and this host implements %d (ABI %s)",
			ErrTooNew, declared, Generation, Version)
	case declared < MinimumGeneration:
		return fmt.Errorf("%w: it declares abi_version %d and this host loads %d or newer (ABI %s)",
			ErrRetired, declared, MinimumGeneration, Version)
	default:
		return nil
	}
}
