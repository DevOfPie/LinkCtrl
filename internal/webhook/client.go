package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// The delivery client, and the whole of M42's security posture.
//
// # The DNS-rebinding posture, stated rather than inherited
//
// The redirect path has an accepted rebinding gap, recorded in docs/SECURITY.md:
// a hostname that resolves publicly when a link is created can resolve privately
// when a visitor follows it, and closing that would mean a DNS lookup inside a
// 20ms budget. **That posture is not inherited here, and the reason is that the
// two are not the same fetch.** The redirect path sends a *visitor's browser*
// somewhere: the request leaves that person's network, the response never
// touches this instance, and 169.254.169.254 means whatever it means on their
// machine. A webhook sends *this server*: the request leaves the instance's own
// network, from inside whatever the instance can reach, and 169.254.169.254
// means the cloud metadata endpoint holding its credentials.
//
// So for server-initiated fetches this product checks **the address, not the
// name, at the moment the socket is opened**:
//
//   - Registration runs the full M30 tier check on the URL. That refuses
//     literals, obfuscated literals, localhost and everything the operator has
//     blocked. It cannot refuse a name that has not rebound yet, and it is not
//     asked to.
//   - Delivery checks every resolved address in the dialer's Control hook, which
//     runs after DNS has answered and before connect(2). The address that is
//     checked is the address that is connected to, so there is no window between
//     the check and the syscall for a second answer to arrive in. Every address
//     in a multi-record set is checked, because Control runs per attempt.
//   - **No redirect is followed at all.** Not "no redirect to a private
//     address" — none. A receiver that answers 302 is pointing this process at
//     a URL nobody registered, which is the sentence internal/feed already has,
//     and the status code is recorded so whoever owns the receiver can see
//     exactly what happened. That also means there is no second hop whose
//     address would need a second policy.
//
// The cost, stated because a posture with an unstated cost is a claim: an
// instance deployed behind an egress proxy that resolves names itself will have
// every address look like the proxy, and this check then says nothing. That is a
// property of the deployment rather than of the check, and it is in
// docs/SECURITY.md beside the rest of the operator's responsibilities.
//
// # Why this is not internal/feed's client
//
// The feed's dialer is fine for the feed and would be wrong here, in both
// directions. The feed dials one URL an operator wrote in configuration; its
// timeout is two seconds because it is spent inside a link creation somebody is
// watching, and it fails open by design. A webhook dials a URL a workspace
// member typed; its timeout is spent in a background job nobody is watching, and
// a failure is retried rather than shrugged off. Sharing one dialer would mean
// either putting this Control hook on the feed — where it would refuse an
// operator's deliberate choice to run a reputation service on their own
// network — or leaving it off here, which is the milestone. They share the
// *posture* on redirects, and that is the part worth copying.

// DefaultTimeout bounds one delivery attempt end to end.
//
// Ten seconds: long enough for a receiver on another continent that does real
// work before answering, short enough that a batch of twenty slow ones fits well
// inside the job's own bound. The setting is WEBHOOK_TIMEOUT.
const DefaultTimeout = 10 * time.Second

// maxResponseBytes bounds what is read back from a receiver.
//
// A webhook response is an acknowledgement; nothing in this product reads its
// body. The bound exists because the body is attacker-influenced — whoever
// registered the URL controls what it returns — and a receiver that streams
// forever would otherwise hold a delivery open for the whole timeout while
// filling memory.
const maxResponseBytes = 8 << 10

// SignatureVersion prefixes the signature header value, so the scheme can change
// without a receiver having to guess which one it is looking at.
const SignatureVersion = "v1"

// Header names, fixed here because they are a published interface. Anything that
// renames one has changed what every receiver in the world verifies.
const (
	HeaderEvent     = "X-LinkCtrl-Event"
	HeaderDelivery  = "X-LinkCtrl-Delivery"
	HeaderTimestamp = "X-LinkCtrl-Timestamp"
	HeaderSignature = "X-LinkCtrl-Signature"
)

// ErrPrivateAddress is what the dialer refuses with.
//
// Its own error so the delivery row says why nothing was sent, and so a test can
// assert the refusal rather than assert that *some* error happened — a test that
// passes because the host was simply unreachable is a test that would keep
// passing after the guard was deleted.
var ErrPrivateAddress = errors.New("webhook: refusing to connect to a private, " +
	"loopback or link-local address")

// GuardedTransport is the seam tests substitute at.
//
// An interface rather than http.RoundTripper directly, so a test cannot
// accidentally supply a transport that skips the guard without saying so: the
// name is the reminder, and the one production implementation is built below.
type GuardedTransport interface {
	http.RoundTripper
}

// Client delivers one signed payload to one receiver.
type Client struct {
	http    *http.Client
	timeout time.Duration
}

// Delivery is one queued event as the client sends it.
type Delivery struct {
	// ID is the delivery row, and it is the receiver's idempotency key: every
	// retry of one event carries the same value, and two events never share one.
	ID uuid.UUID
	// Event is the name from the vocabulary, also sent as a header so a receiver
	// can route without parsing the body.
	Event string
	// Payload is the rendered JSON, byte for byte as it was queued. Signed as
	// stored: re-encoding it here would produce a body whose signature a receiver
	// could not reproduce from what it received.
	Payload []byte
}

// NewClient builds the delivery client.
func NewClient(timeout time.Duration, rt GuardedTransport) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var transport http.RoundTripper = rt
	if transport == nil {
		transport = newGuardedTransport(timeout)
	}
	return &Client{
		timeout: timeout,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// No redirect is followed. See the package comment above: a receiver
			// that answers 302 is pointing this process at a URL nobody
			// registered, and returning the response means the 3xx is recorded
			// on the delivery instead of being chased.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// newGuardedTransport builds the only transport production ever has.
//
// The guard is on the dialer rather than on the URL, which is the whole point:
// a URL check answers a question about a name, and the question that matters is
// about an address.
func newGuardedTransport(timeout time.Duration) http.RoundTripper {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control:   guardDial,
	}
	return &http.Transport{
		DialContext: dialer.DialContext,
		// Small and shared. Deliveries are a background batch of twenty, not a
		// request path.
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: timeout,
		// No proxy, ever. Reading the environment here would let HTTP_PROXY point
		// every delivery at one host, which would both defeat the address check
		// below and send every workspace's events somewhere nobody registered.
		Proxy: nil,
	}
}

// guardDial is the dial-time address check.
//
// It runs after the resolver has answered and before connect(2), with the
// address the socket is about to be connected to. That placement is what makes
// this a rebinding defence rather than a second URL check: there is no window
// between what is checked and what is dialled for a second DNS answer to arrive
// in, and it runs once per address, so a name answering with one public and one
// private record does not get in on the second attempt.
//
// The predicate is link.IsRestrictedAddr — the same function the unappealable
// tier uses, deliberately, because two definitions of "private address" in one
// program is a drift bug waiting for the day somebody adds a range to one of
// them.
func guardDial(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		// Nothing else should ever be dialled by an HTTP transport. Refused
		// rather than allowed, so a future change that introduces one has to
		// come through here.
		return fmt.Errorf("%w: network %q", ErrPrivateAddress, network)
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparseable address", ErrPrivateAddress)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Control is documented to receive a resolved address. One that will not
		// parse is a state nobody has described, and the safe answer to a state
		// nobody has described is no.
		return fmt.Errorf("%w: unresolved address", ErrPrivateAddress)
	}
	if link.IsRestrictedAddr(addr) {
		return ErrPrivateAddress
	}
	return nil
}

// Deliver POSTs one signed payload and reports what the receiver said.
//
// Returns the HTTP status code, or zero when there was no response at all — a
// refused connection, a timeout, or this instance declining to open the socket.
// A non-2xx status is an error carrying the code, so the caller records both.
func (c *Client) Deliver(ctx context.Context, url string, secret []byte, d Delivery) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(d.Payload))
	if err != nil {
		return 0, fmt.Errorf("build delivery request: %w", err)
	}

	timestamp := time.Now().UTC().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "LinkCtrl-Webhook")
	req.Header.Set(HeaderEvent, d.Event)
	req.Header.Set(HeaderDelivery, d.ID.String())
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	req.Header.Set(HeaderSignature, SignatureVersion+"="+Sign(secret, timestamp, d.Payload))

	resp, err := c.http.Do(req)
	if err != nil {
		// Unwrapped to the cause where there is one, so a refusal by guardDial
		// reads as the refusal rather than as `Post "https://...": ...`. That
		// also keeps the URL out of the message the delivery row stores.
		if errors.Is(err, ErrPrivateAddress) {
			return 0, ErrPrivateAddress
		}
		return 0, transportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Drained and discarded. Nothing in this product reads a webhook response
	// body, but a connection whose body was never read cannot be reused, and the
	// bound is what stops a hostile receiver making that read unbounded.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("receiver answered %s", resp.Status)
	}
	return resp.StatusCode, nil
}

// transportError strips the URL out of a transport failure.
//
// `*url.Error` prints as `Post "https://host/path": dial tcp ...`, which puts the
// registered URL into every message that reaches a log line or a stored row. The
// row belongs to the workspace that registered it, so this is defence in depth
// rather than the whole defence — but the same shape is an open finding against
// the reputation feed (F34), and repeating it deliberately would be strange.
func transportError(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}

// Sign produces the payload signature a receiver verifies.
//
// # The scheme, for whoever is writing a receiver
//
//	signed  = "<timestamp>.<raw request body>"
//	digest  = HMAC-SHA256(key = the secret exactly as it was shown to you,
//	                      message = signed)
//	header  = "X-LinkCtrl-Signature: v1=" + lowercase hex of digest
//
// Three things are worth stating because getting any of them wrong produces a
// signature that never matches and no way to tell why:
//
//   - **The key is the secret string as displayed**, the 64 lowercase hex
//     characters, used as-is. It is not hex-decoded first. This product stores 32
//     random bytes and shows you their hex; making the visible string the key
//     means a receiver copies it out of the dashboard and uses it, with no
//     encoding step to get wrong.
//   - **The message is the raw body**, byte for byte as it arrived. Do not parse
//     and re-serialize the JSON before verifying — key order and whitespace will
//     not survive it.
//   - **The timestamp is the header value**, seconds since the epoch, and it is
//     part of what is signed. A receiver that wants replay protection rejects a
//     timestamp too far from its own clock; a receiver that does not still has to
//     include it in the message.
//
// Compare with a constant-time comparison. hmac.Equal in Go, hash_equals in PHP,
// hmac.compare_digest in Python.
func Sign(secret []byte, timestamp int64, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(hex.EncodeToString(secret)))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
