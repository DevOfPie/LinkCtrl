package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// validEnv is a minimal environment that must load cleanly. Tests mutate a
// copy of it to exercise one failure at a time.
func validEnv() map[string]string {
	return map[string]string{
		"LINKCTRL_APP_ENV":        "production",
		"LINKCTRL_BASE_URL":       "https://links.example.com",
		"LINKCTRL_API_KEY_PEPPER": strings.Repeat("p", 48),
		"LINKCTRL_DATABASE_URL":   "postgres://u:p@localhost:5432/linkctrl?sslmode=disable",
	}
}

// configTags collects the env variable name of every field on Config, nested
// structs included.
//
// Reflection rather than a hand-kept list, so "this variable no longer exists"
// is checked against the struct that actually drives parsing.
func configTags() string {
	var b strings.Builder
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := range t.NumField() {
			f := t.Field(i)
			if tag, ok := f.Tag.Lookup("env"); ok {
				name, _, _ := strings.Cut(tag, ",")
				b.WriteString(`env:"` + name + `" `)
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type)
			}
		}
	}
	walk(reflect.TypeOf(Config{}))
	return b.String()
}

func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	// Clear any LINKCTRL_ variable the host happens to have set, so a
	// developer's shell cannot change the outcome of these tests. t.Setenv
	// restores the previous value when the test finishes.
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, EnvPrefix) {
			t.Setenv(k, "")
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestParseValidConfig(t *testing.T) {
	setEnv(t, validEnv())

	c, err := Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if c.AppEnv != Production {
		t.Errorf("AppEnv = %q, want production", c.AppEnv)
	}
	if c.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080 (default not applied)", c.HTTP.Addr)
	}
	if c.Redirect.DefaultStatus != 302 {
		t.Errorf("Redirect.DefaultStatus = %d, want 302", c.Redirect.DefaultStatus)
	}
	if c.Auth.SignupMode != SignupClosed {
		t.Errorf("SignupMode = %q, want closed; a fresh instance must not accept "+
			"public registration by default", c.Auth.SignupMode)
	}
	if !c.SecureCookies {
		t.Error("SecureCookies defaulted to false")
	}
	if c.Host() != "links.example.com" {
		t.Errorf("Host() = %q, want links.example.com", c.Host())
	}
}

// A removed variable must be a warning, never an error. An upgrade that refuses
// to boot because a stale line survived in someone's .env is a worse outcome
// than the knob it is complaining about.
func TestRemovedVariablesWarnButDoNotFailParsing(t *testing.T) {
	env := validEnv()
	env["LINKCTRL_INGEST_WORKERS"] = "4"
	env["LINKCTRL_BOT_FILTER_ENABLED"] = "false"
	setEnv(t, env)

	if _, err := Parse(); err != nil {
		t.Fatalf("Parse rejected a config carrying removed variables: %v", err)
	}

	warnings := RemovedInUse()
	if len(warnings) != 2 {
		t.Fatalf("RemovedInUse() = %v, want one warning per set variable", warnings)
	}
	for _, w := range warnings {
		// The message has to name the variable and say what is fixed instead,
		// or the reader learns only that something they set is being ignored.
		if !strings.Contains(w, "no longer read") {
			t.Errorf("warning does not explain itself: %q", w)
		}
	}
	if !strings.Contains(warnings[0], "BOT_FILTER_ENABLED") {
		t.Errorf("warnings are not sorted: %v", warnings)
	}
}

func TestRemovedVariablesAreSilentWhenUnset(t *testing.T) {
	setEnv(t, validEnv())
	if got := RemovedInUse(); len(got) != 0 {
		t.Errorf("RemovedInUse() = %v, want none", got)
	}
}

// Every entry in Removed must be absent from the parsed struct, or the map would
// be claiming a variable is gone while the code still reads it.
func TestRemovedVariablesHaveNoRemainingTag(t *testing.T) {
	for name := range Removed {
		if strings.Contains(configTags(), `env:"`+name+`"`) {
			t.Errorf("%s is listed as removed but still has an env tag", name)
		}
	}
}

func TestParseTrimsTrailingSlashFromBaseURL(t *testing.T) {
	env := validEnv()
	env["LINKCTRL_BASE_URL"] = "https://links.example.com/"
	setEnv(t, env)

	c, err := Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.BaseURL != "https://links.example.com" {
		t.Errorf("BaseURL = %q, want the trailing slash removed", c.BaseURL)
	}
}

// TestValidateReportsEveryProblemAtOnce is the behaviour that matters most in
// this package. An operator bringing up a first instance should see all their
// mistakes in one run rather than one per restart.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	setEnv(t, map[string]string{
		"LINKCTRL_APP_ENV":                 "production",
		"LINKCTRL_BASE_URL":                "http://links.example.com/some/path",
		"LINKCTRL_API_KEY_PEPPER":          "alsotooshort",
		"LINKCTRL_DATABASE_URL":            "postgres://localhost/db",
		"LINKCTRL_LOG_LEVEL":               "verbose",
		"LINKCTRL_SIGNUP_MODE":             "everyone",
		"LINKCTRL_ARGON2_MEMORY_KIB":       "1024",
		"LINKCTRL_REDIRECT_DEFAULT_STATUS": "301",
		"LINKCTRL_REDIRECT_NEGATIVE_TTL":   "1h",
		"LINKCTRL_SECURE_COOKIES":          "false",
	})

	_, err := Parse()
	if err == nil {
		t.Fatal("Parse succeeded on a thoroughly broken environment")
	}
	msg := err.Error()

	// Each of these is an independent problem; all must be reported together.
	wantMentions := []string{
		"BASE_URL",
		"API_KEY_PEPPER",
		"LOG_LEVEL",
		"SIGNUP_MODE",
		"ARGON2_MEMORY_KIB",
		"REDIRECT_DEFAULT_STATUS",
		"REDIRECT_NEGATIVE_TTL",
		"SECURE_COOKIES",
	}
	for _, want := range wantMentions {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %s; aggregated validation is not working.\nGot:\n%s",
				want, msg)
		}
	}

	if n := strings.Count(msg, "\n") + 1; n < len(wantMentions) {
		t.Errorf("got %d error lines, want at least %d", n, len(wantMentions))
	}
}

func TestValidateIndividualRules(t *testing.T) {
	// The rows that turn add-ons on need a directory that exists, because ADDONS_DIR
	// is stat'd. Which rows those are is the point of the comment beside them.
	addonsDir := t.TempDir()
	tests := []struct {
		name        string
		mutate      map[string]string
		wantMention string
	}{
		{"missing base url", map[string]string{"LINKCTRL_BASE_URL": ""}, "BASE_URL"},
		{"base url with path", map[string]string{"LINKCTRL_BASE_URL": "https://x.example.com/app"}, "must not include a path"},
		{"base url bad scheme", map[string]string{"LINKCTRL_BASE_URL": "ftp://x.example.com"}, "scheme must be http or https"},
		{"base url no host", map[string]string{"LINKCTRL_BASE_URL": "https://"}, "must include a host"},
		{"base url with query", map[string]string{"LINKCTRL_BASE_URL": "https://x.example.com?a=1"}, "query or fragment"},

		{"short pepper", map[string]string{"LINKCTRL_API_KEY_PEPPER": "abc"}, "at least 32 bytes"},

		{"http in production", map[string]string{"LINKCTRL_BASE_URL": "http://x.example.com"}, "__Host-"},
		{"insecure cookies in production", map[string]string{"LINKCTRL_SECURE_COOKIES": "false"}, "SECURE_COOKIES"},

		{"bad app env", map[string]string{"LINKCTRL_APP_ENV": "staging"}, "APP_ENV"},
		{"bad log level", map[string]string{"LINKCTRL_LOG_LEVEL": "trace"}, "LOG_LEVEL"},
		{"bad log format", map[string]string{"LINKCTRL_LOG_FORMAT": "xml"}, "LOG_FORMAT"},
		{"bad signup mode", map[string]string{"LINKCTRL_SIGNUP_MODE": "public"}, "SIGNUP_MODE"},

		{"argon2 memory below RFC floor", map[string]string{"LINKCTRL_ARGON2_MEMORY_KIB": "8192"}, "RFC 9106"},
		{"argon2 iterations too low", map[string]string{"LINKCTRL_ARGON2_ITERATIONS": "1"}, "ARGON2_ITERATIONS"},
		{"idle ttl exceeds absolute", map[string]string{
			"LINKCTRL_SESSION_IDLE_TTL": "1000h", "LINKCTRL_SESSION_ABSOLUTE_TTL": "24h",
		}, "SESSION_IDLE_TTL"},

		{"permanent redirect 301", map[string]string{"LINKCTRL_REDIRECT_DEFAULT_STATUS": "301"}, "permanent redirect"},
		{"permanent redirect 308", map[string]string{"LINKCTRL_REDIRECT_DEFAULT_STATUS": "308"}, "permanent redirect"},
		{"nonsense redirect status", map[string]string{"LINKCTRL_REDIRECT_DEFAULT_STATUS": "200"}, "REDIRECT_DEFAULT_STATUS"},
		{"long negative ttl", map[string]string{"LINKCTRL_REDIRECT_NEGATIVE_TTL": "30m"}, "appear broken"},

		{"min conns above max", map[string]string{
			"LINKCTRL_DB_MIN_CONNS": "50", "LINKCTRL_DB_MAX_CONNS": "10",
		}, "DB_MIN_CONNS"},
		{"pools exceed postgres default", map[string]string{
			"LINKCTRL_DB_MAX_CONNS": "100", "LINKCTRL_DB_REDIRECT_MAX_CONNS": "20",
		}, "max_connections"},
		{"redirect pool zero", map[string]string{"LINKCTRL_DB_REDIRECT_MAX_CONNS": "0"}, "DB_REDIRECT_MAX_CONNS"},

		{"addons dir that is not there", map[string]string{
			"LINKCTRL_ADDONS_DIR": "/nonexistent/addons",
		}, "ADDONS_DIR"},

		// The three add-on egress bounds. The two durations have to nest or the
		// outer one is a knob that does nothing — M68.5 shipped a route deadline
		// equal to the request timeout and it never fired once, and these rows are
		// what makes that a failing test rather than something a reader has to
		// notice. The byte cap has no nesting to assert and one thing to refuse:
		// zero, which an operator writes meaning *no cap* and which silently became
		// the 256 KiB default until the third review of this milestone found the
		// only one of the three with nothing said about it.
		//
		// The two cross-checks against HTTP_REQUEST_TIMEOUT set ADDONS_DIR as well,
		// and that is not scaffolding: the rule is guarded on add-ons being enabled,
		// because with the guard off a deployment running none of this refused to
		// start over the *default* route deadline the moment its operator had set a
		// short HTTP_REQUEST_TIMEOUT. The row below this pair holds that.
		{"route deadline equal to the request timeout", map[string]string{
			"LINKCTRL_ADDONS_DIR":           addonsDir,
			"LINKCTRL_ADDON_ROUTE_DEADLINE": "15s",
		}, "ADDON_ROUTE_DEADLINE"},
		{"route deadline over the request timeout", map[string]string{
			"LINKCTRL_ADDONS_DIR":           addonsDir,
			"LINKCTRL_ADDON_ROUTE_DEADLINE": "60s",
		}, "HTTP_REQUEST_TIMEOUT"},
		{"route deadline of zero", map[string]string{
			"LINKCTRL_ADDON_ROUTE_DEADLINE": "0s",
		}, "must be positive"},
		{"fetch timeout over the route deadline", map[string]string{
			"LINKCTRL_ADDON_FETCH_TIMEOUT": "11s",
		}, "ADDON_FETCH_TIMEOUT"},
		{"fetch timeout of zero", map[string]string{
			"LINKCTRL_ADDON_FETCH_TIMEOUT": "0s",
		}, "must be positive"},
		{"fetch cap of zero, which an operator writes meaning no cap", map[string]string{
			"LINKCTRL_ADDON_FETCH_MAX_BYTES": "0",
		}, "ADDON_FETCH_MAX_BYTES"},
		{"negative fetch cap", map[string]string{
			"LINKCTRL_ADDON_FETCH_MAX_BYTES": "-1",
		}, "must be positive"},

		{"batch exceeds queue", map[string]string{
			"LINKCTRL_INGEST_BATCH_SIZE": "100", "LINKCTRL_INGEST_QUEUE_SIZE": "10",
		}, "INGEST_BATCH_SIZE"},
		// Zero disables a rate limit, so only a negative value is a mistake —
		// and it is one worth naming, because -1 reads like "unlimited".
		{"negative login rate", map[string]string{"LINKCTRL_LOGIN_RATE_PER_MIN": "-1"}, "LOGIN_RATE_PER_MIN"},
		{"negative api rate", map[string]string{"LINKCTRL_API_RATE_PER_MIN": "-5"}, "API_RATE_PER_MIN"},
		{"negative 404 limit", map[string]string{"LINKCTRL_REDIRECT_404_RATE_LIMIT": "-1"}, "REDIRECT_404_RATE_LIMIT"},

		{"negative retention", map[string]string{"LINKCTRL_ANALYTICS_RETENTION_DAYS": "-1"}, "ANALYTICS_RETENTION_DAYS"},
		// A negative audit window reads as "unlimited" and would silently be
		// treated as "keep forever", which is the same behaviour as 0 by luck
		// rather than by contract. The operator who typed it meant something.
		{"negative audit retention", map[string]string{"LINKCTRL_AUDIT_RETENTION_DAYS": "-1"}, "AUDIT_RETENTION_DAYS"},
		{"missing geoip file", map[string]string{"LINKCTRL_GEOIP_MMDB_PATH": "/nope/missing.mmdb"}, "GEOIP_MMDB_PATH"},

		{"alias too short", map[string]string{"LINKCTRL_ALIAS_LENGTH": "2"}, "ALIAS_LENGTH"},
		{"alias too long", map[string]string{"LINKCTRL_ALIAS_LENGTH": "40"}, "ALIAS_LENGTH"},

		{"dangerous destination scheme", map[string]string{
			"LINKCTRL_DESTINATION_SCHEMES": "http,https,javascript",
		}, "javascript"},

		{"shutdown exceeds grace period", map[string]string{
			"LINKCTRL_SHUTDOWN_DRAIN_DELAY": "20s", "LINKCTRL_SHUTDOWN_TIMEOUT": "20s",
		}, "stop_grace_period"},

		// The mailer. Every one of these is only reachable once SMTP_HOST is
		// set, which is what TestMailerIsOffAndSilentByDefault holds the other
		// side of.
		{"mailer with no sender", map[string]string{
			"LINKCTRL_SMTP_HOST": "smtp.example.com",
		}, "SMTP_FROM"},
		{"mailer with an unparseable sender", map[string]string{
			"LINKCTRL_SMTP_HOST": "smtp.example.com", "LINKCTRL_SMTP_FROM": "not an address",
		}, "SMTP_FROM"},
		{"unknown tls mode", map[string]string{
			"LINKCTRL_SMTP_HOST": "smtp.example.com", "LINKCTRL_SMTP_FROM": "l@example.com",
			"LINKCTRL_SMTP_TLS": "ssl",
		}, "SMTP_TLS"},
		{"port out of range", map[string]string{
			"LINKCTRL_SMTP_HOST": "smtp.example.com", "LINKCTRL_SMTP_FROM": "l@example.com",
			"LINKCTRL_SMTP_PORT": "70000",
		}, "SMTP_PORT"},
		{"a username with no password", map[string]string{
			"LINKCTRL_SMTP_HOST": "smtp.example.com", "LINKCTRL_SMTP_FROM": "l@example.com",
			"LINKCTRL_SMTP_USERNAME": "postmaster",
		}, "set both or neither"},
		{"a password with no username", map[string]string{
			"LINKCTRL_SMTP_HOST": "smtp.example.com", "LINKCTRL_SMTP_FROM": "l@example.com",
			"LINKCTRL_SMTP_PASSWORD": "hunter2",
		}, "set both or neither"},
		// Credentials over an unencrypted connection are refused rather than
		// warned about: Go's own SMTP client refuses PLAIN in clear too, so
		// accepting it here would only move the failure to the first send.
		{"credentials without encryption", map[string]string{
			"LINKCTRL_SMTP_HOST": "smtp.example.com", "LINKCTRL_SMTP_FROM": "l@example.com",
			"LINKCTRL_SMTP_USERNAME": "postmaster", "LINKCTRL_SMTP_PASSWORD": "hunter2",
			"LINKCTRL_SMTP_TLS": "none",
		}, "in clear"},
		{"zero timeout", map[string]string{
			"LINKCTRL_SMTP_HOST": "smtp.example.com", "LINKCTRL_SMTP_FROM": "l@example.com",
			"LINKCTRL_SMTP_TIMEOUT": "0s",
		}, "SMTP_TIMEOUT"},

		// The feed. Refusing a credential written into FEED_URL is finding F35,
		// and it is a refusal rather than a redaction on purpose: Go sends
		// userinfo as Basic auth, so it works, and /feeds is shown to every
		// signed-in user by design (D40). Both spellings, because the one with
		// no password is the one a "just put the key in the URL" reading of an
		// API's documentation produces.
		{"a credential in the feed url", map[string]string{
			"LINKCTRL_FEED_URL":  "https://apikey:SUPERSECRET@feed.example.com/v1/check",
			"LINKCTRL_FEED_NAME": "Example Reputation",
		}, "must not carry a username or password"},
		{"a bare username in the feed url", map[string]string{
			"LINKCTRL_FEED_URL":  "https://SUPERSECRET@feed.example.com/v1/check",
			"LINKCTRL_FEED_NAME": "Example Reputation",
		}, "must not carry a username or password"},
		// The pre-existing rule, kept beside it: the two checks now share a
		// branch, and a refactor that drops one would otherwise be invisible.
		{"a feed url in clear", map[string]string{
			"LINKCTRL_FEED_URL":  "http://feed.example.com/v1/check",
			"LINKCTRL_FEED_NAME": "Example Reputation",
		}, "must use https"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			for k, v := range tc.mutate {
				env[k] = v
			}
			setEnv(t, env)

			_, err := Parse()
			if err == nil {
				t.Fatalf("Parse succeeded, want an error mentioning %q", tc.wantMention)
			}
			if !strings.Contains(err.Error(), tc.wantMention) {
				t.Errorf("error does not mention %q.\nGot: %v", tc.wantMention, err)
			}
		})
	}
}

// M26's headline claim, at the configuration layer: an instance that sets
// nothing has no mailer, and none of the mailer's rules can refuse its boot.
//
// The second half matters as much as the first. Every SMTP rule sits inside the
// Enabled guard, so a default instance must parse cleanly even with values that
// would be refused outright once a host is named — otherwise "off by default"
// would mean "off, unless you left something in your .env".
func TestMailerIsOffAndSilentByDefault(t *testing.T) {
	setEnv(t, validEnv())

	c, err := Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.SMTP.Enabled() {
		t.Error("SMTP is enabled on a configuration that names no host")
	}

	// Values that are errors once a host is set, on an instance with no host.
	env := validEnv()
	env["LINKCTRL_SMTP_TLS"] = "ssl"
	env["LINKCTRL_SMTP_FROM"] = "not an address"
	env["LINKCTRL_SMTP_USERNAME"] = "postmaster"
	setEnv(t, env)

	c, err = Parse()
	if err != nil {
		t.Fatalf("mailer settings refused a boot on an instance with no SMTP_HOST: %v", err)
	}
	if c.SMTP.Enabled() {
		t.Error("SMTP is enabled without a host")
	}
}

// The whole supported surface, accepted. A configuration that a self-hoster
// would actually write must parse, or every rule above is only proving that the
// mailer is hard to switch on.
func TestMailerAcceptsEachSupportedMode(t *testing.T) {
	for _, mode := range []string{SMTPStartTLS, SMTPImplicit} {
		t.Run(mode, func(t *testing.T) {
			env := validEnv()
			env["LINKCTRL_SMTP_HOST"] = "smtp.example.com"
			env["LINKCTRL_SMTP_PORT"] = "465"
			env["LINKCTRL_SMTP_FROM"] = "LinkCtrl <links@example.com>"
			env["LINKCTRL_SMTP_USERNAME"] = "postmaster"
			env["LINKCTRL_SMTP_PASSWORD"] = "hunter2"
			env["LINKCTRL_SMTP_TLS"] = mode
			setEnv(t, env)

			c, err := Parse()
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !c.SMTP.Enabled() {
				t.Fatal("SMTP is not enabled with a host set")
			}
			if got := c.SMTP.Addr(); got != "smtp.example.com:465" {
				t.Errorf("Addr() = %q", got)
			}
			// The password is a Secret, so a config dump or a formatted panic
			// cannot print it. Same treatment as the database DSN and the
			// API-key pepper, and worth asserting rather than assuming.
			dumped := fmt.Sprintf("%v %#v", c.SMTP, c.SMTP) +
				fmt.Sprintf(" %v %q %#v", c.SMTP.Password, c.SMTP.Password, c.SMTP.Password)
			if strings.Contains(dumped, "hunter2") {
				t.Error("the SMTP password printed itself")
			}
			if c.SMTP.Password.Reveal() != "hunter2" {
				t.Error("the SMTP password did not survive parsing")
			}
		})
	}

	// A relay that wants no encryption and no credentials — a local postfix, a
	// mailhog in development — is a legitimate configuration and must parse.
	env := validEnv()
	env["LINKCTRL_SMTP_HOST"] = "localhost"
	env["LINKCTRL_SMTP_PORT"] = "1025"
	env["LINKCTRL_SMTP_FROM"] = "links@example.com"
	env["LINKCTRL_SMTP_TLS"] = SMTPNone
	setEnv(t, env)
	if _, err := Parse(); err != nil {
		t.Errorf("Parse refused an unauthenticated local relay: %v", err)
	}
}

// SMTP_PASSWORD was in Removed through Phase 1, because there was no mail
// feature to authenticate to. M26 built one, so the variable is read again and
// the entry had to go — a warning that a variable does nothing, on an instance
// where it does something, is the exact defect the Removed list exists to
// prevent, arriving from the other side.
func TestSMTPPasswordIsNoLongerReportedAsRemoved(t *testing.T) {
	if _, ok := Removed["SMTP_PASSWORD"]; ok {
		t.Fatal("SMTP_PASSWORD is still listed as removed, but Parse reads it")
	}

	env := validEnv()
	env["LINKCTRL_SMTP_HOST"] = "smtp.example.com"
	env["LINKCTRL_SMTP_FROM"] = "links@example.com"
	env["LINKCTRL_SMTP_USERNAME"] = "postmaster"
	env["LINKCTRL_SMTP_PASSWORD"] = "hunter2"
	setEnv(t, env)

	if _, err := Parse(); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, w := range RemovedInUse() {
		if strings.Contains(w, "SMTP_PASSWORD") {
			t.Errorf("startup would warn that a variable it reads is unread: %q", w)
		}
	}
}

func TestDevelopmentAllowsInsecureSettings(t *testing.T) {
	// The production-only rules must not block local development over HTTP.
	env := validEnv()
	env["LINKCTRL_APP_ENV"] = "development"
	env["LINKCTRL_BASE_URL"] = "http://localhost:8080"
	env["LINKCTRL_SECURE_COOKIES"] = "false"
	setEnv(t, env)

	if _, err := Parse(); err != nil {
		t.Fatalf("Parse rejected a valid development config: %v", err)
	}
}

// --- _FILE secret convention ------------------------------------------------

func TestFileSecretsAreReadFromDisk(t *testing.T) {
	dir := t.TempDir()
	pepperPath := filepath.Join(dir, "pepper")
	want := strings.Repeat("f", 48)

	// Written with a trailing newline on purpose: `echo secret > file` adds
	// one, and if it is not trimmed the pepper silently differs from the
	// configured value and every API key stops verifying.
	if err := os.WriteFile(pepperPath, []byte(want+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := validEnv()
	delete(env, "LINKCTRL_API_KEY_PEPPER")
	env["LINKCTRL_API_KEY_PEPPER_FILE"] = pepperPath
	setEnv(t, env)

	c, err := Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.APIKeyPepper.Reveal(); got != want {
		t.Errorf("pepper = %q (len %d), want %q (len %d); trailing newline not trimmed?",
			got, len(got), want, len(want))
	}
}

func TestFileSecretRejectsBothFormsSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pepper")
	if err := os.WriteFile(path, []byte(strings.Repeat("f", 48)), 0o600); err != nil {
		t.Fatal(err)
	}

	env := validEnv()
	env["LINKCTRL_API_KEY_PEPPER_FILE"] = path
	setEnv(t, env)

	// Ambiguity must be an error rather than a silent precedence rule; an
	// operator who sets both has a wrong mental model of which one wins.
	_, err := Parse()
	if err == nil {
		t.Fatal("Parse succeeded with both the inline and _FILE forms set")
	}
	if !strings.Contains(err.Error(), "both set") {
		t.Errorf("error = %v, want it to explain the conflict", err)
	}
}

func TestFileSecretReportsUnreadableFile(t *testing.T) {
	env := validEnv()
	delete(env, "LINKCTRL_API_KEY_PEPPER")
	env["LINKCTRL_API_KEY_PEPPER_FILE"] = filepath.Join(t.TempDir(), "does-not-exist")
	setEnv(t, env)

	_, err := Parse()
	if err == nil {
		t.Fatal("Parse succeeded with an unreadable secret file")
	}
	if !strings.Contains(err.Error(), "API_KEY_PEPPER_FILE") {
		t.Errorf("error = %v, want it to name the variable", err)
	}
}

// --- secret redaction -------------------------------------------------------

func TestSecretNeverPrintsItself(t *testing.T) {
	const value = "super-secret-pepper-value"
	s := Secret(value)

	// Every route by which a secret could plausibly escape into a log line,
	// an error string, a crash dump or an API response.
	checks := map[string]string{
		"%v":     fmt.Sprintf("%v", s),
		"%s":     fmt.Sprintf("%s", s),
		"%q":     fmt.Sprintf("%q", s),
		"%#v":    fmt.Sprintf("%#v", s),
		"%+v":    fmt.Sprintf("%+v", s),
		"%d":     fmt.Sprintf("%d", s),
		"String": s.String(),
		"print":  fmt.Sprint(s),
	}
	for name, got := range checks {
		if strings.Contains(got, value) {
			t.Errorf("%s leaked the secret: %s", name, got)
		}
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("%s = %q, want it to contain REDACTED", name, got)
		}
	}

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), value) {
		t.Errorf("json.Marshal leaked the secret: %s", b)
	}

	if got := s.Reveal(); got != value {
		t.Errorf("Reveal() = %q, want the original value", got)
	}
}

func TestSecretRedactedInStructFormatting(t *testing.T) {
	// The realistic accident: formatting a whole config struct.
	env := validEnv()
	setEnv(t, env)
	c, err := Parse()
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"%v", "%+v", "%#v"} {
		out := fmt.Sprintf(format, c)
		for _, leaked := range []string{strings.Repeat("k", 48), strings.Repeat("p", 48), "postgres://u:p@"} {
			if strings.Contains(out, leaked) {
				t.Errorf("formatting the config with %s leaked a secret", format)
			}
		}
	}
}

func TestSecretRedactedInSlogOutput(t *testing.T) {
	const value = "another-secret-value-here"
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	logger.Info("startup", "pepper", Secret(value))

	if strings.Contains(buf.String(), value) {
		t.Errorf("slog leaked the secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "REDACTED") {
		t.Errorf("slog output missing REDACTED: %s", buf.String())
	}
}

func TestSecretHelpers(t *testing.T) {
	if !Secret("").IsZero() {
		t.Error("empty Secret should report IsZero")
	}
	if Secret("abc").IsZero() {
		t.Error("non-empty Secret should not report IsZero")
	}
	if got := Secret("abcde").Len(); got != 5 {
		t.Errorf("Len() = %d, want 5", got)
	}
}

// The audit window must default to 0, and 0 must mean keep forever.
//
// This is decision D5 expressed as a test rather than as prose. The failure it
// guards against is an upgrade that starts deleting audit history an operator
// assumed permanent — silent, irreversible, and discovered only when somebody
// goes looking for a record that is no longer there. A default borrowed from
// ANALYTICS_RETENTION_DAYS, or a "sensible" non-zero value added later, is
// exactly that failure.
func TestAuditRetentionDefaultsToKeepingEverything(t *testing.T) {
	setEnv(t, validEnv())

	c, err := Parse()
	if err != nil {
		t.Fatal(err)
	}
	if c.Audit.RetentionDays != 0 {
		t.Errorf("AUDIT_RETENTION_DAYS defaults to %d, want 0: an instance nobody "+
			"configured must never delete audit history (D5)", c.Audit.RetentionDays)
	}
	// And it is its own setting. Sharing the analytics window would delete the
	// audit trail on a number chosen for click events.
	if c.Analytics.RetentionDays == c.Audit.RetentionDays {
		t.Error("the audit and analytics windows have the same default; they are " +
			"separate policies and the whole point is that their defaults differ")
	}
}

// The claim M68.5's first attempt got wrong, asserted rather than commented.
//
// A route invocation runs under the application tree's request context, which
// httpx.RequestTimeout cancels at HTTP_REQUEST_TIMEOUT. Whichever of the two
// deadlines is earlier is the one that fires, and the earlier one starts first —
// so a route deadline that is not strictly shorter is a bound that never binds.
// The first attempt shipped both at fifteen seconds and the new knob did nothing;
// what makes that not recur is this test plus the two rules in Validate, because
// a comment saying "keep these ordered" is what was there before.
//
// The three-fetch relationship is asserted here too. It is the sizing argument for
// both defaults — discovery, token exchange, key set — and it was previously
// arithmetic in a doc comment against a budget that did not exist.
func TestTheAddonEgressBoundsNestInsideTheRequestDeadline(t *testing.T) {
	setEnv(t, validEnv())

	c, err := Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Addons.RouteDeadline >= c.HTTP.RequestTimeout {
		t.Errorf("ADDON_ROUTE_DEADLINE defaults to %s and HTTP_REQUEST_TIMEOUT to %s; "+
			"the request context is created first and cancels the same context, so a "+
			"route deadline at or over it never fires",
			c.Addons.RouteDeadline, c.HTTP.RequestTimeout)
	}
	if c.Addons.FetchTimeout > c.Addons.RouteDeadline {
		t.Errorf("ADDON_FETCH_TIMEOUT defaults to %s inside a route deadline of %s",
			c.Addons.FetchTimeout, c.Addons.RouteDeadline)
	}
	if got := 3 * c.Addons.FetchTimeout; got > c.Addons.RouteDeadline {
		t.Errorf("three fetches at the default timeout are %s against a route deadline "+
			"of %s; the defaults are documented as holding an authorization-code flow's "+
			"discovery, token exchange and key set", got, c.Addons.RouteDeadline)
	}
	// A deployment that has turned the request timeout off leaves the route
	// deadline as the only bound, so there is nothing above it to nest inside and
	// Validate must not invent one.
	env := validEnv()
	env["LINKCTRL_HTTP_REQUEST_TIMEOUT"] = "0s"
	env["LINKCTRL_ADDON_ROUTE_DEADLINE"] = "60s"
	setEnv(t, env)
	if _, err := Parse(); err != nil {
		t.Errorf("with HTTP_REQUEST_TIMEOUT disabled a 60s route deadline was refused: %v", err)
	}
}

// **The nesting rule may not refuse to start an instance that runs no add-ons.**
//
// LINKCTRL_HTTP_REQUEST_TIMEOUT=5s is valid today and nothing has ever refused it.
// M68.5's rule, written without the guard, then refused that instance at boot over
// the *default* value of ADDON_ROUTE_DEADLINE — a variable its operator had never
// set, on a deployment with ADDONS_DIR unset and no add-on anywhere. An upgrade
// that will not start is the most expensive failure this file has, and it was
// being spent on a subsystem the deployment does not use.
//
// The other half is the rule still doing its job: turn add-ons on with the same
// pair and the instance is refused, because the route deadline genuinely cannot
// fire. That is a real upgrade break and CHANGELOG.md carries it as one.
func TestTheNestingRuleIgnoresAnInstanceThatRunsNoAddons(t *testing.T) {
	env := validEnv()
	env["LINKCTRL_HTTP_REQUEST_TIMEOUT"] = "5s"
	setEnv(t, env)
	if _, err := Parse(); err != nil {
		t.Errorf("an instance with ADDONS_DIR unset and a 5s request timeout was "+
			"refused: %v. It runs no add-ons; the route deadline it is being held to "+
			"is a default nobody chose", err)
	}

	env["LINKCTRL_ADDONS_DIR"] = t.TempDir()
	setEnv(t, env)
	_, err := Parse()
	if err == nil {
		t.Fatal("with add-ons enabled a 10s route deadline inside a 5s request " +
			"timeout was accepted; the route deadline cannot fire and the knob is a " +
			"knob in name only")
	}
	// Both remedies, because the operator set the request timeout on purpose and
	// the other number is the one they have never seen.
	for _, want := range []string{"ADDON_ROUTE_DEADLINE", "HTTP_REQUEST_TIMEOUT", "Lower"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// Unset is the shipped state and has to stay cheap to express: no host, and no
// validation of a path nobody gave.
func TestAddonsAreOffByDefault(t *testing.T) {
	setEnv(t, validEnv())

	c, err := Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Addons.Enabled() {
		t.Error("add-ons are enabled with LINKCTRL_ADDONS_DIR unset")
	}
	if c.Addons.Dir != "" {
		t.Errorf("ADDONS_DIR defaulted to %q", c.Addons.Dir)
	}
}

// A file where a directory belongs is the same operator mistake with a different
// spelling, and it has to be refused at parse time rather than met at boot: the
// operator believes this instance is running modules it has never seen.
func TestAddonsDirMustBeADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "addon.wasm")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := validEnv()
	env["LINKCTRL_ADDONS_DIR"] = file
	setEnv(t, env)

	_, err := Parse()
	if err == nil {
		t.Fatal("a file was accepted as the add-ons directory")
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("unexpected error: %v", err)
	}
}

// And a real directory loads, so the rule above is refusing the mistake rather
// than the feature.
func TestAddonsDirAcceptsADirectory(t *testing.T) {
	env := validEnv()
	dir := t.TempDir()
	env["LINKCTRL_ADDONS_DIR"] = dir
	setEnv(t, env)

	c, err := Parse()
	if err != nil {
		t.Fatalf("Parse with a real add-ons directory: %v", err)
	}
	if !c.Addons.Enabled() || c.Addons.Dir != dir {
		t.Errorf("ADDONS_DIR parsed as %q, enabled=%v", c.Addons.Dir, c.Addons.Enabled())
	}
}
