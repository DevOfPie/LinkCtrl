package addon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/build"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// This file is M68.6: the second way a module gets in.
//
// # It is a second door, not a replacement
//
// M67's upload stays exactly as it is, M60's boot directory stays exactly as it
// is, and the three produce the same add-on: from the digest check onward this
// file calls [Host.install], so the manifest parse, the checksum against the
// manifest, the staging directory, the single rename and the audit record are
// M67's, once. A test installs the same module both ways and compares the two
// directories byte for byte.
//
// **The reason this exists at all is [D365].** m67.md shipped *install is an
// upload, never a fetch* and argued server-side request forgery for it at length.
// No decision backed that: the owner's intention was local and direct-URL
// installs with a module store built on top of them, and the argument — which is
// a real argument — is answered by M68.5's machinery rather than by refusing the
// capability. Every bound that milestone asserts applies here unchanged, because
// this file drives the same [fetcher.get].
//
// # What authorizes the destination is not an allowlist
//
// An add-on's own fetch is bounded twice: this host dials only globally-routable
// unicast space, and an add-on reaches only the origins its operator wrote into a
// setting. **The second of those does not transfer**, and saying so is this
// milestone's central argument. An install is somebody holding `addons.manage`
// typing a URL at the moment of the act — there is no configured origin to check
// against, because the act *is* the configuration. So the origin policy here is
// [operatorURL]: the origin of the URL that was typed, and nothing else, which
// makes a redirect off that origin refused by the same two doors an add-on's
// fetch meets.
//
// The address policy is unchanged and is not weakened by any of that: a URL
// naming loopback, a link-local address, an RFC 1918 range or anything outside
// routable space is refused at dial time by [refuseAddress], after resolution,
// on every address a name resolves to and on every hop of a redirect. That is
// what makes this something other than an authenticated request-forgery
// endpoint, and it is why m67.md's argument is answered rather than overruled.
//
// # The digest is the whole of the rest of it
//
// **It comes from the operator and never from the URL.** A checksum fetched from
// the same place as the module proves nothing at all — whoever can serve the one
// can serve the other — so the install takes the expected `sha256` beside the URL
// and refuses the fetch that does not hash to it. The manifest inside the bundle
// still declares the module's digest and that check still runs, and it is not
// this one: it says the module matches *its own manifest*, which a hostile
// publisher satisfies trivially. What the operator's digest says is that the
// bundle is the bundle they meant, which is the only claim a host can check for
// them.
//
// So a manifest fetched from the URL is never the sole source of its own module's
// digest, and the reason that is *structural* rather than promised is the bundle:
// one object, one digest, manifest and module inside it together. A bundle is a
// tar, a gzipped tar or a zip — three containers under one member rule, which is
// [D384] — and which one it is comes from the bytes rather than from the URL's
// extension. See bundle.go.
//
// What none of this can do is make the operator's digest mean something. An
// operator who pastes a URL and a digest from the same web page has authenticated
// nothing, and no mechanism in this file replaces reading where the two came
// from. That is the honest reason a module store with publisher identity is the
// eventual answer and this is the foundation under it, and docs/SECURITY.md says
// so to the person who has to decide.

// InstallFetchTimeout bounds the whole of one bundle fetch, connect through last
// byte.
//
// **Not [DefaultFetchTimeout], and m68.6.md asks for the difference to be said
// out loud rather than inherited.** That number is three seconds and it was sized
// against a measurement of what an OIDC relying party fetches: discovery and key
// documents of 839 to 12,852 bytes, where the elapsed time is a round trip and a
// handshake rather than a transfer. A module is three orders of magnitude larger
// — the fixtures this repository builds are 1.8 MB to 3.6 MB, because a
// `GOOS=wasip1` binary from big-Go carries the runtime — so three seconds is a
// bound that would refuse ordinary installs on ordinary links.
//
// Ten seconds, and the arithmetic is the same shape [DefaultRouteDeadline]'s is,
// against the same ceiling. An install is a request in the application tree, so
// `LINKCTRL_HTTP_REQUEST_TIMEOUT` — fifteen seconds by default — is already
// cancelling the context this runs under, and a fetch bound at or above it would
// never fire. Ten leaves five seconds for what still has to happen after the last
// byte: hashing, unpacking, parsing, writing, and compiling a WebAssembly module,
// which is the expensive one. It is a ceiling and not a reservation — the fetch
// ends at this bound *or* at whatever is left of the request's own, whichever
// comes first.
//
// **What it does not promise is [MaxUploadBytes] in ten seconds.** That would
// need 3.4 MB/s sustained, and a module near the cap over a slow link will time
// out here. That is stated rather than engineered around, because the answer for
// such a module already exists and is better: upload it, where the bytes travel on
// the client's own request and no server-side deadline is guessing at a link this
// instance cannot see.
const InstallFetchTimeout = 10 * time.Second

// The refusals a URL install answers with, as codes rather than as sentences.
//
// **A code per bound**, because m68.6.md asks for a refusal that says which bound
// bit rather than *the upload was refused*. The dashboard renders none of the
// messages — everything on that surface is attacker-influenced text and it words
// its own sentence from the code (internal/httpx/web_addons.go) — so the code is
// the whole of what crosses to a reader, and a bound with no code of its own is a
// bound an operator cannot act on.
//
// [URLInstallCodes] is the closed set, and it is held from both ends. A test in
// internal/httpx holds the page's vocabulary against it, so a code added here
// without a sentence there is a failing build rather than a blank flash; a test
// in this package reads the outcomes [Host.fetchBundle] can arrive at out of the
// source that produces them, so a word added to [fetchFailure]'s switch without a
// code here is a failing build rather than a generic sentence. One direction
// alone would leave the list closed against the page and open against the wire.
const (
	// CodeURLInvalid is a URL this host will not make a request out of at all:
	// not https, no host, credentials in it, or not a URL.
	CodeURLInvalid = "url_invalid"
	// CodeDigestInvalid is a digest that is not 64 hex characters. Its own code
	// rather than a mismatch, because the operator mistyped the field rather than
	// fetched the wrong thing.
	CodeDigestInvalid = "digest_invalid"
	// CodeDigestMismatch is the one that matters: the bytes arrived and they are
	// not the bytes the operator named.
	CodeDigestMismatch = "digest_mismatch"
	// CodeBundleInvalid is bytes that hashed correctly and are not an add-on
	// bundle, or are one holding something other than a manifest and its module.
	CodeBundleInvalid = "bundle_invalid"
	// CodeBundleExpands is a compressed bundle that unpacks to more than this host
	// will carry, or at a ratio nothing a publisher builds produces. Its own code
	// rather than [CodeBundleInvalid], because the archive parsed: what is wrong
	// with it is a number, and the operator can look at that number.
	CodeBundleExpands = "bundle_expands"
	// CodeBundleMismatch is a bundle whose module is not the file its own manifest
	// names.
	CodeBundleMismatch = "bundle_mismatch"
	// CodeFetchStatus is an origin that answered something other than 200.
	CodeFetchStatus = "fetch_status"
)

// URLInstallCodes is every code above, plus one per fetch outcome that can reach
// an operator. Sorted, because it is a vocabulary rather than a sequence.
//
// The fetch half is spelled `fetch_` + the word from [abi.FetchOutcomes], so the
// counter an operator reads on the Add-on manager and the refusal they read on
// the install form use the same word for the same event.
var URLInstallCodes = []string{
	CodeBundleExpands,
	CodeBundleInvalid,
	CodeBundleMismatch,
	CodeDigestInvalid,
	CodeDigestMismatch,
	CodeFetchStatus,
	"fetch_address_refused",
	"fetch_connect_failed",
	"fetch_dns_failed",
	"fetch_origin_refused",
	"fetch_redirect_refused",
	"fetch_timeout",
	"fetch_too_large",
	CodeURLInvalid,
}

// URLInstallRequest is what an operator typed: where the bundle is, and what it
// must hash to.
//
// **Two fields and they are inseparable.** The digest is not optional and there
// is no shape of this request without one — an install that fetched whatever the
// URL happened to serve would be exactly the request forgery m67.md refused, with
// the response executed. The surfaces put the two inputs side by side for the
// same reason.
type URLInstallRequest struct {
	// URL is the bundle's address. https only, checked by the same
	// [checkFetchRequest] an add-on's own fetch passes through.
	URL string
	// SHA256 is what the fetched bundle must hash to, lowercase hex, supplied by
	// the operator and never read out of anything this host fetched.
	SHA256 string
}

// digestRe is the expected digest's shape: sha256 as lowercase hex, which is what
// `sha256sum` prints and what a manifest's own `sha256` field carries.
var digestRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// InstallFromURL fetches a bundle, verifies it against the digest the operator
// supplied, and installs it exactly as an upload is installed.
func (h *Host) InstallFromURL(
	ctx context.Context, actor *auth.Identity, req URLInstallRequest,
) (Installed, error) {
	if !actor.Can(auth.PermAddonsManage) {
		return Installed{}, fmt.Errorf("%w: installing an add-on requires %s",
			domain.ErrForbidden, auth.PermAddonsManage)
	}
	return h.installFromURL(ctx, actor, req)
}

// installFromURL is InstallFromURL with the permission already established. The
// split is [Host.install]'s and exists for the same reason: nothing outside
// internal/auth can mint an authority, so a unit test in this package cannot
// produce an actor holding `addons.manage`. The gate itself is asserted in
// test/integration, both directions.
func (h *Host) installFromURL(
	ctx context.Context, actor *auth.Identity, req URLInstallRequest,
) (Installed, error) {
	if h == nil || h.dir == "" {
		return Installed{}, ErrNoAddonsDir
	}

	digest := strings.ToLower(strings.TrimSpace(req.SHA256))
	if !digestRe.MatchString(digest) {
		return Installed{}, domain.ValidationErrors{{
			Field: "sha256", Code: CodeDigestInvalid,
			Message: "the expected digest is the module bundle's sha256 as 64 lowercase " +
				"hex characters, which is what sha256sum prints",
		}}
	}

	raw, err := h.fetchBundle(ctx, strings.TrimSpace(req.URL))
	if err != nil {
		return Installed{}, err
	}

	// **Before anything looks inside.** What is in hand is bytes an address chose,
	// and the operator's digest is the only statement about them this host can
	// check — so it is checked before an archive reader, a decompressor, a JSON
	// parser or a compiler is pointed at them, and a bundle that is not the
	// expected one is never unpacked at all. That ordering is what keeps three
	// container parsers from being three parsers reachable by anybody: the bytes
	// have to be the bytes the operator named before one of them runs.
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != digest {
		h.log.Warn("a URL install was refused: the fetched bundle is not the one the "+
			"operator named",
			slog.String("expected_sha256", digest),
			slog.String("fetched_sha256", got),
			slog.Int("bytes", len(raw)))
		return Installed{}, domain.ValidationErrors{{
			Field: "sha256", Code: CodeDigestMismatch,
			Message: fmt.Sprintf("the bundle at that URL hashes to %s and you expected %s; "+
				"nothing was written", got, digest),
		}}
	}

	manifest, moduleName, module, err := unbundle(raw)
	if err != nil {
		return Installed{}, domain.ValidationErrors{{
			Field: "url", Code: bundleRefusal(err), Message: err.Error(),
		}}
	}
	// The manifest is parsed here **only** to hold the archive's own naming
	// against it. [Host.install] parses it again and that parse is the
	// authoritative one — this file adds no second reader and no second set of
	// refusals, which is what *from the digest check onward the path is M67's*
	// means in code.
	m, err := parseManifest(bytes.NewReader(manifest))
	if err == nil && m.Module != moduleName {
		return Installed{}, domain.ValidationErrors{{
			Field: "url", Code: CodeBundleMismatch,
			Message: fmt.Sprintf("the bundle's manifest names %q and the bundle carries %q; "+
				"a bundle holds the module its own manifest describes",
				moduleText(m.Module), safeName(moduleName)),
		}}
	}

	out, err := h.install(ctx, actor, InstallRequest{Manifest: manifest, Module: module})
	if err != nil {
		return Installed{}, err
	}
	// Beside the install's own line rather than instead of it, and it carries the
	// one fact that line cannot: **where this came from**. The digest is in both,
	// which is what lets an operator reading the log tie the module now running to
	// the address it was pulled from and to the audit record of the act.
	h.log.Warn("the add-on that was just installed arrived over the network, from a "+
		"URL somebody typed and a digest they supplied",
		slog.String("addon", out.Name),
		slog.String("sha256", out.SHA256),
		slog.String("bundle_sha256", digest),
		slog.String("origin", installOrigin(req.URL)))
	return out, nil
}

// fetchBundle makes the one outbound request, and turns everything that can go
// wrong with it into a refusal naming which bound bit.
func (h *Host) fetchBundle(ctx context.Context, raw string) ([]byte, error) {
	if h.installFetcher == nil {
		// Unreachable on a host with a directory — Open builds it beside the
		// add-on fetcher. Answered rather than assumed, because a nil client is a
		// panic and this is a path an operator reaches by hand.
		return nil, fmt.Errorf("%w: this instance cannot fetch", domain.ErrUnavailable)
	}
	req := fetchRequest{
		URL:    raw,
		Method: http.MethodGet,
		// **Not `application/json`.** The add-on path asks for JSON because what it
		// fetches is a discovery document; what this fetches is an archive, and an
		// origin content-negotiating against the wrong Accept would serve an HTML
		// page that then fails the digest — a refusal naming the right bound for the
		// wrong reason. All three container types are named, and none of them decides
		// anything: what a response *is* comes from its leading bytes
		// ([detectBundleFormat]), so this header is a preference expressed to a
		// polite origin rather than an input to the reader.
		Accept: "application/x-tar, application/gzip, application/zip, " +
			"application/octet-stream;q=0.9, */*;q=0.1",
		// Named as an install rather than as an add-on, because there is no add-on
		// yet: what the origin's log should show is this product asking for a
		// bundle, and the version is what tells its publisher which ABI the asker
		// supports.
		UserAgent: "LinkCtrl/" + build.Get().Version + " (+add-on install)",
		// The digest has to cover the file the operator hashed, not what a transport
		// handed back after inflating it. See [fetchRequest.Identity] — the add-on
		// path deliberately does not set this (F340).
		Identity: true,
	}
	u, resp, err := h.installFetcher.get(ctx, req, operatorURL{})
	switch resp.Stage {
	case stageRequest:
		return nil, domain.ValidationErrors{{
			Field: "url", Code: CodeURLInvalid,
			Message: "that is not an address this host will fetch from: " + err.Error(),
		}}
	case stagePolicy:
		// [operatorURL] permits the origin of the URL it was given, so the only way
		// to arrive here is a redirect that left it — which checkRedirect refuses
		// first — or a URL whose origin cannot be read, which stageRequest already
		// refused. Answered anyway and in the fetch vocabulary, because a policy
		// that can never refuse is a policy nobody would notice becoming wrong.
		return nil, urlFetchRefusal(resp.Outcome,
			"the origin this host was pointed at is not the one it ended up asking")
	case stageConnect:
		if resp.Refusal != nil {
			h.log.Warn("this host refused to dial an address a URL install resolved to",
				slog.String("origin", originString(u)),
				slog.String("outcome", resp.Outcome),
				slog.String("address", resp.Refusal.addr.String()),
				slog.String("address_rule", resp.Refusal.rule),
				slog.String("reason", resp.Refusal.why))
			return nil, urlFetchRefusal(resp.Outcome, resp.Refusal.why+
				". This host dials globally-routable addresses only; the server log names "+
				"which rule refused it under address_rule")
		}
		h.log.Warn("a URL install did not complete",
			slog.String("origin", originString(u)),
			slog.String("outcome", resp.Outcome),
			slog.Any("error", err))
		return nil, urlFetchRefusal(resp.Outcome, "the request did not complete")
	case stageRead:
		h.log.Warn("a URL install failed while reading the bundle",
			slog.String("origin", originString(u)),
			slog.String("outcome", resp.Outcome),
			slog.Any("error", err))
		return nil, urlFetchRefusal(resp.Outcome, "the bundle did not finish arriving")
	case stageCap:
		h.log.Warn("a URL install answered more than this host will carry",
			slog.String("origin", originString(u)),
			slog.Int64("cap_bytes", MaxUploadBytes))
		return nil, urlFetchRefusal(resp.Outcome,
			"an add-on bundle is at most "+byteBound(MaxUploadBytes)+
				", and that URL answered with more")
	}
	if resp.Status != http.StatusOK {
		// A 404 or a 403 from a release page is the commonest way this fails and it
		// is nothing to do with the bundle, so it says the number rather than
		// reporting that a digest did not match something that never arrived.
		h.log.Warn("a URL install was answered with something other than 200",
			slog.String("origin", originString(u)), slog.Int("status", resp.Status))
		return nil, domain.ValidationErrors{{
			Field: "url", Code: CodeFetchStatus,
			Message: fmt.Sprintf("that URL answered %d; an install reads a bundle and "+
				"nothing else", resp.Status),
		}}
	}
	return resp.Body, nil
}

// urlFetchRefusal is one fetch outcome as a field error, in the vocabulary the
// Add-on manager's outcome table already uses.
func urlFetchRefusal(outcome, why string) error {
	return domain.ValidationErrors{{
		Field: "url", Code: "fetch_" + outcome, Message: why + ". Nothing was written.",
	}}
}

// operatorURL is the origin policy a URL install carries: whatever origin the
// operator's own URL names, and nothing else.
//
// **The empty struct is the design.** There is nothing to configure, because the
// thing being authorized is the act of typing — so this permits the URL it is
// asked about and relies on [fetcher.checkRedirect] to refuse a hop that leaves
// it. That is the same arrangement an add-on gets, read from the other end: an
// add-on's set comes from a setting somebody deployed, and an operator's set is
// the one URL in front of them.
//
// It is a type rather than a closure so that the one call site reads as a policy
// being chosen, which is the whole point of [fetcher] taking one.
type operatorURL struct{}

func (operatorURL) permits(u *url.URL) (bool, string) {
	if _, err := originOf(u); err != nil {
		return false, "origin_refused"
	}
	return true, "ok"
}

// installOrigin is a typed URL reduced to its origin for a log line — never the
// path, which is the operator's business and may carry a token.
func installOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "unparseable"
	}
	return originString(u)
}

// byteBound writes a cap the way the documentation writes it. internal/httpx has
// the same function for the same constant; this one exists so that a refusal
// produced by the service says the same thing when nothing HTTP is involved.
func byteBound(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}
