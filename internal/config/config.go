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
	Domains   DomainsConfig
	Alias     AliasConfig
	Auth      AuthConfig
	Ingest    IngestConfig
	Analytics AnalyticsConfig
	Audit     AuditConfig
	SMTP      SMTPConfig
	Feed      FeedConfig
	Webhooks  WebhooksConfig
	Addons    AddonsConfig
	Shutdown  ShutdownConfig

	APIKeyPepper Secret `env:"API_KEY_PEPPER,required,unset"`

	// MFASecretKey encrypts the TOTP secret at rest (M53).
	//
	// **Its own variable, never the pepper**, which m53.md refuses by name. The
	// pepper is bound to retained API-key rows and rotating it silently
	// invalidates every issued key; sharing it would mean rotating an API-key
	// secret also locks every account out of its second factor, coupling two
	// credential lifecycles that have nothing to do with each other.
	//
	// **Optional, unlike the pepper, and the asymmetry is deliberate.** Unset is
	// an instance with no second factor available, which is exactly what every
	// deployment was before this milestone — making it required would refuse to
	// boot every existing instance on upgrade to buy a feature nobody had asked
	// for. Losing it after accounts have enrolled locks those accounts out of the
	// second factor and no further: recovery codes are SHA-256 and do not involve
	// this key, so an enrolled account signs in with one, disables the factor with
	// another, and enrols again. docs/configuration.md states that chain beside
	// the variable, in the same terms the pepper's consequence is stated in.
	MFASecretKey Secret `env:"MFA_SECRET_KEY,unset"`

	// UpdateCheck is whether this instance may ask, once a day, whether a newer
	// LinkCtrl has been published (M55).
	//
	// **The deployment's half of a two-part switch, and it only ever says no.**
	// The other half is `instance_settings.update_check_enabled`, which is the
	// answer an operator gave when they were asked (D149, D164). The check runs
	// when both allow it: this variable is what an air-gapped or egress-restricted
	// deployment sets to `false` in the place such a deployment configures
	// everything else, and setting it there cannot be undone from a browser by
	// somebody who does not know why the box has no egress.
	//
	// **Default true, and true is permission rather than instruction.** The owner
	// overruled a recommendation of off-by-default on the grounds that the
	// operator is asked and therefore chooses knowingly (D149) — so what this
	// default buys is that the question gets asked, not that the request gets
	// made. The other half starts unanswered and reads as off (D164), which is
	// where an instance upgrading into 0.3.0 sits until an administrator signs in.
	// What the request carries is enumerated in docs/configuration.md beside this
	// variable, and in internal/update's package comment, where a test holds the
	// enumeration to the wire.
	UpdateCheck bool `env:"UPDATE_CHECK" envDefault:"true"`

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

	// SubscriberReadTimeout is how long the cache-invalidation subscriber will
	// sit in one read before it makes Redis prove the subscription is still
	// delivering. It is not ReadTimeout and cannot be: on the hot path a
	// timeout means the cache failed, while here it usually means nobody has
	// edited a link, which is the ordinary state of a healthy instance. F30,
	// D42.
	SubscriberReadTimeout time.Duration `env:"REDIS_SUBSCRIBER_READ_TIMEOUT" envDefault:"30s"`

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
	// PasswordLimit caps guesses at a password link, per minute, per address
	// *and* per alias (M35, D54). Twenty rather than the login limit's number:
	// a person who has been handed a link and its password types it once, and a
	// legitimate visitor never approaches this. Zero disables it, which on a
	// public instance means a link password is only as strong as the wordlist
	// somebody is willing to run.
	PasswordLimit int `env:"LINK_PASSWORD_RATE_LIMIT" envDefault:"20"`
}

// DomainsConfig is custom-domain verification (M40).
//
// **Every value here is operator-visible on purpose, and decision D70 is why.**
// The grace window decides how long an instance keeps serving a hostname whose
// DNS its owner may no longer control, and a number with that consequence
// belongs in configuration and in the deployment runbook rather than in a
// constant somebody has to read the source to find.
type DomainsConfig struct {
	// VerifyInterval is how often the leader re-checks every registered
	// hostname. One hour: the point of the cadence is to make a single failure
	// weak evidence and a sustained one strong, and at this rate a domain must
	// fail twenty-four consecutive checks before serving stops. Zero disables
	// the job entirely, which leaves verification on-demand only.
	VerifyInterval time.Duration `env:"DOMAIN_VERIFY_INTERVAL" envDefault:"1h"`
	// VerifyGrace is how long a *serving* hostname keeps serving after its first
	// failed check. Twenty-four hours: long enough that somebody woken by the
	// notification has a working day to fix their DNS, short enough that the
	// window is stated in the runbook as "one day" rather than as a calculation.
	// It is never zero — an unset or zero value takes the default, because a
	// zero window would turn one resolver hiccup into an outage.
	VerifyGrace time.Duration `env:"DOMAIN_VERIFY_GRACE" envDefault:"24h"`
	// VerifyDNSTimeout bounds one TXT lookup. A nameserver that accepts a query
	// and never answers must cost this and not the whole pass.
	VerifyDNSTimeout time.Duration `env:"DOMAIN_VERIFY_DNS_TIMEOUT" envDefault:"5s"`
	// VerifyBatch caps how many hostnames one pass checks, oldest check first.
	// A bound rather than a limit anybody is expected to reach: it is what keeps
	// an instance with ten thousand registrations from turning one job run into
	// ten thousand DNS queries.
	VerifyBatch int `env:"DOMAIN_VERIFY_BATCH" envDefault:"500"`
}

type AliasConfig struct {
	Length          int      `env:"ALIAS_LENGTH" envDefault:"7"`
	MinUserLength   int      `env:"ALIAS_MIN_USER_LENGTH" envDefault:"3"`
	ReservedExtra   []string `env:"ALIAS_RESERVED_EXTRA" envSeparator:","`
	ProfanityFilter bool     `env:"ALIAS_PROFANITY_FILTER" envDefault:"true"`
	// DestSchemes may narrow the scheme allowlist and may never widen it:
	// Validate refuses anything outside {http, https}. Non-http(s) schemes are
	// the unappealable tier (M30), and a variable that could add "javascript"
	// back would be an override switch on a tier documented as having none.
	DestSchemes   []string `env:"DESTINATION_SCHEMES" envSeparator:"," envDefault:"http,https"`
	DestMaxLength int      `env:"DESTINATION_MAX_LENGTH" envDefault:"2048"`

	// DestBlocklist is the operator's own host list. Since M30 it seeds the
	// runtime Postgres blocklist at boot rather than being consulted in memory,
	// and it is reconciled on every boot — an entry removed from here is
	// retired, and nothing the owner added through review is touched.
	DestBlocklist []string `env:"DESTINATION_BLOCKLIST" envSeparator:","`
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

	// UploadRatePerMin bounds how often one address may upload a file (M50.5).
	//
	// **A bucket of its own because an upload is not an API call.** Every other
	// request under `/api/v1` carries a body this product caps at 256 KiB and
	// parses as JSON; an upload carries up to `qr.MaxLogoUploadBytes` and is
	// decoded, which is the one place a request's cost is set by its content
	// rather than by its shape. `API_RATE_PER_MIN` defaults to 600, and 600
	// megabyte uploads a minute is a bandwidth and decoder budget nobody chose
	// by setting a number about JSON.
	//
	// Thirty is what somebody restyling a poster does — upload, look, upload
	// again — with room to spare. It charges the *address* like every other
	// limit here rather than the workspace: the resource being protected is this
	// instance's, and an attacker with one account has as many addresses as they
	// have hosts either way.
	UploadRatePerMin int `env:"UPLOAD_RATE_PER_MIN" envDefault:"30"`
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

// FeedConfig is the optional third-party reputation feed (M32). Off unless URL
// is set, and off is the default.
//
// This is the only setting in this file whose default is chosen by a promise
// rather than by an engineering trade. Every other blocking decision this
// product makes is local — a compiled host list, a Postgres table, heuristics
// that read a URL's own text. Answering *is this destination malicious* means
// sending the destination to somebody else's server, which is a deliberate
// exception to Plan.md's "no destination leaves the box uninvited" and is why
// switching it on costs an operator a named feed rather than a boolean.
//
// FeedName is required alongside FeedURL for the same reason: the disclosure
// this feature ships names the third party, and a disclosure that cannot is not
// one. See docs/build-notes/decisions.md, D40.
type FeedConfig struct {
	// URL is the endpoint, and the switch. Empty means no feed, no client, and
	// no code path that sends a destination anywhere.
	URL string `env:"FEED_URL"`
	// Name is the third party in words — "Google Safe Browsing", "urlscan.io" —
	// as the disclosure page and the docs print it.
	Name string `env:"FEED_NAME"`

	// Method is GET or POST. POST by default, which is what most reputation
	// APIs take and which keeps the destination out of the feed's access log
	// query string.
	Method string `env:"FEED_METHOD" envDefault:"POST"`
	// Param names the field carrying the destination: a query parameter on GET,
	// a JSON key on POST.
	Param string `env:"FEED_PARAM" envDefault:"url"`
	// VerdictField is the dotted path into the JSON response holding the
	// answer, e.g. "data.malicious".
	VerdictField string `env:"FEED_VERDICT_FIELD" envDefault:"blocked"`

	// AuthHeader and AuthToken authenticate to the feed. The header is only
	// sent when the token is set.
	AuthHeader string `env:"FEED_AUTH_HEADER" envDefault:"Authorization"`
	AuthToken  Secret `env:"FEED_AUTH_TOKEN,unset"`

	// Timeout bounds one check. Spent inside a link creation somebody is
	// waiting on, so it is small: two seconds is long enough for a healthy API
	// on another continent and short enough that a sick one is not felt as the
	// dashboard being broken.
	Timeout time.Duration `env:"FEED_TIMEOUT" envDefault:"2s"`
}

// Enabled reports whether a feed is configured. The one question every consumer
// asks, so it is a method rather than a comparison repeated in four places.
func (f FeedConfig) Enabled() bool { return f.URL != "" }

// WebhooksConfig is outbound webhook delivery (M42).
//
// Two numbers, both operator-visible for the reason D70 made the domain
// verification numbers visible: each has a consequence somebody deploying this
// has to be able to see and change. The timeout decides how long one unresponsive
// receiver holds a delivery slot, and the retention window decides how long the
// delivery log — one row per link write per enabled webhook — is kept before it
// is pruned.
//
// The **attempt count is not here**, and that is deliberate rather than an
// omission. It is `webhook.MaxAttempts`, six, and it is documented in
// docs/usage.md: unlike the two below, changing it changes what a *receiver*
// experiences — how long a delivery can arrive late — which is a contract with
// somebody who does not read this instance's environment. An operator who wants a
// different one is asking for a different contract, and should say so in a
// release rather than in a variable.
type WebhooksConfig struct {
	// Timeout bounds one delivery attempt end to end: connect, write, read. Ten
	// seconds is long enough for a receiver that does real work before answering
	// and short enough that a batch of twenty slow ones fits well inside the
	// job's own bound.
	Timeout time.Duration `env:"WEBHOOK_TIMEOUT" envDefault:"10s"`
	// RetentionDays is how long a delivered or abandoned delivery row is kept.
	// Thirty days, matching the mail outbox, because the two are the same kind
	// of record: what was attempted and what happened, not an archive.
	//
	// Never zero. Zero means "keep forever" elsewhere in this file (audit
	// retention, D5), and a table that grows by one row per link write per
	// webhook with no window is the growth problem that convention exists to
	// make visible rather than to permit here. Validate refuses it.
	RetentionDays int `env:"WEBHOOK_RETENTION_DAYS" envDefault:"30"`
}

// AddonsConfig is where the WASM host looks for add-ons (M60).
type AddonsConfig struct {
	// Dir is an operator-owned directory holding one subdirectory per add-on,
	// each with an addon.json and the .wasm it describes.
	//
	// **Unset is the shipped default and it means there is no host**: no WASM
	// runtime is constructed, no goroutine started, no route mounted, no table
	// created and no metric series published. That is asserted by tests in
	// internal/addon rather than promised here, because "off costs nothing" is
	// the kind of claim that stops being true one milestone after somebody writes
	// it down.
	//
	// Operator-owned rather than a path an add-on or a tenant can influence: a
	// module in this directory is code this instance executes, so who may write
	// to it is the whole of the trust boundary. docs/SECURITY.md states that in
	// the same terms.
	Dir string `env:"ADDONS_DIR"`

	// InlineDeadline is how long an add-on holding `redirect.inline` may keep a
	// redirect open before the host stops waiting for it, kills the invocation and
	// answers the visitor without it (M66).
	//
	// **One knob for the instance, with no per-add-on override**, which was fixed
	// as the shape of this answer a phase before the number existed: a second
	// number per add-on would be an operator choosing a latency budget per module
	// with no more information than they had for the first, and the case that
	// argues for one has not arrived.
	//
	// The default is measured rather than chosen — see addon.DefaultInlineDeadline
	// and the runs in docs/slo.md — and it is deliberately larger than the 20 ms
	// cached-redirect target. The target is core's, measured with nothing on the
	// path; this is the point at which the host stops waiting for somebody else's
	// code, and setting it under the target would kill add-ons that were working.
	//
	// It bounds the **guest call and nothing else**. Starting the module is this
	// host's own cost on this host's machine, so it is bounded separately by
	// [AddonsConfig.InstantiateDeadline] below. The two shipped as one number and
	// that was F326: on a machine slower than the one 25 ms was measured on, every
	// invocation was killed before the add-on's code ran, and the counter blamed
	// the add-on.
	InlineDeadline time.Duration `env:"ADDON_INLINE_DEADLINE" envDefault:"25ms"`

	// InstantiateDeadline is how long this instance will spend starting an add-on's
	// module for a redirect — inline or observing — before it gives up and serves
	// the redirect without it (M66, reopened; D327).
	//
	// **Separate from the deadline above because it is somebody else's cost.** What
	// a module does is the add-on's and is bounded by a number an operator sets
	// against their tolerance for latency; what instantiating costs is a property
	// of this machine, this load and this build, none of which the add-on chose.
	// Charging it to the add-on's budget made the add-on's budget a function of how
	// fast the hardware is, which is what F326 found on a CI runner.
	//
	// **Wider, and not borrowed from either number that already exists.**
	// LINKCTRL_ADDON_LOAD_TIMEOUT bounds a module that hangs at boot at 30 seconds
	// and no redirect may wait that; the inline deadline is the number that proved
	// too small. 500 ms is eight times instantiation measured under contention on
	// the machine the figure was taken on, and it is what a module hanging in
	// package initialization costs the one redirect it arrived on — see
	// addon.DefaultInstantiateDeadline for the measurement and the arithmetic. It
	// leans wide on purpose: a bound that is too small stops add-ons running and
	// blames them for it, which is the defect this variable exists because of.
	InstantiateDeadline time.Duration `env:"ADDON_INSTANTIATE_DEADLINE" envDefault:"500ms"`
}

// Enabled reports whether this instance has an add-on host at all.
func (a AddonsConfig) Enabled() bool { return a.Dir != "" }

// AddonEnvPrefix is where an add-on's configured settings are read from:
// LINKCTRL_ADDON_<NAME>_<SETTING>, both halves upper-cased.
//
// The same environment every other value in this file comes from, deliberately —
// m64.md's "config reaches an add-on the way it reaches the product". An add-on's
// settings cannot be struct fields, because which of them exist is decided by a
// manifest an operator dropped in a directory rather than by this build, so
// [AddonSettings] reads them by name instead of by tag. That is the whole of the
// difference, and it costs two things worth knowing:
//
//   - `.env.example` cannot enumerate them, so the reference documents the shape
//     and surface_test.go carves the prefix out by name rather than by accident;
//   - the `unset` treatment the env library gives this file's own secrets does not
//     reach them. A value read here stays in the process environment, because the
//     add-on host may be opened more than once in one process and a variable
//     consumed by the first open would be missing from the second. What does reach
//     them is the [Secret] type: every value comes back wrapped, whatever the
//     manifest called it, so no value an operator configured can print itself
//     through fmt, slog or json.
const AddonEnvPrefix = EnvPrefix + "ADDON_"

// AddonSettingVar is the variable one setting of one add-on is read from.
func AddonSettingVar(addon, setting string) string {
	return AddonEnvPrefix + strings.ToUpper(addon) + "_" + strings.ToUpper(setting)
}

// AddonOverrideNames are the two per-add-on variables that are **not** settings:
// they are answers an operator gives about an add-on rather than values an add-on
// reads, and no add-on may declare a setting by either name.
//
// They live in the same LINKCTRL_ADDON_<NAME>_<X> namespace deliberately — an
// operator configuring an add-on should not have to learn a second prefix — which
// is why the collision has to be closed rather than tolerated: without the
// reservation, LINKCTRL_ADDON_OIDC_FAILURE_CLASS would be the operator's answer
// and a declared setting called `failure_class` at the same time, and no lookup
// could tell which was meant. internal/addon's manifest validation refuses a
// manifest declaring either name, so the ambiguity does not exist rather than
// being resolved.
//
//   - `failure_class` overrides what the manifest declared. It is the escape hatch
//     m65.md requires: an add-on holding `session.mint` is treated as `required`
//     whatever its manifest says, and this is how an operator says otherwise,
//     knowing that external sign-in then disappears on a failed load while local
//     sign-in continues.
//   - `mfa_satisfied` says the provider behind this add-on already met a second
//     factor. False is the default and the safe reading: an account with TOTP
//     enrolled meets its factor after an add-on's assertion rather than instead of
//     it.
var AddonOverrideNames = []string{"failure_class", "mfa_satisfied"}

// AddonReservedNames are add-on names no manifest may take, because this file
// already spells a variable that a setting of that add-on would spell too.
//
// Two entries, and both are a redirect bound's: `LINKCTRL_ADDON_INLINE_DEADLINE`
// ([AddonsConfig.InlineDeadline]) and `LINKCTRL_ADDON_INSTANTIATE_DEADLINE`
// ([AddonsConfig.InstantiateDeadline]) are instance-wide, and each is also exactly
// what a setting called `deadline` on an add-on called `inline` or `instantiate`
// would be read from.
// The collision is the same one [AddonOverrideNames] closes and it is closed the
// same way — the ambiguity is made not to exist rather than resolved, because a
// concatenation offers nothing to resolve it with.
//
// It is a reserved **name** rather than a reserved setting because the variable
// is not per-add-on: there is no add-on it belongs to, so refusing the setting
// `deadline` on every add-on would be a far wider reservation bought for the same
// collision. internal/addon's manifest validation is where it is refused, for the
// reason the override names are refused there — this package is imported by that
// one and not the other way round.
var AddonReservedNames = []string{"inline", "instantiate"}

// AddonOverrides reads the operator's answers about one add-on.
//
// Read by name, exactly like [AddonSettings] and never by scanning the
// environment for a prefix, and for the same reason: a scan would hand an add-on
// every variable under its name including a neighbour's. A variable that is set
// and empty is treated as unset, which is what an operator leaving a line in their
// .env with nothing after the `=` means.
func AddonOverrides(addon string) map[string]string {
	out := make(map[string]string, len(AddonOverrideNames))
	for _, name := range AddonOverrideNames {
		if v := os.Getenv(AddonSettingVar(addon, name)); v != "" {
			out[name] = v
		}
	}
	return out
}

// AddonSettings reads the values an operator configured for one add-on.
//
// Asked for the settings the manifest **declares**, and it reads exactly those —
// never a scan of the environment for a matching prefix. That is not tidiness: a
// prefix scan would hand an add-on every variable under its name, including the
// ones a neighbour's name reaches into, and would have to guess who meant what.
// An add-on that declared nothing reads nothing.
//
// Declaring does not make the *variable* unambiguous, and this comment used to
// say it did. `LINKCTRL_ADDON_OIDC_X_KEY` is `x_key` of `oidc` and `key` of
// `oidc_x` — both legal names — whichever way it is looked up, because the
// variable is a concatenation and the name is one half of it. What resolves it is
// that the two add-ons cannot both be loaded: names standing in a `name + "_"`
// prefix relation are refused at load, in nameCollisions (internal/addon), where
// the same relation closes the cookie namespace. So no two *loaded* add-ons
// produce one variable, which is the property this function needs and the only one
// it has.
//
// A variable that is set and empty is treated as unset, which is what an operator
// leaving a line in their .env with nothing after the `=` means. The add-on then
// gets its declared default, or ErrNotFound.
func AddonSettings(addon string, declared []string) map[string]Secret {
	out := make(map[string]Secret, len(declared))
	for _, name := range declared {
		if v := os.Getenv(AddonSettingVar(addon, name)); v != "" {
			out[name] = Secret(v)
		}
	}
	return out
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
	"MFA_SECRET_KEY",
	"DATABASE_URL",
	"SMTP_PASSWORD",
	"FEED_AUTH_TOKEN",
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
	"DESTINATION_BLOCK_PRIVATE_IPS": "private, loopback, link-local, carrier-NAT " +
		"and cloud-metadata addresses are refused unconditionally since M30. It " +
		"was an off switch on the one tier that must not have one: the person it " +
		"protects is the visitor whose browser would do the fetching, and they " +
		"are not the person who would be turning it off. Point links at an " +
		"intranet with a hostname that resolves there, not with a literal address",
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
// lowercased, with the DNS root dot folded and an explicit default HTTP(S) port
// removed.
//
// The port matters because the two sides of the comparison come from different
// places. The configured value is written by an operator ("manage.example.com")
// and the request value is written by a proxy, which may or may not append
// ":443". Comparing them raw makes the router's behavior depend on that choice.
//
// **A non-default port is kept, deliberately.** SplitHosts compares the app and
// link hosts through this function, and an instance that serves the dashboard and
// short links on one name and two ports is split-host — stripping the port
// unconditionally would collapse it to single-host and take the two trees down to
// one. HostOnly is the spelling for the other question.
//
// **The trailing dot is folded, and it was not (F72, F88).** "lnk.example.com."
// is the fully qualified spelling of "lnk.example.com" and names the same host;
// only storage folded it. `domain.ValidateHostname` drops it before a hostname is
// written and the unique index is on `lower(hostname)`, so a stored name can
// never carry one — which made the mismatch entirely request-side, and made every
// tier that reads a Host header miss together: the split-host router answered its
// ops-only 404, and the single-host mux served a customer's verified hostname the
// dashboard, the API and the *default* domain's aliases. It is reachable over
// HTTPS because SNI carries no trailing dot (RFC 6066), so the handshake completes
// on the certificate for the folded name and Go passes r.Host through unchanged.
func CanonicalHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		// No port, so the whole string is the host and the dot is at its end.
		return strings.TrimSuffix(host, ".")
	}
	h = strings.TrimSuffix(h, ".")
	if port != "80" && port != "443" {
		// JoinHostPort re-adds the brackets an IPv6 literal needs.
		return net.JoinHostPort(h, port)
	}
	// SplitHostPort strips the brackets an IPv6 literal needs to stay parseable.
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}

// HostOnly is CanonicalHost with any port removed as well.
//
// **The two are different questions and F88 is what happens when one function
// answers both.** CanonicalHost asks "is this the host this instance was
// configured with", where the port is part of the configured value and dropping
// it would merge two deployments into one. HostOnly asks "is this a verified
// custom hostname", where there is no port to compare against: `domains.hostname`
// is stored bare, it is validated bare, and the hostname is served on whichever
// port this instance happens to listen on. Keying the verified-host cache through
// CanonicalHost meant `Host: go.customer.example:8080` missed a hostname this
// instance is verified to serve, fell through to the tree behind it, and — on a
// single-host deployment — was answered by the dashboard, the API and the default
// domain's aliases.
//
// It is deliberately *wider* than CanonicalHost rather than differently spelled:
// every host CanonicalHost matches, this matches too. That direction is the safe
// one. The narrower spelling is what fails silently, because a name normalized
// out of the set stops being served while every page goes on saying it is
// verified.
func HostOnly(host string) string {
	host = CanonicalHost(host)
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
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
	// Checked only when set, because unset is a supported state: an instance with
	// no MFA_SECRET_KEY offers no second factor, which is what every instance was
	// before M53. A value too short to be meant seriously is refused rather than
	// hashed into a working key — the derivation accepts anything, so the floor is
	// the only thing that stops `MFA_SECRET_KEY=changeme` producing an instance
	// that looks configured.
	//
	// 32 is written out here rather than imported from auth.MFAKeyMinBytes, for
	// the reason the pepper's floor above is: this package reads the environment
	// for every other package and depends on none of them. The two are held
	// together by TestTheMFAKeyFloorIsTheOneConfigEnforces in internal/auth.
	if !c.MFASecretKey.IsZero() && c.MFASecretKey.Len() < 32 {
		add("MFA_SECRET_KEY: must be at least 32 bytes, got %d (generate: openssl rand -base64 48). "+
			"Losing it locks every enrolled account out of its second factor; they fall back "+
			"to recovery codes.", c.MFASecretKey.Len())
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
	// 303 joined the set when the password gate was made to answer one
	// unconditionally: it is a status this server emits, so an operator who wants
	// every redirect to carry it is asking for something the tree already does on
	// one branch. It is temporary, it is never cached as permanent, and it
	// mandates a GET.
	switch c.Redirect.DefaultStatus {
	case 301, 302, 303, 307, 308:
		if c.Redirect.DefaultStatus == 301 || c.Redirect.DefaultStatus == 308 {
			add("REDIRECT_DEFAULT_STATUS: %d is a permanent redirect and will be cached "+
				"by browsers and intermediaries, so later edits to a link will not take "+
				"effect. Use 302, 303 or 307.", c.Redirect.DefaultStatus)
		}
	default:
		add("REDIRECT_DEFAULT_STATUS: must be 302, 303 or 307, got %d", c.Redirect.DefaultStatus)
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
		"LOGIN_RATE_PER_MIN":       c.Auth.LoginRatePerMin,
		"API_RATE_PER_MIN":         c.Auth.APIRatePerMin,
		"UPLOAD_RATE_PER_MIN":      c.Auth.UploadRatePerMin,
		"REDIRECT_404_RATE_LIMIT":  c.Redirect.NotFoundLimit,
		"LINK_PASSWORD_RATE_LIMIT": c.Redirect.PasswordLimit,
		"LOGIN_LOCKOUT_THRESHOLD":  c.Auth.LockoutThreshold,
	} {
		if v < 0 {
			add("%s: must be 0 (no limit) or positive, got %d", name, v)
		}
	}

	// The scheme allowlist may be narrowed and never widened. An operator who
	// wants https-only destinations is making their instance stricter and is
	// welcome to; an operator who adds "javascript" has re-opened the redirect
	// XSS the allowlist exists to close, on behalf of visitors who never agreed
	// to it. Refused at startup rather than at the first link, because a running
	// instance that accepts javascript: URLs has already accepted some.
	for _, s := range c.Alias.DestSchemes {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "http", "https":
		case "":
			add("DESTINATION_SCHEMES: contains an empty entry; " +
				"check for a stray comma")
		default:
			add("DESTINATION_SCHEMES: %q cannot be permitted. The list may only "+
				"narrow the built-in http,https — non-http(s) schemes are refused "+
				"unappealably, and this variable is not a way around that", s)
		}
	}
	if len(c.Alias.DestSchemes) == 0 {
		add("DESTINATION_SCHEMES: must name at least one of http, https")
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

	// The reputation feed. Every check is inside the Enabled guard, like the
	// mailer's: an instance with no FEED_URL is the default deployment and must
	// not be told about settings it never set.
	//
	// FEED_NAME is required rather than defaulted, and that is the one rule here
	// worth arguing about. A default of "a third party" would let an instance
	// come up sending destinations somewhere its own disclosure page cannot
	// name, which is the exact failure D40's page exists to prevent.
	if c.Feed.Enabled() {
		if u, err := url.Parse(c.Feed.URL); err != nil {
			add("FEED_URL: not a valid URL: %v", err)
		} else {
			if u.Scheme != "https" {
				// https only, and not narrowable by an operator who wants to test
				// against a local endpoint. Destinations are being sent to somebody
				// else's server; sending them in clear as well would make the
				// disclosure's "sent to <third party>" quietly mean "and to whoever
				// is on the path".
				add("FEED_URL: must use https, got %q; destinations are sent to this "+
					"endpoint and must not travel in clear", u.Scheme)
			}
			// A credential in the URL is refused rather than tolerated and
			// redacted (finding F35). Go's client turns userinfo into a Basic
			// auth header, so it *works* and the operator gets no signal that
			// they have put a live credential somewhere every signed-in user
			// reads — /feeds discloses the endpoint by design (D40), and until
			// this release it printed userinfo verbatim.
			//
			// Refusing at boot rather than only stripping it at the disclosure
			// is what keeps FEED_AUTH_TOKEN's discipline meaning something: that
			// one is a Secret, unset from the environment after parsing, and
			// available as a mounted file. A credential smuggled in through the
			// URL gets none of that, so it is the URL that has to be refused.
			if u.User != nil {
				add("FEED_URL: must not carry a username or password in the URL. " +
					"Go sends those as Basic auth, so it would work, and /feeds " +
					"shows every signed-in user where destinations go — use " +
					"FEED_AUTH_HEADER and FEED_AUTH_TOKEN, which are redacted, " +
					"unset from the environment after parsing, and readable from " +
					"a file")
			}
		}
		if c.Feed.Name == "" {
			add("FEED_NAME: is required once FEED_URL is set. It names the third " +
				"party destinations are sent to, and the instance discloses it at " +
				"/feeds and in the docs")
		}
		switch strings.ToUpper(c.Feed.Method) {
		case "GET", "POST":
		default:
			add("FEED_METHOD: must be GET or POST, got %q", c.Feed.Method)
		}
		if c.Feed.Param == "" {
			add("FEED_PARAM: must name the field carrying the destination")
		}
		if c.Feed.VerdictField == "" {
			add("FEED_VERDICT_FIELD: must name the response field holding the verdict")
		}
		if c.Feed.AuthHeader == "" && !c.Feed.AuthToken.IsZero() {
			add("FEED_AUTH_HEADER: cannot be empty when FEED_AUTH_TOKEN is set")
		}
		// Bounded on both sides. Zero or negative would mean no timeout at all
		// on a call made inside a form submission, and anything past the request
		// deadline is a knob whose upper half cannot take effect.
		if c.Feed.Timeout <= 0 {
			add("FEED_TIMEOUT: must be positive, got %s", c.Feed.Timeout)
		} else if c.Feed.Timeout > c.HTTP.RequestTimeout {
			add("FEED_TIMEOUT (%s): must not exceed HTTP_REQUEST_TIMEOUT (%s); a feed "+
				"check happens inside a request somebody is waiting on",
				c.Feed.Timeout, c.HTTP.RequestTimeout)
		}
	}

	// Webhook delivery (M42). Not inside an Enabled guard, because there is no
	// switch: webhooks are a workspace feature rather than an operator one, and
	// these two numbers apply the moment anybody registers one.
	//
	// Bounded on both sides. A timeout of zero would mean one unresponsive
	// receiver holds a delivery slot until the job's own bound expires; the
	// ceiling is what a drain occupies the shared scheduler goroutine for,
	// because a batch is dialled together (webhook.DeliveryConcurrency) and so
	// costs one of these rather than one per row.
	//
	// The batch size and the concurrency limit are both named in prose rather
	// than imported: internal/webhook reaches internal/link for the address
	// predicate, and a config package that imported it would put the whole
	// service graph behind the thing that parses the environment.
	if c.Webhooks.Timeout <= 0 {
		add("WEBHOOK_TIMEOUT: must be positive, got %s", c.Webhooks.Timeout)
	} else if c.Webhooks.Timeout > time.Minute {
		add("WEBHOOK_TIMEOUT (%s): must not exceed 1m; a drain occupies the "+
			"scheduler for this long, and deliveries are attempted on a 30s tick",
			c.Webhooks.Timeout)
	}
	// Zero is refused rather than read as "keep forever". Everywhere else in
	// this file zero means forever (audit retention, D5) and is a deliberate
	// operator choice about a table they can measure; this one grows by a row
	// per link write per enabled webhook, so "forever" here is a decision nobody
	// would make on purpose.
	if c.Webhooks.RetentionDays < 1 {
		add("WEBHOOK_RETENTION_DAYS: must be at least 1, got %d; the delivery log "+
			"is a record of what was attempted, not an archive, and unlike audit "+
			"retention there is no 'keep forever' setting for it",
			c.Webhooks.RetentionDays)
	}

	if c.Analytics.GeoIPPath != "" {
		if _, err := os.Stat(c.Analytics.GeoIPPath); err != nil {
			add("GEOIP_MMDB_PATH: %q is not readable: %v; leave it empty to disable "+
				"geographic reporting", c.Analytics.GeoIPPath, err)
		}
	}

	// Refused at parse time rather than survived at boot, matching GEOIP_MMDB_PATH
	// above and differing from it in one way: a missing GeoIP file disables one
	// report, while a missing add-ons directory is an operator who believes this
	// instance is running modules it has never seen. A path that is a file rather
	// than a directory is the same mistake with a different spelling.
	if c.Addons.Enabled() {
		switch info, err := os.Stat(c.Addons.Dir); {
		case err != nil:
			add("ADDONS_DIR: %q is not readable: %v; leave it empty to run no "+
				"add-ons at all", c.Addons.Dir, err)
		case !info.IsDir():
			add("ADDONS_DIR: %q is not a directory; it holds one directory per "+
				"add-on, each with an addon.json", c.Addons.Dir)
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
