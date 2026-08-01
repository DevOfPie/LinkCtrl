// Package config loads and validates runtime configuration from the
// environment.
//
// Two properties matter more than the mechanics.
//
// Validation is aggregated rather than fail-on-first. An operator bringing up a
// self-hosted instance for the first time should see every problem in one run,
// not discover them one restart at a time.
//
// Secrets are typed as Secret, which refuses to print itself through fmt, slog
// or JSON. A config dump or a formatted panic cannot leak the database password
// or the API-key pepper.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// EnvPrefix is prepended to every variable name. POSTGRES_* variables consumed
// by the Postgres container itself are deliberately outside this prefix.
const EnvPrefix = "LINKCTRL_"

type Environment string

const (
	Development Environment = "development"
	Production  Environment = "production"
)

func (e Environment) IsProduction() bool { return e == Production }

type Config struct {
	AppEnv  Environment `env:"APP_ENV" envDefault:"production"`
	BaseURL string      `env:"BASE_URL,required"`

	// AppBaseURL and LinkBaseURL split the instance across two hostnames: the
	// dashboard and API on one, short links on the other. Both default to
	// BaseURL, so leaving them unset is the single-host deployment unchanged.
	//
	// After Load they are always populated, so callers use them rather than
	// deciding for themselves whether the split is configured.
	AppBaseURL  string `env:"APP_BASE_URL"`
	LinkBaseURL string `env:"LINK_BASE_URL"`

	HTTP      HTTPConfig
	Log       LogConfig
	DB        DBConfig
	Redis     RedisConfig
	Redirect  RedirectConfig
	Alias     AliasConfig
	Auth      AuthConfig
	Ingest    IngestConfig
	Analytics AnalyticsConfig
	Audit     AuditConfig
	SMTP      SMTPConfig
	Shutdown  ShutdownConfig

	APIKeyPepper Secret `env:"API_KEY_PEPPER,required,unset"`

	DocsEnabled    bool `env:"DOCS_ENABLED" envDefault:"true"`
	SecureCookies  bool `env:"SECURE_COOKIES" envDefault:"true"`
	MigrateOnStart bool `env:"MIGRATE_ON_START" envDefault:"true"`

	// TrustedProxies must stay empty unless the app really is behind a proxy.
	// A non-empty value makes the app believe X-Forwarded-For, which is how
	// rate limiting and analytics get spoofed when it is set carelessly.
	TrustedProxies []netip.Prefix `env:"TRUSTED_PROXIES" envSeparator:","`

	// baseURL is the parsed form of BaseURL, computed once during Load.
	baseURL *url.URL
	// appBaseURL and linkBaseURL are the parsed effective origins. Never nil
	// after Load; both fall back to baseURL.
	appBaseURL  *url.URL
	linkBaseURL *url.URL
}

type HTTPConfig struct {
	Addr              string        `env:"HTTP_ADDR" envDefault:":8080"`
	MetricsAddr       string        `env:"METRICS_ADDR" envDefault:":9090"`
	ReadHeaderTimeout time.Duration `env:"HTTP_READ_HEADER_TIMEOUT" envDefault:"5s"`
	WriteTimeout      time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"30s"`
	RequestTimeout    time.Duration `env:"HTTP_REQUEST_TIMEOUT" envDefault:"15s"`
	ServerTiming      bool          `env:"SERVER_TIMING" envDefault:"false"`
}

type LogConfig struct {
	Level  string `env:"LOG_LEVEL" envDefault:"info"`
	Format string `env:"LOG_FORMAT" envDefault:"json"`
}

type DBConfig struct {
	URL Secret `env:"DATABASE_URL,required,unset"`

	// Two pools. The redirect pool is small, separate, and exists so that a
	// slow analytics query on the application pool cannot starve the hot path
	// of connections. M13 asserts empirically that it does not.
	MaxConns         int32         `env:"DB_MAX_CONNS" envDefault:"20"`
	MinConns         int32         `env:"DB_MIN_CONNS" envDefault:"2"`
	RedirectMaxConns int32         `env:"DB_REDIRECT_MAX_CONNS" envDefault:"6"`
	MaxConnLifetime  time.Duration `env:"DB_MAX_CONN_LIFETIME" envDefault:"1h"`
	MaxConnIdleTime  time.Duration `env:"DB_MAX_CONN_IDLE_TIME" envDefault:"15m"`
	ConnectTimeout   time.Duration `env:"DB_CONNECT_TIMEOUT" envDefault:"10s"`
}

type RedisConfig struct {
	URL         string        `env:"REDIS_URL" envDefault:"redis://redis:6379/0"`
	DialTimeout time.Duration `env:"REDIS_DIAL_TIMEOUT" envDefault:"1s"`
	ReadTimeout time.Duration `env:"REDIS_READ_TIMEOUT" envDefault:"50ms"`

	// InvalidateBudget is the total an edit will wait for the cache to be
	// invalidated, across every retry rather than per attempt. The retry loop
	// used to spend ReadTimeout three times over, so raising ReadTimeout
	// tripled the worst case an operator saw on a form submission; this is the
	// one number that bounds it. D26.
	InvalidateBudget time.Duration `env:"REDIS_INVALIDATE_BUDGET" envDefault:"250ms"`

	PoolSize     int  `env:"REDIS_POOL_SIZE" envDefault:"50"`
	CacheEnabled bool `env:"CACHE_ENABLED" envDefault:"true"`
}

type RedirectConfig struct {
	TTL           time.Duration `env:"REDIRECT_TTL" envDefault:"24h"`
	NegativeTTL   time.Duration `env:"REDIRECT_NEGATIVE_TTL" envDefault:"60s"`
	Timeout       time.Duration `env:"REDIRECT_TIMEOUT" envDefault:"250ms"`
	DefaultStatus int           `env:"REDIRECT_DEFAULT_STATUS" envDefault:"302"`
	LogSample     int           `env:"REDIRECT_LOG_SAMPLE" envDefault:"0"`
	NotFoundLimit int           `env:"REDIRECT_404_RATE_LIMIT" envDefault:"60"`
}

type AliasConfig struct {
	Length              int      `env:"ALIAS_LENGTH" envDefault:"7"`
	MinUserLength       int      `env:"ALIAS_MIN_USER_LENGTH" envDefault:"3"`
	ReservedExtra       []string `env:"ALIAS_RESERVED_EXTRA" envSeparator:","`
	ProfanityFilter     bool     `env:"ALIAS_PROFANITY_FILTER" envDefault:"true"`
	DestSchemes         []string `env:"DESTINATION_SCHEMES" envSeparator:"," envDefault:"http,https"`
	DestMaxLength       int      `env:"DESTINATION_MAX_LENGTH" envDefault:"2048"`
	DestBlockPrivateIPs bool     `env:"DESTINATION_BLOCK_PRIVATE_IPS" envDefault:"true"`
	DestBlocklist       []string `env:"DESTINATION_BLOCKLIST" envSeparator:","`
}

type SignupMode string

const (
	SignupClosed SignupMode = "closed"
	SignupInvite SignupMode = "invite"
	SignupOpen   SignupMode = "open"
)

type AuthConfig struct {
	SignupMode         SignupMode    `env:"SIGNUP_MODE" envDefault:"closed"`
	SessionAbsoluteTTL time.Duration `env:"SESSION_ABSOLUTE_TTL" envDefault:"720h"`
	SessionIdleTTL     time.Duration `env:"SESSION_IDLE_TTL" envDefault:"168h"`

	// InviteTTL is how long an invitation stays redeemable, measured from when
	// it was created (decision D29).
	//
	// A knob rather than a constant, for the reason D5 refused a constant for
	// audit retention: time is the one thing an operator cannot work around
	// without a rebuild. The clock starts at creation and not at delivery,
	// because mail leaves through the outbox on the scheduler's tick (D23) and
	// there is no send moment to start it from — so a slow relay spends the
	// operator's TTL, which is exactly why it is tunable.
	InviteTTL time.Duration `env:"INVITE_TTL" envDefault:"168h"`

	// RFC 9106 recommends at least 19 MiB for the memory-constrained profile;
	// 64 MiB is the comfortable default. Validate enforces the floor, because
	// lowering this is the easiest way to silently weaken password storage.
	Argon2MemoryKiB   uint32 `env:"ARGON2_MEMORY_KIB" envDefault:"65536"`
	Argon2Iterations  uint32 `env:"ARGON2_ITERATIONS" envDefault:"3"`
	Argon2Parallelism uint8  `env:"ARGON2_PARALLELISM" envDefault:"2"`

	LoginRatePerMin  int `env:"LOGIN_RATE_PER_MIN" envDefault:"10"`
	LockoutThreshold int `env:"LOGIN_LOCKOUT_THRESHOLD" envDefault:"5"`
	APIRatePerMin    int `env:"API_RATE_PER_MIN" envDefault:"600"`
}

// IngestConfig tunes the click pipeline.
//
// There is deliberately no worker count. One consumer is what makes batch
// coalescing work — a second would split every batch and interleave the writes —
// so the knob that used to be here was removed rather than implemented. See
// Removed.
type IngestConfig struct {
	QueueSize     int           `env:"INGEST_QUEUE_SIZE" envDefault:"16384"`
	BatchSize     int           `env:"INGEST_BATCH_SIZE" envDefault:"500"`
	FlushInterval time.Duration `env:"INGEST_FLUSH_INTERVAL" envDefault:"250ms"`
}

// AnalyticsConfig tunes analytics storage and enrichment.
//
// Salt rotation and bot classification are not configurable, and that is a
// design decision rather than an omission: the daily rotation is what the purge
// window de-identifies against, and bots are always classified because the
// control that matters — keeping them out of headline figures — is in the
// queries. See Removed.
type AnalyticsConfig struct {
	RetentionDays int    `env:"ANALYTICS_RETENTION_DAYS" envDefault:"395"`
	GeoIPPath     string `env:"GEOIP_MMDB_PATH"`
}

// AuditConfig is the audit log's retention policy, which is deliberately its
// own setting rather than a share of the analytics window.
//
// The default is 0 — keep forever — and it is different from the analytics
// default on purpose. Both choices are a data-loss policy, and they fail in
// opposite directions: a finite window means an upgrade silently starts
// deleting history an operator assumed permanent, while keep-forever means
// unbounded growth. The first failure is invisible and irreversible; the second
// is visible and recoverable, and linkctrl_audit_log_bytes plus the alert recipe
// in docs/operations.md are what make it visible. See decisions.md, D5.
type AuditConfig struct {
	RetentionDays int `env:"AUDIT_RETENTION_DAYS" envDefault:"0"`

	// SizeWarnBytes raises an owner notification once the audit partitions pass
	// it. 5 GB, and **on by default** — which is the asymmetry with
	// RetentionDays above, not an inconsistency (D19).
	//
	// The two defaults protect against opposite failures. Retention defaults to
	// inaction because acting unasked destroys data. The warning defaults to
	// acting because inaction is what leaves the operator uninformed, and
	// keep-forever is only a safe default on an instance nobody configured if
	// that instance is the one being warned. A threshold that had to be
	// switched on would be no threshold at all for exactly the operators who
	// need it.
	//
	// 0 disables it, for an operator who has decided and does not want reminding.
	SizeWarnBytes int64 `env:"AUDIT_SIZE_WARN_BYTES" envDefault:"5368709120"`
}

// TLS modes for the mailer. Three, and no more: the honest set is "the two ways
// a modern submission server listens, plus a local relay that does not".
const (
	// SMTPStartTLS is submission on 587: connect in clear, then upgrade. The
	// default, because it is what almost every provider documents.
	SMTPStartTLS = "starttls"
	// SMTPImplicit is SMTPS on 465: TLS from the first byte.
	SMTPImplicit = "tls"
	// SMTPNone is no encryption at all, for a relay on the same host or the same
	// private network. Credentials are refused in this mode.
	SMTPNone = "none"
)

// SMTPConfig is the optional mailer. Off unless Host is set.
//
// The surface is deliberately small. TLS modes and auth mechanisms are where a
// mail configuration turns into a compatibility matrix, so this ships the set it
// can honestly claim — STARTTLS, implicit TLS, or nothing, with PLAIN auth over
// an encrypted connection — and documents the rest as unsupported rather than
// implying it works and failing at the first send.
type SMTPConfig struct {
	// Host is the switch. Empty means no mailer, which is the default and the
	// state every consumer must degrade to.
	Host string `env:"SMTP_HOST"`
	Port int    `env:"SMTP_PORT" envDefault:"587"`

	// Username and Password authenticate with PLAIN. Both or neither.
	Username string `env:"SMTP_USERNAME"`
	Password Secret `env:"SMTP_PASSWORD,unset"`

	// From is the envelope sender and the From header. Required once Host is
	// set: a message with no sender is refused by most receivers, and finding
	// that out from a bounce is worse than finding it out at boot.
	From string `env:"SMTP_FROM"`

	TLS string `env:"SMTP_TLS" envDefault:"starttls"`

	// Timeout bounds one delivery attempt end to end — dial, handshake, DATA.
	// A hung relay must not hold the scheduler.
	Timeout time.Duration `env:"SMTP_TIMEOUT" envDefault:"10s"`
}

// Enabled reports whether a mailer is configured. The one question every
// consumer asks, so it is a method rather than a comparison repeated five times.
func (s SMTPConfig) Enabled() bool { return s.Host != "" }

// Addr is the host:port to dial.
func (s SMTPConfig) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

type ShutdownConfig struct {
	DrainDelay time.Duration `env:"SHUTDOWN_DRAIN_DELAY" envDefault:"5s"`
	Timeout    time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"15s"`
}

// Load reads configuration from the environment and validates it.
//
// A .env file is honoured only in development, and only when APP_ENV says so
// before the file is read. A stray .env on a production host must not be able
// to change how the service runs.
func Load() (Config, error) {
	if Environment(os.Getenv(EnvPrefix+"APP_ENV")) == Development {
		if _, err := os.Stat(".env"); err == nil {
			if err := godotenv.Load(); err != nil {
				return Config{}, fmt.Errorf("load .env: %w", err)
			}
		}
	}
	return Parse()
}

// FileSecretVars are the variables that additionally support a _FILE suffix,
// for Docker and Swarm secrets mounted under /run/secrets.
var FileSecretVars = []string{
	"API_KEY_PEPPER",
	"DATABASE_URL",
	"SMTP_PASSWORD",
}

// resolveFileSecrets implements the LINKCTRL_X_FILE convention: when set, the
// file at that path is read and its contents become LINKCTRL_X.
//
// This is hand-rolled rather than delegated to the env library's "file" option,
// which has different semantics — there, the variable's own value is the path,
// so there is no way to supply a secret inline. Both forms need to work: inline
// for a plain .env, and _FILE for orchestrators that mount secrets as files.
//
// The trailing newline is trimmed deliberately. `echo secret > secret.txt` adds
// one, and a password with a trailing newline fails authentication for exactly
// the same invisible-character reason a CRLF in .env does.
func resolveFileSecrets() error {
	var errs []error
	for _, name := range FileSecretVars {
		fileVar := EnvPrefix + name + "_FILE"
		path := os.Getenv(fileVar)
		if path == "" {
			continue
		}
		direct := EnvPrefix + name
		if os.Getenv(direct) != "" {
			errs = append(errs, fmt.Errorf(
				"%s and %s are both set; supply the secret one way or the other", direct, fileVar))
			continue
		}
		// The path is the whole point: an operator sets LINKCTRL_X_FILE to say
		// which file holds the secret, so reading a variable path is the
		// feature. Anyone who can set the process environment can already read
		// what the process reads.
		b, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied path, by design
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: cannot read %q: %w", fileVar, path, err))
			continue
		}
		if err := os.Setenv(direct, strings.TrimRight(string(b), "\r\n")); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", direct, err))
		}
	}
	return errors.Join(errs...)
}

// Removed names variables that once existed and no longer do, each with the
// behaviour that is now fixed.
//
// Kept as data rather than deleted quietly, because silent removal reproduces
// the defect it is fixing from the other side: the operator still has the line
// in their .env and still believes it does something. Startup reports these as
// warnings rather than errors — an upgrade must not refuse to boot over a stale
// line in a file.
var Removed = map[string]string{
	"SECRET_KEY": "nothing was keyed by it. Sessions use random 32-byte tokens " +
		"stored as SHA-256, CSRF is origin-based, and API keys use API_KEY_PEPPER; " +
		"rotating this changed nothing, which is the opposite of what a variable " +
		"with this name promises",
	// SMTP_PASSWORD was here, and is not any more. It was removed in Phase 1
	// because there was no mail feature to authenticate to; M26 built one, so
	// the variable is read again and a warning that it does nothing would now be
	// the lie the Removed list exists to prevent. Entries leave this map when
	// the behaviour comes back, which is the only reason one ever should.
	"INGEST_WORKERS": "the ingester runs a single consumer, which is what makes " +
		"batch coalescing work; a worker count would break it",
	"VISITOR_SALT_ROTATION": "visitor salts rotate once per UTC day, which is the " +
		"period the purge window de-identifies against",
	"BOT_FILTER_ENABLED": "bots are always classified and recorded; headline " +
		"figures exclude them in the queries instead",
}

// RemovedInUse reports removed variables that are still set, ready to log.
//
// Sorted, so the output is stable across runs and diffable in a log.
func RemovedInUse() []string {
	var out []string
	for name, why := range Removed {
		if os.Getenv(EnvPrefix+name) != "" {
			out = append(out, fmt.Sprintf("%s%s is set but no longer read: %s",
				EnvPrefix, name, why))
		}
	}
	sort.Strings(out)
	return out
}

// Parse reads configuration from the current environment without consulting a
// .env file. Tests use it directly.
func Parse() (Config, error) {
	if err := resolveFileSecrets(); err != nil {
		return Config{}, err
	}
	var c Config
	if err := env.ParseWithOptions(&c, env.Options{Prefix: EnvPrefix}); err != nil {
		return Config{}, err
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	// Parsed during Validate; store it for BaseURLParsed.
	u, _ := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	c.baseURL = u
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")

	// Unset means "the same host as everything else", which is the deployment
	// this product shipped with and must keep working untouched.
	if c.AppBaseURL == "" {
		c.AppBaseURL = c.BaseURL
	}
	if c.LinkBaseURL == "" {
		c.LinkBaseURL = c.BaseURL
	}
	c.AppBaseURL = strings.TrimRight(c.AppBaseURL, "/")
	c.LinkBaseURL = strings.TrimRight(c.LinkBaseURL, "/")
	c.appBaseURL, _ = url.Parse(c.AppBaseURL)
	c.linkBaseURL, _ = url.Parse(c.LinkBaseURL)
	return c, nil
}

// BaseURLParsed returns the parsed canonical origin.
func (c Config) BaseURLParsed() *url.URL { return c.baseURL }

// AppOrigin returns the origin serving the dashboard and the API.
//
// Falls back to BaseURL, so a Config assembled by hand — every test does this
// rather than going through Load — behaves as a single-host deployment instead
// of as one with no dashboard origin at all.
func (c Config) AppOrigin() string {
	if c.AppBaseURL != "" {
		return c.AppBaseURL
	}
	return c.BaseURL
}

// LinkOrigin returns the origin short links are published under.
func (c Config) LinkOrigin() string {
	if c.LinkBaseURL != "" {
		return c.LinkBaseURL
	}
	return c.BaseURL
}

// AppBaseURLParsed returns the origin serving the dashboard and the API.
func (c Config) AppBaseURLParsed() *url.URL { return c.appBaseURL }

// LinkBaseURLParsed returns the origin serving short links.
func (c Config) LinkBaseURLParsed() *url.URL { return c.linkBaseURL }

// Host returns the host short links are served on, which is the default domain
// when resolving an alias.
func (c Config) Host() string {
	if c.linkBaseURL != nil {
		return c.linkBaseURL.Host
	}
	if c.baseURL == nil {
		return ""
	}
	return c.baseURL.Host
}

// SplitHosts reports whether the dashboard and short links are served on
// different hostnames.
//
// Compared on host rather than on the whole origin: the routing decision and
// the cookie boundary are both about the host, and an instance configured with
// two schemes on one host has neither.
func (c Config) SplitHosts() bool {
	if c.appBaseURL == nil || c.linkBaseURL == nil {
		return false
	}
	return CanonicalHost(c.appBaseURL.Host) != CanonicalHost(c.linkBaseURL.Host)
}

// CanonicalHost normalizes a Host header or a URL host for comparison:
// lowercased, with an explicit default HTTP(S) port removed.
//
// The port matters because the two sides of the comparison come from different
// places. The configured value is written by an operator ("manage.example.com")
// and the request value is written by a proxy, which may or may not append
// ":443". Comparing them raw makes the router's behavior depend on that choice.
func CanonicalHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if port != "80" && port != "443" {
		return host
	}
	// SplitHostPort strips the brackets an IPv6 literal needs to stay parseable.
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}

// Validate collects every problem rather than returning at the first.
//
// The messages name the variable and say what to do about it. An operator
// reading them should not need to consult the source.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	switch c.AppEnv {
	case Development, Production:
	default:
		add("APP_ENV: must be %q or %q, got %q", Development, Production, c.AppEnv)
	}

	// The three origins share one rule set, so a mistake in APP_BASE_URL reads
	// the same way as the same mistake in BASE_URL.
	checkOrigin := func(name, raw string) *url.URL {
		u, err := url.Parse(raw)
		switch {
		case err != nil:
			add("%s: not a valid URL: %v", name, err)
			return nil
		case u.Scheme != "http" && u.Scheme != "https":
			add("%s: scheme must be http or https, got %q", name, u.Scheme)
		case u.Host == "":
			add("%s: must include a host, for example https://links.example.com", name)
		case u.Path != "" && u.Path != "/":
			add("%s: must not include a path, got %q", name, u.Path)
		case u.RawQuery != "" || u.Fragment != "":
			add("%s: must not include a query or fragment", name)
		}
		return u
	}

	u := checkOrigin("BASE_URL", c.BaseURL)
	var appU, linkU *url.URL
	if c.AppBaseURL != "" {
		appU = checkOrigin("APP_BASE_URL", c.AppBaseURL)
	}
	if c.LinkBaseURL != "" {
		linkU = checkOrigin("LINK_BASE_URL", c.LinkBaseURL)
	}

	if c.APIKeyPepper.Len() < 32 {
		add("API_KEY_PEPPER: must be at least 32 bytes, got %d (generate: openssl rand -base64 48). "+
			"Changing this invalidates every existing API key.", c.APIKeyPepper.Len())
	}
	if c.DB.URL.IsZero() {
		add("DATABASE_URL: is required")
	}

	// Production-only invariants. These exist because the failure they prevent
	// is silent: cookies that never persist, or credentials sent in clear.
	if c.AppEnv.IsProduction() {
		if !c.SecureCookies {
			add("SECURE_COOKIES: cannot be false when APP_ENV=production")
		}
		for name, o := range map[string]*url.URL{
			"BASE_URL": u, "APP_BASE_URL": appU, "LINK_BASE_URL": linkU,
		} {
			if o != nil && o.Scheme == "http" {
				add("%s: must use https in production; session cookies use the "+
					"__Host- prefix, which browsers only accept over TLS", name)
			}
		}
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("LOG_LEVEL: must be one of debug, info, warn, error; got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("LOG_FORMAT: must be json or text, got %q", c.Log.Format)
	}

	switch c.Auth.SignupMode {
	case SignupClosed, SignupInvite, SignupOpen:
	default:
		add("SIGNUP_MODE: must be one of closed, invite, open; got %q", c.Auth.SignupMode)
	}

	if c.Auth.Argon2MemoryKiB < 19*1024 {
		add("ARGON2_MEMORY_KIB: must be at least 19456 (RFC 9106 minimum), got %d",
			c.Auth.Argon2MemoryKiB)
	}
	if c.Auth.Argon2Iterations < 2 {
		add("ARGON2_ITERATIONS: must be at least 2, got %d", c.Auth.Argon2Iterations)
	}
	if c.Auth.Argon2Parallelism < 1 {
		add("ARGON2_PARALLELISM: must be at least 1")
	}
	if c.Auth.SessionIdleTTL > c.Auth.SessionAbsoluteTTL {
		add("SESSION_IDLE_TTL (%s): must not exceed SESSION_ABSOLUTE_TTL (%s)",
			c.Auth.SessionIdleTTL, c.Auth.SessionAbsoluteTTL)
	}
	// Zero is refused rather than read as "never expires". D29 rejected
	// no-expiry outright: single-use bounds an invite to one account, and under
	// D27 a stale one is still a live grant to the named address, so nothing
	// else bounds it in time. An operator who wants a very long window says so
	// in hours.
	if c.Auth.InviteTTL <= 0 {
		add("INVITE_TTL: must be positive, got %s; there is no way to disable "+
			"invitation expiry", c.Auth.InviteTTL)
	}

	// A long negative TTL makes a newly created link look broken to anyone who
	// probed the alias first. Create deletes the negative key, but a cap keeps
	// the blast radius small if that path is ever missed.
	if c.Redirect.NegativeTTL > 5*time.Minute {
		add("REDIRECT_NEGATIVE_TTL: must not exceed 5m, got %s; a longer value makes "+
			"a newly created link appear broken", c.Redirect.NegativeTTL)
	}
	switch c.Redirect.DefaultStatus {
	case 301, 302, 307, 308:
		if c.Redirect.DefaultStatus == 301 || c.Redirect.DefaultStatus == 308 {
			add("REDIRECT_DEFAULT_STATUS: %d is a permanent redirect and will be cached "+
				"by browsers and intermediaries, so later edits to a link will not take "+
				"effect. Use 302 or 307.", c.Redirect.DefaultStatus)
		}
	default:
		add("REDIRECT_DEFAULT_STATUS: must be 302 or 307, got %d", c.Redirect.DefaultStatus)
	}

	if c.DB.MinConns > c.DB.MaxConns {
		add("DB_MIN_CONNS (%d): must not exceed DB_MAX_CONNS (%d)", c.DB.MinConns, c.DB.MaxConns)
	}
	if c.DB.RedirectMaxConns < 1 {
		add("DB_REDIRECT_MAX_CONNS: must be at least 1")
	}
	if total := c.DB.MaxConns + c.DB.RedirectMaxConns; total > 90 {
		add("DB pools total %d connections, which approaches the default Postgres "+
			"max_connections of 100; lower them or raise the server limit", total)
	}

	if c.Ingest.QueueSize < 1 {
		add("INGEST_QUEUE_SIZE: must be at least 1")
	}
	if c.Ingest.BatchSize < 1 {
		add("INGEST_BATCH_SIZE: must be at least 1")
	}
	if c.Ingest.BatchSize > c.Ingest.QueueSize {
		add("INGEST_BATCH_SIZE (%d): must not exceed INGEST_QUEUE_SIZE (%d)",
			c.Ingest.BatchSize, c.Ingest.QueueSize)
	}

	// Rate limits. Zero is a legal value meaning "no limit", so only a negative
	// number is a mistake — and it is worth catching, because a limit of -1 reads
	// like "unlimited" and would otherwise silently disable throttling.
	for name, v := range map[string]int{
		"LOGIN_RATE_PER_MIN":      c.Auth.LoginRatePerMin,
		"API_RATE_PER_MIN":        c.Auth.APIRatePerMin,
		"REDIRECT_404_RATE_LIMIT": c.Redirect.NotFoundLimit,
		"LOGIN_LOCKOUT_THRESHOLD": c.Auth.LockoutThreshold,
	} {
		if v < 0 {
			add("%s: must be 0 (no limit) or positive, got %d", name, v)
		}
	}

	if c.Analytics.RetentionDays < 0 {
		add("ANALYTICS_RETENTION_DAYS: must be 0 (keep forever) or positive, got %d",
			c.Analytics.RetentionDays)
	}
	if c.Audit.RetentionDays < 0 {
		add("AUDIT_RETENTION_DAYS: must be 0 (keep forever) or positive, got %d",
			c.Audit.RetentionDays)
	}
	if c.Audit.SizeWarnBytes < 0 {
		add("AUDIT_SIZE_WARN_BYTES: must be 0 (no warning) or positive, got %d",
			c.Audit.SizeWarnBytes)
	}

	// The mailer. Every check is inside the Enabled guard: an instance with no
	// SMTP_HOST is the default deployment and must not be told about mail
	// settings it never set.
	if c.SMTP.Enabled() {
		if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
			add("SMTP_PORT: must be between 1 and 65535, got %d", c.SMTP.Port)
		}
		switch c.SMTP.TLS {
		case SMTPStartTLS, SMTPImplicit, SMTPNone:
		default:
			add("SMTP_TLS: must be one of %s, %s, %s; got %q",
				SMTPStartTLS, SMTPImplicit, SMTPNone, c.SMTP.TLS)
		}
		if c.SMTP.From == "" {
			add("SMTP_FROM: is required once SMTP_HOST is set, for example " +
				`"LinkCtrl <links@example.com>"`)
		} else if _, err := mail.ParseAddress(c.SMTP.From); err != nil {
			// Parsed rather than pattern-matched, and parsed here rather than at
			// the first send: an unparseable sender is a configuration mistake,
			// and a configuration mistake found by a bounce three days later is
			// the failure this whole file exists to prevent.
			add("SMTP_FROM: %q is not a valid address: %v", c.SMTP.From, err)
		}
		// Both or neither. A username with no password authenticates as nobody
		// and a password with no username is never sent, and both fail as
		// "relay access denied" — an error that says nothing about the cause.
		if (c.SMTP.Username == "") != c.SMTP.Password.IsZero() {
			add("SMTP_USERNAME and SMTP_PASSWORD: set both or neither; " +
				"one without the other cannot authenticate")
		}
		// Credentials in clear are refused rather than warned about. Go's own
		// SMTP client refuses PLAIN over an unencrypted connection too, so
		// permitting it here would only move the failure to the first send.
		if c.SMTP.TLS == SMTPNone && c.SMTP.Username != "" {
			add("SMTP_TLS=none with SMTP_USERNAME set would send the password in "+
				"clear; use %s or %s, or drop the credentials for a local relay",
				SMTPStartTLS, SMTPImplicit)
		}
		if c.SMTP.Timeout <= 0 {
			add("SMTP_TIMEOUT: must be positive, got %s", c.SMTP.Timeout)
		}
	}

	if c.Analytics.GeoIPPath != "" {
		if _, err := os.Stat(c.Analytics.GeoIPPath); err != nil {
			add("GEOIP_MMDB_PATH: %q is not readable: %v; leave it empty to disable "+
				"geographic reporting", c.Analytics.GeoIPPath, err)
		}
	}

	if c.Alias.Length < 4 || c.Alias.Length > 12 {
		add("ALIAS_LENGTH: must be between 4 and 12, got %d", c.Alias.Length)
	}
	if c.Alias.MinUserLength < 1 {
		add("ALIAS_MIN_USER_LENGTH: must be at least 1")
	}
	for _, s := range c.Alias.DestSchemes {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "http", "https":
		default:
			add("DESTINATION_SCHEMES: %q is not allowed; only http and https are "+
				"supported, and permitting others enables javascript: and data: "+
				"redirect attacks", s)
		}
	}

	// The drain delay plus the HTTP timeout must fit inside the container's
	// stop grace period, or Docker sends SIGKILL mid-flush and the buffered
	// click events that graceful shutdown exists to save are lost anyway.
	if total := c.Shutdown.DrainDelay + c.Shutdown.Timeout; total > 25*time.Second {
		add("SHUTDOWN_DRAIN_DELAY + SHUTDOWN_TIMEOUT = %s, which leaves no margin under "+
			"the compose stop_grace_period of 30s; reduce them", total)
	}

	return errors.Join(errs...)
}
