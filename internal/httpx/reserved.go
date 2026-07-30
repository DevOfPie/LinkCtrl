package httpx

import "github.com/DevOfPie/LinkCtrl/internal/alias"

// isReserved reports whether a top-level path is on the alias reserved list.
// Used by the router test that guards against a user-created alias shadowing a
// real route.
func isReserved(path string) bool { return alias.IsReserved(path) }
