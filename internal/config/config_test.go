package config

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

func noEnv(string) string { return "" }

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(nil, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	// Defaulting to loopback rather than 0.0.0.0 is a security decision, not
	// a convenience: a fresh install must not be publicly reachable by
	// accident.
	if cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("the default listen address %q is not loopback", cfg.Addr)
	}
	if cfg.DBPath != DefaultDBPath {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, DefaultDBPath)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, want none by default", cfg.TrustedProxies)
	}
	if !cfg.OriginIsLocal() {
		t.Error("the derived default origin should be recognised as local")
	}
}

func TestFlagsBeatEnvironment(t *testing.T) {
	// A stale exported variable must not override what the operator typed.
	cfg, err := Load(
		[]string{"-addr", "0.0.0.0:8080", "-db", "/data/trapline.db"},
		env(map[string]string{"TRAPLINE_ADDR": "127.0.0.1:1", "TRAPLINE_DB": "/ignored.db"}),
		io.Discard,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Addr != "0.0.0.0:8080" {
		t.Errorf("Addr = %q, want the flag to win", cfg.Addr)
	}
	if cfg.DBPath != "/data/trapline.db" {
		t.Errorf("DBPath = %q, want the flag to win", cfg.DBPath)
	}
}

func TestEnvironmentBeatsDefaults(t *testing.T) {
	cfg, err := Load(nil, env(map[string]string{
		"TRAPLINE_ADDR":            "0.0.0.0:7000",
		"TRAPLINE_ORIGIN":          "https://errors.example.com",
		"TRAPLINE_TRUSTED_PROXIES": " 10.0.0.1 , 10.0.0.2 ,, ",
		"TRAPLINE_DEBUG":           "yes",
	}), io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Addr != "0.0.0.0:7000" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.Origin.String() != "https://errors.example.com" {
		t.Errorf("Origin = %q", cfg.Origin.String())
	}
	if cfg.OriginIsLocal() {
		t.Error("a configured public origin was reported as local")
	}
	if got := cfg.TrustedProxies; len(got) != 2 || got[0] != "10.0.0.1" || got[1] != "10.0.0.2" {
		t.Errorf("TrustedProxies = %v, want the list trimmed with blanks dropped", got)
	}
	if !cfg.Debug {
		t.Error("Debug should be on")
	}
}

func TestOriginIsValidated(t *testing.T) {
	_, err := Load([]string{"-origin", "errors.example.com"}, noEnv, io.Discard)
	if !errors.Is(err, domain.ErrInvalidOrigin) {
		t.Errorf("error = %v, want ErrInvalidOrigin", err)
	}
}

func TestOriginIsLocalDetection(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:9000":      true,
		"http://127.0.0.1:9000":      true,
		"http://0.0.0.0:9000":        true,
		"http://[::1]:9000":          true,
		"https://errors.example.com": false,
		"https://192.168.1.10:8080":  false,
	}
	for raw, wantLocal := range cases {
		t.Run(raw, func(t *testing.T) {
			cfg, err := Load([]string{"-origin", raw}, noEnv, io.Discard)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.OriginIsLocal(); got != wantLocal {
				t.Errorf("OriginIsLocal() = %v, want %v", got, wantLocal)
			}
		})
	}
}

func TestUnknownFlagFails(t *testing.T) {
	if _, err := Load([]string{"-nope"}, noEnv, io.Discard); err == nil {
		t.Error("an unknown flag was accepted")
	}
}

func TestPerAddressLimitsDefaultToTheDomainsFigures(t *testing.T) {
	// The defaults are derived in the domain, next to the design volume they
	// come from, so that a limit and the throughput it is supposed to permit
	// cannot drift apart. Config's job is to pass them through, and this is
	// the assertion that it does.
	cfg, err := Load(nil, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.IngestIPRateLimitPerMinute != domain.DefaultIngestIPRateLimitPerMinute {
		t.Errorf("IngestIPRateLimitPerMinute = %d, want %d",
			cfg.IngestIPRateLimitPerMinute, domain.DefaultIngestIPRateLimitPerMinute)
	}
	if cfg.AuthRateLimitPerMinute != domain.DefaultAuthRateLimitPerMinute {
		t.Errorf("AuthRateLimitPerMinute = %d, want %d",
			cfg.AuthRateLimitPerMinute, domain.DefaultAuthRateLimitPerMinute)
	}
}

func TestPerAddressLimitsAreConfigurable(t *testing.T) {
	cfg, err := Load([]string{"-auth-rate-limit", "3"}, env(map[string]string{
		"TRAPLINE_INGEST_IP_RATE_LIMIT": "5000",
		"TRAPLINE_AUTH_RATE_LIMIT":      "999",
	}), io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.IngestIPRateLimitPerMinute != 5000 {
		t.Errorf("IngestIPRateLimitPerMinute = %d, want the environment's 5000", cfg.IngestIPRateLimitPerMinute)
	}
	if cfg.AuthRateLimitPerMinute != 3 {
		t.Errorf("AuthRateLimitPerMinute = %d, want the flag to beat the environment", cfg.AuthRateLimitPerMinute)
	}
}

func TestABadPerAddressLimitStopsTheServer(t *testing.T) {
	// Silently falling back would give an operator who typed "1_000" the
	// default ceiling while their shell insists otherwise, and the difference
	// is only visible during the attack the setting exists for.
	cases := map[string][]any{
		"a flag that is not a number":       {[]string{"-auth-rate-limit", "-4"}, noEnv},
		"an environment value with a comma": {[]string(nil), env(map[string]string{"TRAPLINE_AUTH_RATE_LIMIT": "1,000"})},
		"a negative environment value":      {[]string(nil), env(map[string]string{"TRAPLINE_INGEST_IP_RATE_LIMIT": "-1"})},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			flags, _ := args[0].([]string)
			getenv, _ := args[1].(func(string) string)
			if _, err := Load(flags, getenv, io.Discard); err == nil {
				t.Error("a malformed rate limit was accepted")
			}
		})
	}
}

func TestATypoInTheTrustedProxiesStopsTheServer(t *testing.T) {
	// The one setting here that can be wrong in a dangerous direction. An
	// operator who believes they configured it and did not gets every visitor
	// counted as the proxy, which looks like a working rate limiter.
	for _, entry := range []string{"not-an-address", "proxy.example.com", "10.0.0.0/99"} {
		t.Run(entry, func(t *testing.T) {
			_, err := Load([]string{"-trusted-proxies", entry}, noEnv, io.Discard)
			if err == nil {
				t.Fatalf("Load accepted %q as a trusted proxy", entry)
			}
			if !strings.Contains(err.Error(), entry) {
				t.Errorf("error %q does not name the offending entry", err)
			}
		})
	}
}

func TestTrustedProxiesAcceptAddressesAndRanges(t *testing.T) {
	cfg, err := Load([]string{"-trusted-proxies", "10.0.0.1, 172.16.0.0/12, ::1, 2001:db8::/32"}, noEnv, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.TrustedProxies) != 4 {
		t.Errorf("TrustedProxies = %v, want four entries", cfg.TrustedProxies)
	}
}
