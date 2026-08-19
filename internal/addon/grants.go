package addon

import (
	"slices"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
)

// Grants is the permission set one add-on holds, resolved once at load.
//
// **Resolved once, and the shape is load-bearing.** From M66 a grant check sits
// on the redirect path, where the inherited rule is a cached p99 under 20 ms, so
// [Grants.Has] has to be a lookup on an already-resolved set — never a read of
// the manifest, never a walk of the vocabulary, and never I/O. A test asserts it
// allocates nothing, and another asserts that editing a manifest's Permissions
// slice after load does not change what the add-on holds, because that is the
// falsifiable form of *resolved once*.
//
// The zero value is a valid empty grant set, which is what an add-on that
// declared nothing holds. Every method is therefore safe on it.
type Grants struct {
	// held is the set, not a slice, for the reason above. Unexported and never
	// mutated after resolveGrants returns, which is what makes copying a Grants —
	// as Addons() copies a Loaded — safe without a lock.
	held map[string]struct{}
}

// Has reports whether this add-on holds the named permission.
func (g Grants) Has(permission string) bool {
	_, ok := g.held[permission]
	return ok
}

// Len is how many grants are held.
func (g Grants) Len() int { return len(g.held) }

// Names is the held set, sorted, which is what a log line and a metric label
// carry. Sorted so that two boots of one instance produce the same string and an
// operator diffing them sees only real changes.
func (g Grants) Names() []string {
	out := make([]string, 0, len(g.held))
	for name := range g.held {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// String is the label form: the sorted names, comma-separated, and empty for an
// add-on that holds nothing.
func (g Grants) String() string { return strings.Join(g.Names(), ",") }

// resolveGrants turns a manifest's declarations into what the add-on actually
// holds, plus the declarations that were **withheld**.
//
// Withheld means declared, in the vocabulary, and not grantable on this host —
// `redirect.inline` today. It is not an error and does not refuse the add-on: the
// class exists so that M66 can turn it on, and refusing a module for declaring it
// would make the declaration unusable. The module simply does not hold it, every
// capability behind it is denied, and the host says so at load.
//
// A token *outside* the vocabulary never reaches here. Manifest.Validate refuses
// it, for the reason DisallowUnknownFields refuses an unknown field: a
// declaration this host cannot interpret is a manifest whose author expects
// behaviour that will not happen, and the vocabulary is closed.
func resolveGrants(m Manifest) (Grants, []string) {
	g := Grants{held: make(map[string]struct{}, len(m.Permissions))}
	var withheld []string
	for _, p := range m.Permissions {
		if abi.Grantable(p) {
			g.held[p] = struct{}{}
			continue
		}
		withheld = append(withheld, p)
	}
	slices.Sort(withheld)
	return g, withheld
}
