// Package config resolves the server's settings from flags and environment.
package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/clientip"
	"github.com/antoniojosev/trapline/internal/domain"
)

// Defaults. The database path is relative on purpose: running the binary in a
// directory and getting a working install is the whole `curl | sh` promise.
// The systemd unit overrides it with a proper state directory.
const (
	DefaultAddr    = "127.0.0.1:9000"
	DefaultDBPath  = "trapline.db"
	DefaultTimeout = 30 * time.Second
	// DefaultSourceMapCacheMB matches sourcemap.DefaultCacheBytes. It is
	// restated here rather than imported so this package keeps depending on
	// nothing but the domain, and the test that pins the two together is what
	// stops them drifting.
	DefaultSourceMapCacheMB = 64
)

// Config is the resolved server configuration.
type Config struct {
	// Addr is the listen address. It defaults to loopback, not 0.0.0.0:
	// a fresh install must not become publicly reachable because someone
	// ran it before reading the docs. Exposing it is an explicit act.
	Addr string

	// DBPath is the single SQLite file holding all state.
	DBPath string

	// Origin is the public scheme and host that SDKs send events to, and the
	// only source for the DSNs shown in the panel. It cannot be inferred from
	// requests: behind a proxy the request says loopback while the DSN must
	// say the real domain.
	Origin domain.Origin

	// TrustedProxies are the addresses whose X-Forwarded-For header is
	// believed. Addresses or CIDR blocks, either family. Empty means trust
	// nothing and rate-limit by the direct peer. Without this, per-IP limits
	// behind a proxy either count every request against the proxy or accept
	// spoofed client addresses (ADR 023, SECURITY.md).
	TrustedProxies []string

	// IngestIPRateLimitPerMinute caps ingest requests per client address.
	// Deliberately generous: a backend's whole legitimate volume arrives from
	// one address, so this is a ceiling on abuse, not a quota (ADR 023).
	IngestIPRateLimitPerMinute int

	// AuthRateLimitPerMinute caps attempts against /setup and /login per
	// client address. Deliberately strict: every attempt costs an Argon2id
	// working set, so this rations memory rather than bandwidth.
	AuthRateLimitPerMinute int

	// SecretKeyFile is where the key that encrypts alert channel credentials
	// lives. Empty means `<DBPath>.key`, which is what an installation gets
	// without configuring anything. It is separate from the database on
	// purpose and that has a consequence worth knowing before the day it
	// matters: `trapline backup` copies the database and not this file
	// (ADR 015).
	SecretKeyFile string

	// ScrubKeys are extra field names whose values are removed before
	// anything is persisted. An installation knows its own secrets — an
	// internal header name, an application-specific id — and the shipped list
	// cannot, so it has to be extensible or it will be wrong for everyone in
	// a different way.
	ScrubKeys []string

	// Debug turns on the ingest decision log: why an envelope was dropped,
	// limited or accepted. "Why did my event not show up?" is the most
	// expensive question a user can ask, and this is the answer.
	Debug bool

	// UptimeAllowPrivate lets uptime monitors reach addresses that are not
	// globally routable — loopback, RFC 1918, link-local, the cloud metadata
	// endpoint.
	//
	// It is off by default and it is only half of the permission: a monitor
	// must also carry allow_private, and neither is sufficient alone. Two
	// switches rather than one because the two are held by different people.
	// Whoever runs the server decides whether this installation is allowed
	// into its own network at all; whoever writes a monitor decides whether
	// this particular check needs it. A single flag would mean that turning
	// it on for one internal service silently opens every monitor anybody
	// adds afterwards, including one pointed at 169.254.169.254 (ADR 016,
	// SECURITY.md).
	UptimeAllowPrivate bool

	// SourceMapCacheMB is how much memory parsed source maps may occupy.
	//
	// A budget in megabytes rather than a number of maps, because the size of
	// a parsed map is chosen by whoever uploads it: one bundle with embedded
	// sources is tens of megabytes and a vendor chunk is tens of kilobytes, so
	// "keep 200 maps" is a memory limit set by somebody else. Zero means the
	// default, and the default sits inside the product's 80 MB peak rather
	// than on top of it (ADR 018).
	SourceMapCacheMB int

	// SessionWindowSize is the ceiling on sessions tracked in memory at once,
	// and zero means domain.DefaultSessionWindowSize.
	//
	// It is configurable for two reasons and neither of them is tuning. A
	// machine with less memory than this product assumes should be able to
	// lower it; and the gate that proves ADR 008's promise — that a full
	// window costs precision and never stability — has to be able to fill one
	// without sending fifty thousand sessions.
	SessionWindowSize int
}

// Load resolves configuration from arguments and the environment.
//
// Precedence is flag, then environment, then default. Flags win because they
// are the more specific, more visible statement of intent, and because a
// stale exported variable in a shell should not silently override what the
// operator just typed.
func Load(args []string, getenv func(string) string, stderr io.Writer) (Config, error) {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)

	addr := flags.String("addr", "", "listen address (env TRAPLINE_ADDR)")
	dbPath := flags.String("db", "", "path to the SQLite database (env TRAPLINE_DB)")
	origin := flags.String("origin", "", "public origin used in DSNs, e.g. https://errors.example.com (env TRAPLINE_ORIGIN)")
	trusted := flags.String("trusted-proxies", "", "comma-separated proxy addresses or CIDR blocks whose X-Forwarded-For is trusted (env TRAPLINE_TRUSTED_PROXIES)")
	ingestIPLimit := flags.Int("ingest-ip-rate-limit", 0,
		"ingest requests allowed per minute per client address, 0 for the default (env TRAPLINE_INGEST_IP_RATE_LIMIT)")
	authLimit := flags.Int("auth-rate-limit", 0,
		"login attempts allowed per minute per client address, 0 for the default (env TRAPLINE_AUTH_RATE_LIMIT)")
	secretKeyFile := flags.String("secret-key-file", "",
		"file holding the key that encrypts alert channel credentials, default <db>.key (env TRAPLINE_SECRET_KEY_FILE)")
	scrubKeys := flags.String("scrub-keys", "", "extra comma-separated field names to redact before storing (env TRAPLINE_SCRUB_KEYS)")
	debug := flags.Bool("debug", false, "log ingest decisions (env TRAPLINE_DEBUG)")
	sourceMapCacheMB := flags.Int("sourcemap-cache-mb", 0,
		"memory budget for parsed source maps in MB, 0 for the default (env TRAPLINE_SOURCEMAP_CACHE_MB)")
	uptimeAllowPrivate := flags.Bool("uptime-allow-private", false,
		"let uptime monitors that ask for it reach private, loopback and link-local addresses "+
			"(env TRAPLINE_UPTIME_ALLOW_PRIVATE)")
	sessionWindow := flags.Int("session-window", 0,
		"how many sessions release health tracks in memory at once, 0 for the default "+
			"(env TRAPLINE_SESSION_WINDOW)")

	if err := flags.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parsing flags: %w", err)
	}

	cfg := Config{
		Addr:   firstNonEmpty(*addr, getenv("TRAPLINE_ADDR"), DefaultAddr),
		DBPath: firstNonEmpty(*dbPath, getenv("TRAPLINE_DB"), DefaultDBPath),
		Debug:  *debug || isTruthy(getenv("TRAPLINE_DEBUG")),
		UptimeAllowPrivate: *uptimeAllowPrivate ||
			isTruthy(getenv("TRAPLINE_UPTIME_ALLOW_PRIVATE")),
	}

	cfg.SecretKeyFile = firstNonEmpty(*secretKeyFile, getenv("TRAPLINE_SECRET_KEY_FILE"))

	rawProxies := firstNonEmpty(*trusted, getenv("TRAPLINE_TRUSTED_PROXIES"))
	for _, proxy := range strings.Split(rawProxies, ",") {
		if trimmed := strings.TrimSpace(proxy); trimmed != "" {
			cfg.TrustedProxies = append(cfg.TrustedProxies, trimmed)
		}
	}
	// Validated here rather than where it is used, so a typo stops the server
	// with the offending entry named. Accepting it silently would leave an
	// operator believing they had configured something they had not, and the
	// symptom — every client counted as the proxy — looks like a working rate
	// limiter right up until it matters.
	if err := clientip.Validate(cfg.TrustedProxies); err != nil {
		return Config{}, err
	}

	ingestPerMinute, err := positiveOrDefault(
		*ingestIPLimit, getenv("TRAPLINE_INGEST_IP_RATE_LIMIT"),
		"ingest-ip-rate-limit", domain.DefaultIngestIPRateLimitPerMinute)
	if err != nil {
		return Config{}, err
	}
	cfg.IngestIPRateLimitPerMinute = ingestPerMinute

	authPerMinute, err := positiveOrDefault(
		*authLimit, getenv("TRAPLINE_AUTH_RATE_LIMIT"),
		"auth-rate-limit", domain.DefaultAuthRateLimitPerMinute)
	if err != nil {
		return Config{}, err
	}
	cfg.AuthRateLimitPerMinute = authPerMinute

	cacheMB, err := positiveOrDefault(
		*sourceMapCacheMB, getenv("TRAPLINE_SOURCEMAP_CACHE_MB"),
		"sourcemap-cache-mb", DefaultSourceMapCacheMB)
	if err != nil {
		return Config{}, err
	}
	cfg.SourceMapCacheMB = cacheMB

	sessionWindowSize, err := positiveOrDefault(
		*sessionWindow, getenv("TRAPLINE_SESSION_WINDOW"),
		"session-window", domain.DefaultSessionWindowSize)
	if err != nil {
		return Config{}, err
	}
	cfg.SessionWindowSize = sessionWindowSize

	for _, key := range strings.Split(firstNonEmpty(*scrubKeys, getenv("TRAPLINE_SCRUB_KEYS")), ",") {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			cfg.ScrubKeys = append(cfg.ScrubKeys, trimmed)
		}
	}

	rawOrigin := firstNonEmpty(*origin, getenv("TRAPLINE_ORIGIN"))
	if rawOrigin == "" {
		// Derived from the listen address so a local run works with no
		// configuration at all. Any real deployment sets it, and the panel
		// says so rather than handing out a DSN pointing at localhost.
		rawOrigin = "http://" + cfg.Addr
	}
	parsed, err := domain.ParseOrigin(rawOrigin)
	if err != nil {
		return Config{}, err
	}
	cfg.Origin = parsed

	return cfg, nil
}

// OriginIsLocal reports whether the origin still points at a loopback or
// unspecified address, meaning it was almost certainly never configured. The
// server warns about it at startup instead of silently issuing DSNs no SDK
// outside the machine can reach.
// The rule itself lives on the origin, because the same question is asked
// wherever this product hands out a URL meant to be used somewhere else —
// a DSN, and now the links inside the weekly digest.
func (c *Config) OriginIsLocal() bool { return c.Origin.IsLocal() }

// LookupEnv is os.Getenv, passed to Load in production and replaced in tests.
func LookupEnv(key string) string { return os.Getenv(key) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// positiveOrDefault resolves a numeric setting from a flag, then the
// environment, then the default.
//
// A malformed environment variable is an error rather than a silent fallback.
// These two settings are rate limits: falling back would hand an operator who
// typed "1_000" the default ceiling while their shell insists otherwise, and
// the difference is only visible under the attack the setting was meant to
// survive. Zero means "use the default" and is not an error, because that is
// what an unset flag looks like.
func positiveOrDefault(flagValue int, envValue, name string, fallback int) (int, error) {
	if flagValue < 0 {
		return 0, fmt.Errorf("-%s must be positive, got %d", name, flagValue)
	}
	if flagValue > 0 {
		return flagValue, nil
	}

	raw := strings.TrimSpace(envValue)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %q is not a number", name, raw)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must be positive, got %d", name, parsed)
	}
	if parsed == 0 {
		return fallback, nil
	}
	return parsed, nil
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
