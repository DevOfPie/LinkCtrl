// Package feed asks a third party whether a destination is malicious.
//
// It is the one place in this program that sends a user's destination off the
// box, and everything about it is shaped by that. Read the four properties
// below before changing anything here; each is a promise made somewhere an
// operator can read it, and three of them are asserted by tests that will fail
// rather than explain themselves.
//
// **It does not exist unless an operator names a feed.** New returns a nil
// Client when LINKCTRL_FEED_URL is empty, and a nil Client is not a disabled
// client — it is no client at all, held in a nil interface by
// internal/link.Service, with no branch anywhere that could be got wrong. The
// default instance therefore has no code path that reaches the network with a
// destination in it, which is what "zero destination URLs leave the instance"
// means when it is asserted rather than promised.
//
// **It answers a question and never an instruction.** Check reports Malicious
// or not. It cannot say "allow this", it has no way to name a tier, and
// internal/link stamps every verdict it produces TierLowConfidence — the same
// confinement the heuristics have, for the same reason. A feed that could
// promote its own answer would be a third party writing into a tier this
// product tells operators costs a rebuild to overrule.
//
// **A failure is not a refusal.** Every error path — a timeout, a 500, a
// response that will not parse, a body that is too long — returns
// ResultError and the caller carries on to accept the destination. The built-in
// tiers have already had their say by then, so failing open loses the feed's
// opinion and nothing else. The alternative is a third party's outage deciding
// that this instance may not create links, which is precisely the dependency
// the opt-in exists to keep out of the default deployment.
//
// **It sends the destination and nothing else.** No identity, no workspace, no
// address, no instance name. The disclosure page tells users their destinations
// are sent; anything beyond that would make that sentence untrue.
package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Result is what one check produced. Counted by label, so the vocabulary is
// fixed here rather than assembled at the call site.
type Result string

const (
	// ResultClean means the feed answered and did not object.
	ResultClean Result = "clean"
	// ResultMalicious means the feed answered and objected.
	ResultMalicious Result = "malicious"
	// ResultError means the feed did not answer usefully. The caller fails open.
	ResultError Result = "error"
)

// Methods a feed endpoint may be called with. Two, because a reputation API
// either takes the URL in the query string or in a JSON body, and a third
// option would be a compatibility matrix rather than a feature.
const (
	MethodGET  = "GET"
	MethodPOST = "POST"
)

// maxResponseBytes bounds what is read back.
//
// A reputation verdict is a small JSON object. The bound is not about memory —
// it is that this response is attacker-influenced in the case that matters:
// somebody who controls a destination can often control what a feed says about
// it, and a feed that can be made to stream forever would hold a link creation
// open for the whole request timeout.
const maxResponseBytes = 64 << 10

// Config is the generic HTTP adapter, as an operator configures it.
//
// One adapter and not a plugin system. Which feeds get first-class support is a
// product call nobody has made, and shipping a named integration would be
// choosing one; a generic POST-a-URL-get-a-boolean adapter covers the shape
// every reputation API has and commits this product to none of them.
type Config struct {
	// Name is the third party, in words, for the disclosure. Required whenever
	// URL is set: a page that says "your destinations are sent to a third party"
	// without saying which one is not a disclosure.
	Name string
	// URL is the endpoint, and the switch. Empty means no feed.
	URL string
	// Method is GET or POST.
	Method string
	// Param names the field carrying the destination — a query parameter on GET,
	// a JSON object key on POST.
	Param string
	// AuthHeader and AuthToken authenticate. Both or neither; the header is only
	// set when the token is non-empty.
	AuthHeader string
	AuthToken  string
	// VerdictField is the dotted path into the response JSON holding the
	// verdict. "data.malicious" reads {"data":{"malicious":true}}.
	VerdictField string
	// Timeout bounds one check end to end. It is spent inside a link creation
	// somebody is waiting on, so it is small and it is enforced here as well as
	// by the caller's context.
	Timeout time.Duration
	// Transport is for tests. Nil uses a transport built here, which is the only
	// one production ever has.
	Transport http.RoundTripper
}

// Client checks destinations against one configured feed.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client, or returns nil when no feed is configured.
//
// A nil *Client with a nil error is the ordinary result on a default instance,
// and callers store it in an interface that they then test for nil. It is not
// an error state and it is not logged as one.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, nil
	}
	if cfg.Name == "" {
		return nil, errors.New("feed: a configured feed must be named")
	}
	if cfg.Method != MethodGET && cfg.Method != MethodPOST {
		return nil, fmt.Errorf("feed: method must be %s or %s, got %q",
			MethodGET, MethodPOST, cfg.Method)
	}
	if cfg.Param == "" || cfg.VerdictField == "" {
		return nil, errors.New("feed: the destination parameter and the verdict field are both required")
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("feed: timeout must be positive, got %s", cfg.Timeout)
	}

	rt := cfg.Transport
	if rt == nil {
		rt = &http.Transport{
			// Small and shared: one destination check per link write, on an
			// instance whose whole point is that most writes are people typing
			// into a form.
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		}
	}
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Transport: rt,
			Timeout:   cfg.Timeout,
			// Redirects are not followed. A feed that answers 302 is a feed
			// pointing this process somewhere nobody configured, and the whole
			// value of the opt-in is that the operator named the party their
			// users' destinations go to.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Name is the third party, for the disclosure. Safe on a nil client.
func (c *Client) Name() string {
	if c == nil {
		return ""
	}
	return c.cfg.Name
}

// Endpoint is where destinations are sent, for the disclosure. Safe on a nil
// client.
//
// The configured URL verbatim, minus anything after the path: a feed URL can
// carry an API key in its query string, and the disclosure page is shown to
// every signed-in user rather than to the operator alone.
func (c *Client) Endpoint() string {
	if c == nil {
		return ""
	}
	if i := strings.IndexAny(c.cfg.URL, "?#"); i >= 0 {
		return c.cfg.URL[:i]
	}
	return c.cfg.URL
}

// Disclosure is what an instance tells the people using it about this feature.
//
// It is built from the live client rather than from configuration, so the page
// cannot describe a feed the service is not actually using — a disclosure
// assembled from the environment would keep saying "on" for a client that failed
// to construct, which is the one direction it must never be wrong in.
//
// Every field is on the JSON API as well as the page. This is a statement about
// what happens to somebody's data; a person who reads it through a client is
// owed the same answer as one who reads it in a browser.
type Disclosure struct {
	// Enabled is false on a default instance, and false is the whole disclosure:
	// no destination leaves the box.
	Enabled bool `json:"enabled"`
	// Name is the third party. Empty when disabled.
	Name string `json:"third_party,omitempty"`
	// Endpoint is where destinations are sent, with any query string removed —
	// a feed URL often carries an API key in one, and this is shown to every
	// signed-in user rather than to the operator alone.
	Endpoint string `json:"endpoint,omitempty"`
	// Method is GET or POST.
	Method string `json:"method,omitempty"`
	// TimeoutSeconds is how long a check may take before it fails open.
	TimeoutSeconds float64 `json:"timeout_seconds,omitempty"`
}

// Describe reports what this instance does with destinations. Safe on a nil
// client, which is the default instance and reports Enabled false.
func (c *Client) Describe() Disclosure {
	if c == nil {
		return Disclosure{}
	}
	return Disclosure{
		Enabled:        true,
		Name:           c.cfg.Name,
		Endpoint:       c.Endpoint(),
		Method:         c.cfg.Method,
		TimeoutSeconds: c.cfg.Timeout.Seconds(),
	}
}

// Check asks the feed about one destination.
//
// The destination is the entire payload. Returns ResultError with the cause for
// anything that is not a usable answer, and the caller is required to treat
// that as "no opinion" rather than as a refusal — see the package comment.
func (c *Client) Check(ctx context.Context, destination string) (Result, error) {
	if c == nil {
		return ResultError, errors.New("feed: no feed is configured")
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := c.request(ctx, destination)
	if err != nil {
		return ResultError, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return ResultError, fmt.Errorf("feed %s: %w", c.cfg.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ResultError, fmt.Errorf("feed %s answered %s", c.cfg.Name, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return ResultError, fmt.Errorf("feed %s: read response: %w", c.cfg.Name, err)
	}
	if len(body) > maxResponseBytes {
		return ResultError, fmt.Errorf("feed %s: response exceeds %d bytes", c.cfg.Name, maxResponseBytes)
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return ResultError, fmt.Errorf("feed %s: response is not a JSON object: %w", c.cfg.Name, err)
	}

	raw, ok := lookup(doc, c.cfg.VerdictField)
	if !ok {
		return ResultError, fmt.Errorf("feed %s: response has no %q", c.cfg.Name, c.cfg.VerdictField)
	}
	malicious, err := truthy(raw)
	if err != nil {
		return ResultError, fmt.Errorf("feed %s: %q: %w", c.cfg.Name, c.cfg.VerdictField, err)
	}
	if malicious {
		return ResultMalicious, nil
	}
	return ResultClean, nil
}

// request builds the outbound call.
//
// Headers are set explicitly and there are only four. Nothing here carries the
// caller's identity, the instance's name or a cookie jar — the client is built
// without one — because the disclosure this feature ships says destinations are
// what leaves, and it has to stay true.
func (c *Client) request(ctx context.Context, destination string) (*http.Request, error) {
	var (
		req *http.Request
		err error
	)
	if c.cfg.Method == MethodGET {
		u := c.cfg.URL
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + queryEscape(c.cfg.Param) + "=" + queryEscape(destination)
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	} else {
		var payload []byte
		payload, err = json.Marshal(map[string]string{c.cfg.Param: destination})
		if err != nil {
			return nil, fmt.Errorf("feed %s: encode request: %w", c.cfg.Name, err)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(payload))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("feed %s: build request: %w", c.cfg.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "LinkCtrl")
	if c.cfg.AuthToken != "" {
		req.Header.Set(c.cfg.AuthHeader, c.cfg.AuthToken)
	}
	return req, nil
}

// queryEscape percent-encodes a query component.
//
// net/url would do this, and importing it here would put a `url.` symbol in a
// package whose entire job is to be the one place outbound HTTP is allowed —
// which is fine, except that the scans guarding the rest of this feature read
// for exactly that shape. Written out so the ban stays blunt everywhere else.
func queryEscape(s string) string {
	const hex = "0123456789ABCDEF"
	const safe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if strings.IndexByte(safe, ch) >= 0 {
			b.WriteByte(ch)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[ch>>4])
		b.WriteByte(hex[ch&0x0f])
	}
	return b.String()
}

// lookup walks a dotted path into a decoded JSON object.
//
// Dotted rather than JSONPath because the shape being read is a verdict, which
// every reputation API puts one or two levels down. A path expression language
// would be a dependency and a way to configure a bug.
func lookup(doc map[string]any, path string) (any, bool) {
	var cur any = doc
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// truthy reads a verdict out of whatever JSON type the feed used for it.
//
// Three types are accepted and everything else is an error rather than a
// default, because the default that suggests itself — "anything unrecognized is
// clean" — turns a feed that changed its response shape into a feed that
// silently stopped working. An error at least gets counted and logged.
func truthy(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case float64:
		return t != 0, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "yes", "malicious", "blocked", "phishing", "malware":
			return true, nil
		case "false", "no", "clean", "ok", "harmless", "":
			return false, nil
		}
		if b, err := strconv.ParseBool(t); err == nil {
			return b, nil
		}
		return false, fmt.Errorf("%q is not a verdict this adapter understands", t)
	}
	return false, fmt.Errorf("verdict is %T, want a boolean, a number or a string", v)
}
