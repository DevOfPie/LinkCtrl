// Package authtest builds [auth.Identity] values for tests in other packages.
//
// [auth.Identity] carries its permission set unexported, and deliberately: the
// only things that may fill it are the loaders in internal/auth, which read a
// membership or an instance grant out of the database. That is what stops a call
// site anywhere else from deciding it holds a permission.
//
// A handler test still has to be able to present a caller who holds one. This
// package is the seam, and it is a separate package rather than an exported
// constructor on Identity so that the ability to conjure a permission is
// something a production import would have to name — `internal/auth/authtest` in
// an import block is visible in a way a method on a type in scope is not.
//
// Nothing outside a test may import this. [TestingT] is the enforcement that can
// be expressed in the type system: every constructor demands a *testing.T, which
// production code has nowhere to get.
package authtest

import (
	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
)

// TestingT is the part of *testing.T this package needs. An interface rather
// than the concrete type so a helper can be exercised without a real test
// running, and so this package does not import "testing" into any binary.
type TestingT interface {
	Helper()
}

// Identity is a signed-in user holding exactly the permissions named.
//
// Everything tenancy-shaped is a fixed, obviously-synthetic value: these are for
// tests that care who may do a thing, not which workspace they are in. Pass what
// matters by editing the returned value.
func Identity(t TestingT, permissions ...string) *auth.Identity {
	t.Helper()
	id := &auth.Identity{
		UserID:      uuid.MustParse("01950000-0000-7000-8000-00000000a11e"),
		Email:       "actor@example.test",
		Name:        "Test Actor",
		WorkspaceID: uuid.MustParse("01950000-0000-7000-8000-00000000b00c"),
		OrgID:       uuid.MustParse("01950000-0000-7000-8000-00000000c0de"),
		Role:        "owner",
	}
	auth.SetPermissionsForTest(id, permissions...)
	return id
}
