package config

import (
	"strings"
	"testing"
)

func TestCanonicalHostNormalizesForComparison(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"already canonical", "manage.example.com", "manage.example.com"},
		{"uppercase", "Manage.Example.COM", "manage.example.com"},
		{"surrounding space", "  manage.example.com ", "manage.example.com"},
		// The two sides of the comparison are written by different people: the
		// operator types the origin, a proxy decides whether to append the
		// default port. Neither choice may change which tree serves a request.
		{"default https port", "manage.example.com:443", "manage.example.com"},
		{"default http port", "manage.example.com:80", "manage.example.com"},
		// A non-default port is part of the identity — localhost:8080 and
		// localhost:9090 are different origins during development.
		{"non-default port kept", "localhost:8080", "localhost:8080"},
		{"ipv6 with default port", "[::1]:443", "[::1]"},
		{"ipv6 with non-default port", "[::1]:8080", "[::1]:8080"},
		{"bare ipv6", "[::1]", "[::1]"},
		{"empty", "", ""},
		// F72, F88. The fully qualified spelling names the same host, and
		// storage already folds it — `domain.ValidateHostname` drops the dot and
		// the unique index is on `lower(hostname)` — so a stored name can never
		// carry one and the mismatch was entirely request-side. Reachable over
		// HTTPS because SNI carries no trailing dot (RFC 6066).
		{"trailing dot", "lnk.example.com.", "lnk.example.com"},
		{"trailing dot and default port", "lnk.example.com.:443", "lnk.example.com"},
		// The half a lone TrimSuffix on the whole string would miss, because the
		// dot is not at the end of the string when a port follows it.
		{"trailing dot and non-default port", "lnk.example.com.:8080", "lnk.example.com:8080"},
		{"trailing dot and case", "LNK.Example.COM.", "lnk.example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalHost(tc.in); got != tc.want {
				t.Errorf("CanonicalHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// HostOnly answers the other question, and F88 is what happened while one
// function answered both: the verified-hostname cache was keyed through
// CanonicalHost, which keeps a non-default port, so a Host header carrying one
// missed a hostname this instance is verified to serve.
func TestHostOnlyDropsEveryPort(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no port", "go.customer.example", "go.customer.example"},
		{"default https port", "go.customer.example:443", "go.customer.example"},
		{"non-default port", "go.customer.example:8080", "go.customer.example"},
		{"trailing dot", "go.customer.example.", "go.customer.example"},
		{"trailing dot and non-default port", "go.customer.example.:8080", "go.customer.example"},
		{"uppercase and port", "GO.Customer.Example:8443", "go.customer.example"},
		{"ipv6 with non-default port", "[::1]:8080", "[::1]"},
		{"bare ipv6", "[::1]", "[::1]"},
		{"empty", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostOnly(tc.in); got != tc.want {
				t.Errorf("HostOnly(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The two must not diverge in the direction that fails silently: every host
// CanonicalHost matches, HostOnly must match too. The reverse is the point of
// having two functions — CanonicalHost keeps the port SplitHosts compares.
func TestHostOnlyIsWiderThanCanonicalHost(t *testing.T) {
	for _, in := range []string{
		"lnk.example.com", "LNK.example.com.", "lnk.example.com:443",
		"lnk.example.com:8080", "[::1]:443", "[::1]:8080", "",
	} {
		if got, want := HostOnly(in), HostOnly(CanonicalHost(in)); got != want {
			t.Errorf("HostOnly(%q) = %q but HostOnly(CanonicalHost(%q)) = %q; "+
				"a host one of them folds and the other does not is a host served "+
				"by one router and not the other", in, got, in, want)
		}
	}
}

func TestSplitHostsIsOffByDefault(t *testing.T) {
	setEnv(t, validEnv())

	c, err := Parse()
	if err != nil {
		t.Fatal(err)
	}
	if c.SplitHosts() {
		t.Error("SplitHosts is true with neither APP_BASE_URL nor LINK_BASE_URL set; " +
			"an existing single-host deployment must be unaffected by this feature existing")
	}
	// Both must still resolve to something usable, or every caller has to
	// re-implement the fallback.
	if c.AppOrigin() != c.BaseURL || c.LinkOrigin() != c.BaseURL {
		t.Errorf("origins did not default to BASE_URL: app=%q link=%q base=%q",
			c.AppOrigin(), c.LinkOrigin(), c.BaseURL)
	}
}

func TestSplitHostsDetectedWhenHostsDiffer(t *testing.T) {
	env := validEnv()
	env["LINKCTRL_APP_BASE_URL"] = "https://manage.example.com"
	env["LINKCTRL_LINK_BASE_URL"] = "https://lnk.example.com"
	setEnv(t, env)

	c, err := Parse()
	if err != nil {
		t.Fatal(err)
	}
	if !c.SplitHosts() {
		t.Fatal("SplitHosts is false with two different hostnames configured")
	}
	if c.AppOrigin() != "https://manage.example.com" {
		t.Errorf("AppOrigin = %q", c.AppOrigin())
	}
	if c.LinkOrigin() != "https://lnk.example.com" {
		t.Errorf("LinkOrigin = %q", c.LinkOrigin())
	}
	// Host() is what the redirect path resolves aliases against, so it has to
	// follow the link origin rather than the dashboard one.
	if c.Host() != "lnk.example.com" {
		t.Errorf("Host() = %q, want the link host", c.Host())
	}
}

// A port difference alone is not a split. The routing decision and the cookie
// boundary are both about the host, and two ports on one host share both.
func TestSplitHostsIgnoresPortAndSchemeOnTheSameHost(t *testing.T) {
	env := validEnv()
	env["LINKCTRL_APP_ENV"] = "development"
	env["LINKCTRL_SECURE_COOKIES"] = "false"
	env["LINKCTRL_BASE_URL"] = "http://links.example.com"
	env["LINKCTRL_APP_BASE_URL"] = "http://links.example.com:80"
	env["LINKCTRL_LINK_BASE_URL"] = "http://links.example.com"
	setEnv(t, env)

	c, err := Parse()
	if err != nil {
		t.Fatal(err)
	}
	if c.SplitHosts() {
		t.Error("SplitHosts is true for one host written two ways")
	}
}

func TestSplitOriginsValidatedLikeBaseURL(t *testing.T) {
	cases := []struct {
		name, key, value, wantIn string
	}{
		{"app scheme", "LINKCTRL_APP_BASE_URL", "ftp://manage.example.com", "APP_BASE_URL: scheme"},
		{"link scheme", "LINKCTRL_LINK_BASE_URL", "ftp://lnk.example.com", "LINK_BASE_URL: scheme"},
		{"app path", "LINKCTRL_APP_BASE_URL", "https://manage.example.com/admin", "APP_BASE_URL: must not include a path"},
		{"link query", "LINKCTRL_LINK_BASE_URL", "https://lnk.example.com?a=1", "LINK_BASE_URL: must not include a query"},
		{"app no host", "LINKCTRL_APP_BASE_URL", "https://", "APP_BASE_URL: must include a host"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			env[tc.key] = tc.value
			setEnv(t, env)

			_, err := Parse()
			if err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

// The production https rule covers every origin, not only BASE_URL. Missing one
// would let an operator serve the dashboard over plaintext by naming it in a
// different variable, and __Host- cookies would silently never persist.
func TestProductionRequiresHTTPSOnEveryOrigin(t *testing.T) {
	for _, key := range []string{"LINKCTRL_APP_BASE_URL", "LINKCTRL_LINK_BASE_URL"} {
		t.Run(key, func(t *testing.T) {
			env := validEnv()
			env["LINKCTRL_APP_ENV"] = "production"
			env[key] = "http://plain.example.com"
			setEnv(t, env)

			_, err := Parse()
			if err == nil {
				t.Fatalf("%s over http was accepted in production", key)
			}
			if !strings.Contains(err.Error(), "must use https in production") {
				t.Errorf("error %q does not explain the https requirement", err)
			}
		})
	}
}
