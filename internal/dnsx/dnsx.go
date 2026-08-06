// Package dnsx is this program's DNS client, and it is one function.
//
// It exists as its own package rather than as a method on the service that uses
// it, and the reason is a guard test in internal/link: that package decides what
// a destination may be, and the one thing it is permitted to dial is a
// reputation-feed check behind an interface an operator opted into. A source
// scan there fails the build on any outbound symbol at all — net.Dial,
// http.Client, net.Resolver — because a DNS lookup added to "check the host
// resolves" would send a user's destination to a nameserver that /feeds does not
// name. The page names two channels, and a third one is what the scan is for.
//
// Custom-domain verification (M40) genuinely needs DNS, so the talking happens
// here and internal/link holds a one-method interface. The distinction is not
// cosmetic: what this resolves is a hostname a workspace registered on this
// instance, never a link's destination, and the whole of the traffic is a TXT
// query for a fixed label under a name somebody typed into the domains page.
package dnsx

import (
	"context"
	"net"
	"time"
)

// DefaultTimeout bounds one lookup when none is configured.
const DefaultTimeout = 5 * time.Second

// Resolver looks up TXT records through the host's own resolver.
//
// Bounded per lookup rather than by the caller's context, because the failure it
// has to survive is a nameserver that accepts a query and never answers: the
// re-verification pass walks every registered hostname, and one unbounded lookup
// would hold the whole pass.
type Resolver struct {
	// Timeout bounds one lookup. Zero means DefaultTimeout.
	Timeout time.Duration
}

func (r Resolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return net.DefaultResolver.LookupTXT(lctx, name)
}
