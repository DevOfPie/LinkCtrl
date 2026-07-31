package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// These tests exist because the environment surface drifted in the direction
// nothing checks: the binary read sixty variables, the reference documented all
// of them, and the shipped compose file forwarded fifteen. Everything else was
// read from .env for interpolation and then dropped, so an operator could set a
// retention window, restart, and watch nothing change — with --check-config
// validating the same truncated environment and agreeing.
//
// Reading the repository's own files from a unit test is unusual, and it is the
// point: the claim "every variable listed here takes effect" is about files, so
// only a check over those files can hold it.

func repoRoot(t *testing.T) string {
	t.Helper()
	// internal/config -> repo root.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("could not locate the repository root from %s: %v", root, err)
	}
	return root
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// envNames is every variable Parse actually reads, with the prefix applied.
func envNames() []string {
	var names []string
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := range t.NumField() {
			f := t.Field(i)
			if tag, ok := f.Tag.Lookup("env"); ok {
				name, _, _ := strings.Cut(tag, ",")
				if name != "" {
					names = append(names, EnvPrefix+name)
				}
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type)
			}
		}
	}
	walk(reflect.TypeOf(Config{}))
	return names
}

// Every variable the binary reads must appear in .env.example, commented or
// not. .env.example is what an operator copies, so a variable missing from it
// is a knob nobody discovers.
func TestEveryVariableAppearsInEnvExample(t *testing.T) {
	example := readRepoFile(t, ".env.example")

	for _, name := range envNames() {
		if !regexp.MustCompile(`(?m)^#?\s*` + regexp.QuoteMeta(name) + `=`).MatchString(example) {
			t.Errorf("%s is read by Parse but is not in .env.example", name)
		}
	}
}

// The reverse direction: .env.example must not advertise a variable nothing
// reads. A documented knob that does nothing is worse than an undocumented one,
// because it is set with confidence and silently ignored.
func TestEnvExampleAdvertisesNothingUnread(t *testing.T) {
	example := readRepoFile(t, ".env.example")

	read := map[string]bool{}
	for _, name := range envNames() {
		read[name] = true
	}
	// Variables the compose file consumes rather than the binary. These are
	// named here so adding one is a deliberate act.
	for _, name := range []string{
		"LINKCTRL_TAG", "LINKCTRL_HTTP_PORT",
	} {
		read[name] = true
	}

	found := regexp.MustCompile(`(?m)^#?\s*(LINKCTRL_[A-Z0-9_]+)=`).FindAllStringSubmatch(example, -1)
	for _, m := range found {
		name := m[1]
		if strings.HasSuffix(name, "_FILE") {
			// The _FILE convention is handled before parsing, so it has no tag.
			continue
		}
		if _, removed := Removed[strings.TrimPrefix(name, EnvPrefix)]; removed {
			continue
		}
		if !read[name] {
			t.Errorf("%s is in .env.example but nothing reads it", name)
		}
	}
}

// TRUSTED_PROXIES is the most abuse-relevant value in the configuration and had
// no parse-level test at all. A non-empty list makes the app believe
// X-Forwarded-For, so both directions matter: an entry that silently fails to
// parse could leave a proxy untrusted (every visitor sharing one rate-limit
// bucket), and an entry parsed more broadly than written would let anything in
// the range claim any client address.
func TestTrustedProxiesParsing(t *testing.T) {
	tests := []struct {
		name  string
		value string
		// want is the prefixes, in order, as strings.
		want    []string
		wantErr bool
	}{
		{name: "unset is empty", value: "", want: nil},
		{name: "single v4 range", value: "172.16.0.0/12", want: []string{"172.16.0.0/12"}},
		{name: "single host", value: "10.0.0.5/32", want: []string{"10.0.0.5/32"}},
		{
			name:  "several, comma separated",
			value: "172.16.0.0/12,10.0.0.0/8,192.168.0.0/16",
			want:  []string{"172.16.0.0/12", "10.0.0.0/8", "192.168.0.0/16"},
		},
		{name: "ipv6", value: "fd00::/8", want: []string{"fd00::/8"}},
		{name: "mixed families", value: "10.0.0.0/8,fd00::/8", want: []string{"10.0.0.0/8", "fd00::/8"}},

		// A bare address is the mistake an operator is most likely to make, and
		// it must not be accepted silently as something else.
		{name: "bare address without a prefix", value: "10.0.0.5", wantErr: true},
		{name: "not an address at all", value: "proxy.internal", wantErr: true},
		{name: "one bad entry poisons the list", value: "10.0.0.0/8,nonsense", wantErr: true},
		{name: "prefix out of range", value: "10.0.0.0/33", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			env["LINKCTRL_TRUSTED_PROXIES"] = tt.value
			setEnv(t, env)

			c, err := Parse()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse accepted TRUSTED_PROXIES=%q; a value that does "+
						"not parse must be refused, not ignored", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse rejected TRUSTED_PROXIES=%q: %v", tt.value, err)
			}
			if len(c.TrustedProxies) != len(tt.want) {
				t.Fatalf("parsed %d prefixes from %q, want %d: %v",
					len(c.TrustedProxies), tt.value, len(tt.want), c.TrustedProxies)
			}
			for i, want := range tt.want {
				if got := c.TrustedProxies[i].String(); got != want {
					t.Errorf("prefix %d = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// The default has to be empty. With a non-empty list the app believes
// X-Forwarded-For, so defaulting to anything would make a fresh instance
// spoofable out of the box.
func TestTrustedProxiesDefaultsToEmpty(t *testing.T) {
	setEnv(t, validEnv())

	c, err := Parse()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies defaults to %v, want empty: a default that "+
			"trusts anything makes X-Forwarded-For forgeable on a fresh instance",
			c.TrustedProxies)
	}
}

// The compose file must not forward a hand-picked subset. env_file is what
// makes the documented deployment path able to deliver the documented
// configuration surface; an explicit `environment:` block is only for the
// values compose itself owns.
func TestComposeForwardsTheWholeEnvironment(t *testing.T) {
	compose := readRepoFile(t, "docker-compose.yml")

	if !strings.Contains(compose, "env_file:") {
		t.Fatal("docker-compose.yml has no env_file, so only the variables named " +
			"in its environment block reach the container — most of what " +
			"docs/configuration.md documents would silently not take effect")
	}

	// The explicit block is allowed to pin what compose owns: the addresses of
	// the other services, the ports inside the container, and the secrets it
	// requires up front. Anything else pinned there is a variable an operator
	// cannot override from .env, which is how the surface shrank last time.
	allowed := map[string]bool{
		"LINKCTRL_APP_ENV": true, "LINKCTRL_BASE_URL": true,
		"LINKCTRL_HTTP_ADDR": true, "LINKCTRL_METRICS_ADDR": true,
		"LINKCTRL_API_KEY_PEPPER": true,
		"LINKCTRL_DATABASE_URL":   true, "LINKCTRL_REDIS_URL": true,
		"LINKCTRL_LOG_LEVEL": true, "LINKCTRL_LOG_FORMAT": true,
		"LINKCTRL_SECURE_COOKIES": true, "LINKCTRL_SIGNUP_MODE": true,
		"LINKCTRL_TRUSTED_PROXIES": true, "LINKCTRL_MIGRATE_ON_START": true,
		"LINKCTRL_GEOIP_MMDB_PATH": true,
	}
	for _, m := range regexp.MustCompile(`(?m)^\s+(LINKCTRL_[A-Z0-9_]+):`).FindAllStringSubmatch(compose, -1) {
		if !allowed[m[1]] {
			t.Errorf("%s is pinned in the compose environment block; an operator "+
				"cannot override it from .env. Either add it to the allowed set "+
				"deliberately or let env_file carry it", m[1])
		}
	}
}
