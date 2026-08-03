package httpx

import (
	"context"
	"net/http"

	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

// Serving a verified custom hostname (M40).
//
// **The gate is `verified_at` and this file is where it is applied to traffic.**
// A Host header reaches the redirect tree only if the hostname cache — whose one
// query filters on that column — returns a domain for it. A hostname that is
// unknown, registered but unverified, renamed, removed, or unverified twenty
// seconds ago on another replica is not in that set, so the request falls
// through to whatever the deployment did before custom domains existed: the
// operational 404 on a split-host instance, the ordinary single mux otherwise.
//
// Composed around the existing router rather than replacing it, which is the
// whole reason this is a wrapper. The split-host `hostRouter` keeps answering
// exactly as it did — including its refusal to serve an unrecognized name — and
// a single-host deployment keeps behaving exactly as it did. Custom domains are
// an additional arm, never a change to the two that were there.

// customDomainKey carries the resolved domain to the redirect handler.
type customDomainKey struct{}

// WithCustomDomain marks a request as being for a verified custom hostname.
func WithCustomDomain(ctx context.Context, d redirect.VerifiedDomain) context.Context {
	return context.WithValue(ctx, customDomainKey{}, d)
}

// CustomDomainFrom returns the verified domain this request arrived on, if any.
//
// Absent on every request to the instance's own hosts, which is every request on
// an instance with no custom domains — so the default redirect path pays one
// type assertion against a nil interface value and allocates nothing.
func CustomDomainFrom(ctx context.Context) (redirect.VerifiedDomain, bool) {
	d, ok := ctx.Value(customDomainKey{}).(redirect.VerifiedDomain)
	return d, ok
}

// customHostRouter dispatches a verified custom hostname to the redirect tree.
//
// The lookup is a read lock and a map read. It cannot query, cannot fall back to
// Postgres, and cannot block — which is what "no per-request query" means, and
// why the cache holds the whole verified set rather than entries with TTLs.
type customHostRouter struct {
	hosts *redirect.HostCache
	// custom is the tree a verified hostname is served by: operational
	// endpoints, its own root, and aliases. Deliberately not the dashboard —
	// a customer's hostname must not serve the management UI, and a session
	// cookie must never be offered a second origin to be sent to.
	custom http.Handler
	// next is what this instance did before: the split-host router, or the
	// single mux.
	next http.Handler
}

func (h customHostRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d, ok := h.hosts.Lookup(r.Host); ok {
		h.custom.ServeHTTP(w, r.WithContext(WithCustomDomain(r.Context(), d)))
		return
	}
	h.next.ServeHTTP(w, r)
}

// DomainRootRedirect serves the root of a verified custom hostname.
//
// A separate handler from RootRedirect and not a variant of it, because the two
// answer from different places for different reasons. The instance root reads
// one row through a TTL cache, because it belongs to a domain nothing else
// describes. A custom domain's root travels inside the verified-hostname set
// this request has already been matched against — so there is no cache to miss,
// no load function to fail, and no second invalidation path to keep in step with
// the first.
type DomainRootRedirect struct {
	// Status is the redirect code, following the instance's configured default
	// exactly as the instance root does.
	Status int
}

func (h *DomainRootRedirect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d, ok := CustomDomainFrom(r.Context())
	if !ok || d.RootRedirectURL == "" {
		// Unconfigured, which is where every custom hostname starts. 404 rather
		// than a default page, for the reason the instance root gives: an
		// instance that says nothing about itself is a legitimate choice.
		http.NotFound(w, r)
		return
	}
	status := h.Status
	if status == 0 {
		status = http.StatusFound
	}
	// The same headers the instance root writes. The redirect tree is outside
	// the security-header chain, so each response sets nosniff itself, and
	// no-store because the whole point of a 302 here is that it can be repointed.
	w.Header().Set("Location", d.RootRedirectURL)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}

// TLSAsk answers Caddy's on-demand TLS `ask` (decision D3).
//
// **The app never speaks ACME.** Certificates are the operator's, obtained by
// Caddy, and this endpoint is the whole of this program's part in it: Caddy asks
// "should I get a certificate for this name?" and the honest answer is "yes if
// and only if a workspace has proved it controls that name". Answering anything
// wider would make this instance an unauthenticated certificate-issuance trigger
// for any hostname on the internet, which is the abuse `ask` exists to prevent.
//
// It answers **only for verified custom domains**. The instance's own app and
// link hosts are configured statically in the Caddyfile — they are the
// operator's names, known before any request arrives — so answering for them
// here would be widening the endpoint for no gain.
//
// Unauthenticated by necessity: it is consulted during a TLS handshake, before
// any application request exists. That is affordable because it reads the same
// in-process map the router does, discloses only whether a name is already being
// served publicly, and performs at most one write per verification.
type TLSAsk struct {
	Hosts *redirect.HostCache
	// Activate records that the ask was answered for this domain, so ssl_status
	// stops saying `pending` for a certificate Caddy has now been told to get.
	// Nil records nothing, which leaves the column at `pending` and costs
	// nothing else.
	Activate func(ctx context.Context, d redirect.VerifiedDomain)
}

func (h *TLSAsk) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")

	name := r.URL.Query().Get("domain")
	d, ok := h.Hosts.Lookup(name)
	if !ok {
		// 404, which is what Caddy reads as "do not issue". A body, because an
		// operator will curl this while setting the deployment up and an empty
		// 404 is indistinguishable from the endpoint not existing.
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("not a verified domain on this instance\n"))
		}
		return
	}
	if h.Activate != nil && d.SSLStatus == redirect.SSLStatusPending {
		// Guarded on the cached status, and the storage write is guarded again
		// on `pending`, so this is one write per verification rather than one
		// per handshake — the endpoint is public and a statement it could run on
		// every request would be a write amplifier anybody can pull.
		h.Hosts.MarkTLSActive(d.Hostname)
		h.Activate(context.WithoutCancel(r.Context()), d)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte("ok\n"))
	}
}
