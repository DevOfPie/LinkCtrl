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
	"net/netip"
	"net/url"
	"os"
	"sort"
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
	URL          string        `env:"REDIS_URL" envDefault:"redis://redis:6379/0"`
	DialTimeout  time.Duration `env:"REDIS_DIAL_TIMEOUT" envDefault:"1s"`
	ReadTimeout  time.Duration `env:"REDIS_READ_TIMEOUT" envDefault:"50ms"`
	PoolSize     int           `env:"REDIS_POOL_SIZE" envDefault:"50"`
	CacheEnabled bool          `env:"CACHE_ENABLED" envDefault:"true"`
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
	"SMTP_PASSWORD": "there is no mail feature to authenticate to; it was accepted, " +
		"validated and never read",
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
