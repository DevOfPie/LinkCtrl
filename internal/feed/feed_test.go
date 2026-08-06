package feed

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// enabled is a client pointed at a test server, with the defaults an operator
// gets from .env.example.
func enabled(t *testing.T, srv *httptest.Server, over func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		Name: "Example Reputation", URL: srv.URL, Method: MethodPOST,
		Param: "url", VerdictField: "blocked", Timeout: 2 * time.Second,
		Transport: srv.Client().Transport,
	}
	if over != nil {
		over(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatal("New returned no client for a configured feed")
	}
	return c
}

// asked is one request the feed received, captured while it is still readable.
type asked struct {
	method string
	query  url.Values
	header http.Header
	body   string
}

// answering serves one JSON body and records what it was asked.
func answering(t *testing.T, body string) (*httptest.Server, *[]asked) {
	t.Helper()
	var seen []asked
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, asked{
			method: r.Method, query: r.URL.Query(),
			header: r.Header.Clone(), body: string(b),
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// TestNoFeedConfiguredMeansNoClient is the first half of the milestone's
// no-feed-egress claim, at the only place it can be made structural.
//
// Off is not a flag on a client — it is the absence of one. New returns a nil
// *Client, callers store it in a nil interface, and internal/link's guard is
// `s.feed == nil`. There is therefore no branch anywhere that could be written
// the wrong way round and no object holding a URL that something might later
// call. The other half — that a nil checker means no destination reaches the
// feed — is asserted end to end in test/integration/feed_test.go.
//
// **Neither half is a claim that nothing leaves** (F136). The feed is one of the
// two channels /feeds discloses; the other is a workspace's own webhooks, which
// no operator setting reaches and which this package knows nothing about.
func TestNoFeedConfiguredMeansNoClient(t *testing.T) {
	for _, url := range []string{"", "   ", "\t\n"} {
		c, err := New(Config{URL: url, Name: "Someone", Method: MethodPOST,
			Param: "url", VerdictField: "blocked", Timeout: time.Second})
		if err != nil {
			t.Fatalf("New(%q): %v", url, err)
		}
		if c != nil {
			t.Fatalf("New(%q) built a client; an unconfigured feed must produce none", url)
		}
		// Every accessor is reached on the nil client, because the disclosure
		// page and the log line both call them on a default instance.
		if got := c.Describe(); got.Enabled {
			t.Errorf("a nil client describes itself as enabled: %+v", got)
		}
		if c.Name() != "" || c.Endpoint() != "" {
			t.Errorf("a nil client names a feed: %q %q", c.Name(), c.Endpoint())
		}
		if r, err := c.Check(t.Context(), "https://example.com/"); r != ResultError || err == nil {
			t.Errorf("Check on a nil client = %q, %v; want an error and no verdict", r, err)
		}
	}
}

// TestCheckSendsTheDestinationAndNothingElse is the disclosure, as a test.
//
// The page tells every signed-in user that the destination is what is sent. This
// is what stops that sentence quietly becoming untrue: the request body and the
// query string are read back and compared against the destination and nothing
// else, so a later "while we are here, send the workspace too" fails here rather
// than in somebody's privacy review.
func TestCheckSendsTheDestinationAndNothingElse(t *testing.T) {
	const dest = "https://example.com/a?b=c&d=e#f"

	t.Run("post", func(t *testing.T) {
		srv, seen := answering(t, `{"blocked":false}`)
		c := enabled(t, srv, nil)
		if r, err := c.Check(t.Context(), dest); err != nil || r != ResultClean {
			t.Fatalf("Check = %q, %v", r, err)
		}
		if len(*seen) != 1 {
			t.Fatalf("the feed was asked %d times, want 1", len(*seen))
		}
		req := (*seen)[0]
		if req.method != http.MethodPost {
			t.Errorf("method = %s", req.method)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(req.body), &body); err != nil {
			t.Fatalf("decode body %q: %v", req.body, err)
		}
		if len(body) != 1 || body["url"] != dest {
			t.Errorf("body = %#v, want exactly {\"url\": %q}", body, dest)
		}
		if len(req.query) != 0 {
			t.Errorf("query string = %v, want none", req.query)
		}
	})

	t.Run("get", func(t *testing.T) {
		srv, seen := answering(t, `{"blocked":false}`)
		c := enabled(t, srv, func(cfg *Config) { cfg.Method = MethodGET })
		if r, err := c.Check(t.Context(), dest); err != nil || r != ResultClean {
			t.Fatalf("Check = %q, %v", r, err)
		}
		req := (*seen)[0]
		if req.method != http.MethodGet {
			t.Errorf("method = %s", req.method)
		}
		if len(req.query) != 1 || req.query.Get("url") != dest {
			t.Errorf("query = %v, want exactly url=%q", req.query, dest)
		}
		if req.body != "" {
			t.Errorf("body = %q on a GET, want none", req.body)
		}
	})

	t.Run("no credential unless one is configured", func(t *testing.T) {
		srv, seen := answering(t, `{"blocked":false}`)
		c := enabled(t, srv, nil)
		if _, err := c.Check(t.Context(), dest); err != nil {
			t.Fatal(err)
		}
		if got := (*seen)[0].header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q on a feed with no token", got)
		}
	})

	t.Run("the operator's credential, when there is one", func(t *testing.T) {
		srv, seen := answering(t, `{"blocked":false}`)
		c := enabled(t, srv, func(cfg *Config) {
			cfg.AuthHeader = "X-Api-Key"
			cfg.AuthToken = "s3cret"
		})
		if _, err := c.Check(t.Context(), dest); err != nil {
			t.Fatal(err)
		}
		if got := (*seen)[0].header.Get("X-Api-Key"); got != "s3cret" {
			t.Errorf("X-Api-Key = %q", got)
		}
	})
}

// TestTheVerdictIsReadWhereItWasConfigured covers the shapes a reputation API
// actually answers in, and pins which of them mean "refuse".
func TestTheVerdictIsReadWhereItWasConfigured(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
		want  Result
	}{
		{"boolean true", `{"blocked":true}`, "blocked", ResultMalicious},
		{"boolean false", `{"blocked":false}`, "blocked", ResultClean},
		{"nested", `{"data":{"malicious":true}}`, "data.malicious", ResultMalicious},
		{"number", `{"score":1}`, "score", ResultMalicious},
		{"zero", `{"score":0}`, "score", ResultClean},
		{"word", `{"verdict":"phishing"}`, "verdict", ResultMalicious},
		{"other word", `{"verdict":"harmless"}`, "verdict", ResultClean},
		// A field the feed stopped sending is an error rather than a silent
		// clean, because a feed that quietly stopped working looks exactly like
		// a feed that never refuses anything.
		{"missing", `{"other":true}`, "blocked", ResultError},
		{"wrong type", `{"blocked":{"a":1}}`, "blocked", ResultError},
		{"unreadable word", `{"verdict":"probably?"}`, "verdict", ResultError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := answering(t, tc.body)
			c := enabled(t, srv, func(cfg *Config) { cfg.VerdictField = tc.field })
			got, err := c.Check(t.Context(), "https://example.com/")
			if got != tc.want {
				t.Errorf("Check = %q (%v), want %q", got, err, tc.want)
			}
			if (err != nil) != (tc.want == ResultError) {
				t.Errorf("error = %v with result %q", err, got)
			}
		})
	}
}

// TestAFeedThatMisbehavesIsAnErrorAndNeverARefusal is the fail-open promise at
// the adapter boundary.
//
// Every one of these is a way a third party can fail, and none of them may
// produce ResultMalicious — a feed that 500s, hangs, redirects or streams
// forever must not be able to refuse somebody's link. The caller's half of the
// promise (that ResultError lets the destination through) is asserted in
// internal/link and end to end in the integration suite.
func TestAFeedThatMisbehavesIsAnErrorAndNeverARefusal(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"rate limited": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		},
		"not json": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>maintenance</html>"))
		},
		"json but not an object": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`["blocked"]`))
		},
		"empty": func(w http.ResponseWriter, _ *http.Request) {},
		// A body that never ends would otherwise hold a link creation open for
		// the whole request timeout.
		"endless": func(w http.ResponseWriter, _ *http.Request) {
			chunk := strings.Repeat("a", 4096)
			for i := 0; i < 64; i++ {
				if _, err := w.Write([]byte(chunk)); err != nil {
					return
				}
			}
		},
		// A feed answering 302 is a feed pointing this process at a server the
		// operator never named, which is the whole thing the opt-in decides.
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example/check", http.StatusFound)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			c := enabled(t, srv, nil)
			got, err := c.Check(t.Context(), "https://example.com/")
			if got != ResultError {
				t.Errorf("Check = %q, want %q", got, ResultError)
			}
			if err == nil {
				t.Error("no error accompanied the failure")
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		block := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-block
		}))
		t.Cleanup(func() { close(block); srv.Close() })
		c := enabled(t, srv, func(cfg *Config) { cfg.Timeout = 50 * time.Millisecond })
		start := time.Now()
		got, err := c.Check(t.Context(), "https://example.com/")
		if got != ResultError || err == nil {
			t.Errorf("Check = %q, %v; want an error", got, err)
		}
		// The bound is the point: this is spent inside a form submission.
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("a hung feed held the check for %s", elapsed)
		}
	})
}

// TestDescribeNeverPrintsTheOperatorsCredential.
//
// The disclosure is shown to every signed-in account, and a feed URL commonly
// carries an API key. Printing it would turn a page about protecting users into
// the way an editor reads the operator's secret.
//
// The userinfo rows are finding F35. The first version of this redaction cut
// everything from the first "?" or "#", which is a denylist, and it returned
// `https://apikey:SECRET@api.feed.example/v1/check` unchanged — while Go's
// client turned that same userinfo into a Basic auth header, so the credential
// both worked and was published to every signed-in user. The rewrite builds the
// string from scheme, host and path instead of removing what it knows about,
// which is why the table below can be extended with a spelling nobody has
// thought of yet and still pass.
//
// What is deliberately *not* here: a case asserting the path is stripped. It is
// kept — see Endpoint — and a credential written into a path segment is the
// residue this fix does not reach, which config validation cannot detect either
// and docs/configuration.md warns about instead.
func TestDescribeNeverPrintsTheOperatorsCredential(t *testing.T) {
	const want = "https://api.feed.example/v1/check"
	cases := map[string]string{
		"query string":       "https://api.feed.example/v1/check?key=SUPERSECRET&x=1",
		"fragment":           "https://api.feed.example/v1/check#SUPERSECRET",
		"userinfo pair":      "https://apikey:SUPERSECRET@api.feed.example/v1/check",
		"userinfo bare":      "https://SUPERSECRET@api.feed.example/v1/check",
		"userinfo and query": "https://apikey:SUPERSECRET@api.feed.example/v1/check?key=SUPERSECRET#SUPERSECRET",
		"nothing to remove":  want,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := New(Config{
				Name: "Example", URL: raw, Method: MethodPOST,
				Param: "url", VerdictField: "blocked", Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			d := c.Describe()
			if !d.Enabled || d.Name != "Example" {
				t.Fatalf("Describe = %+v", d)
			}
			if strings.Contains(d.Endpoint, "SUPERSECRET") {
				t.Errorf("Endpoint = %q; the credential reached the disclosure", d.Endpoint)
			}
			if d.Endpoint != want {
				t.Errorf("Endpoint = %q, want %q", d.Endpoint, want)
			}
			// Describe reads Endpoint, and the /feeds template and the JSON API
			// both read Describe — so the two must not be able to disagree.
			if c.Endpoint() != d.Endpoint {
				t.Errorf("Endpoint() = %q but Describe reported %q", c.Endpoint(), d.Endpoint)
			}
		})
	}

	// A port is part of where destinations go and survives.
	c, err := New(Config{
		Name: "Example", URL: "https://apikey:SUPERSECRET@api.feed.example:8443/v1/check",
		Method: MethodPOST, Param: "url", VerdictField: "blocked", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Endpoint(); got != "https://api.feed.example:8443/v1/check" {
		t.Errorf("Endpoint = %q; the port is part of the endpoint, not part of "+
			"the credential", got)
	}
}

// failingTransport fails every request with a fixed cause.
//
// A stand-in for a refused connection, a DNS failure, a TLS error or the
// timeout, all of which reach http.Client as the same thing and all of which
// leave it wrapped in the *url.Error this test is about. Deterministic, and it
// keeps the host in the assertion below from being satisfied by the cause's own
// text rather than by the redaction.
type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

// TestATransportFailureNamesTheEndpointAndNotTheCredential is finding F34.
//
// internal/link logs this error at WARN on every feed failure, and `*url.Error`
// renders as `Post "<the whole configured URL>": <cause>` — so a routine
// timeout on a two-second budget put a live third-party API key into the log
// stream, which is where logs get shipped elsewhere and pasted into tickets.
//
// Both directions are asserted, because either one alone is the wrong fix.
// Dropping the URL would satisfy the secrecy half and leave an operator unable
// to tell which endpoint stopped answering; keeping it satisfies the debugging
// half and is the bug. What the message has to carry is the same string the
// disclosure page shows, and nothing else.
func TestATransportFailureNamesTheEndpointAndNotTheCredential(t *testing.T) {
	c, err := New(Config{
		Name:   "Example Reputation",
		URL:    "https://apiuser:SUPERSECRET123@feed.example.com/v1/check?apikey=SUPERSECRET123&mode=strict",
		Method: MethodGET, Param: "url", VerdictField: "blocked",
		Timeout:   time.Second,
		Transport: failingTransport{err: errors.New("connect: connection refused")},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The destination is asserted by its host rather than whole: on GET it goes
	// into the query percent-encoded, so comparing the raw string would pass
	// against a message that still carries it.
	const destHost = "someone-elses-destination.example"
	r, err := c.Check(t.Context(), "https://"+destHost+"/private")
	if err == nil || r != ResultError {
		t.Fatalf("Check = %q, %v; want an error and no verdict", r, err)
	}
	msg := err.Error()

	for _, secret := range []string{"SUPERSECRET123", "apiuser", "mode=strict"} {
		if strings.Contains(msg, secret) {
			t.Errorf("the error carries %q, and internal/link logs it at WARN:\n%s",
				secret, msg)
		}
	}
	// On GET the destination is a query parameter on the request URL, so the
	// same replacement takes it out. That is not a second fix — it is the same
	// one — but it is the reason the whole URL is replaced rather than filtered.
	if strings.Contains(msg, destHost) {
		t.Errorf("the error carries the user's destination:\n%s", msg)
	}

	// And the operator can still act on it: which endpoint, and what happened.
	if !strings.Contains(msg, "feed.example.com/v1/check") {
		t.Errorf("the error does not name the endpoint that failed, so nobody "+
			"can tell which one to look at:\n%s", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("the cause did not survive the redaction:\n%s", msg)
	}
	if !strings.Contains(msg, "Example Reputation") {
		t.Errorf("the feed is no longer named:\n%s", msg)
	}
	// Nothing can errors.As its way back to the URL: *url.Error is replaced
	// rather than wrapped, so there is no second route to the string above.
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Errorf("the *url.Error survived, carrying %q", ue.URL)
	}
}

// TestAMisconfiguredFeedIsRefusedRatherThanGuessedAt.
//
// Every one of these would otherwise produce an instance that sends destinations
// somewhere while believing it does something else. Refusing at construction
// means the process does not start, which is the correct blast radius for a
// setting whose failure mode is a privacy promise being wrong.
func TestAMisconfiguredFeedIsRefusedRatherThanGuessedAt(t *testing.T) {
	base := Config{
		Name: "Example", URL: "https://feed.example/check", Method: MethodPOST,
		Param: "url", VerdictField: "blocked", Timeout: time.Second,
	}
	cases := map[string]func(*Config){
		"no name":          func(c *Config) { c.Name = "" },
		"unknown method":   func(c *Config) { c.Method = "PUT" },
		"no param":         func(c *Config) { c.Param = "" },
		"no verdict field": func(c *Config) { c.VerdictField = "" },
		"no timeout":       func(c *Config) { c.Timeout = 0 },
		"negative timeout": func(c *Config) { c.Timeout = -time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			c, err := New(cfg)
			if err == nil {
				t.Fatalf("New accepted %s and returned %v", name, c)
			}
			if c != nil {
				t.Error("New returned a client alongside an error")
			}
		})
	}
}
