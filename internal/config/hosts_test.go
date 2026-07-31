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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalHost(tc.in); got != tc.want {
				t.Errorf("CanonicalHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
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
