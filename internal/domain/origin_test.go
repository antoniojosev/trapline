package domain

import (
	"errors"
	"testing"
)

func TestParseOrigin(t *testing.T) {
	cases := map[string]Origin{
		"https://errors.example.com": {Scheme: "https", Host: "errors.example.com"},
		"http://localhost:9000":      {Scheme: "http", Host: "localhost:9000"},
		"  https://example.com/  ":   {Scheme: "https", Host: "example.com"},
		"https://192.168.1.10:8080":  {Scheme: "https", Host: "192.168.1.10:8080"},
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			got, err := ParseOrigin(raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != want {
				t.Errorf("ParseOrigin(%q) = %+v, want %+v", raw, got, want)
			}
		})
	}
}

func TestParseOriginRejections(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"whitespace":   "   ",
		"no scheme":    "errors.example.com",
		"wrong scheme": "ftp://errors.example.com",
		"no host":      "https://",
		"with path":    "https://errors.example.com/trapline",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseOrigin(raw); !errors.Is(err, ErrInvalidOrigin) {
				t.Errorf("ParseOrigin(%q) error = %v, want ErrInvalidOrigin", raw, err)
			}
		})
	}
}

func TestOriginDSNFor(t *testing.T) {
	origin := Origin{Scheme: "https", Host: "errors.example.com"}
	key := Key{PublicKey: "abababababababababababababababab", ProjectID: 7}

	dsn := origin.DSNFor(key)

	if got, want := dsn.String(), "https://abababababababababababababababab@errors.example.com/7"; got != want {
		t.Errorf("DSN = %q, want %q", got, want)
	}

	// A DSN must survive the round trip through the parser, otherwise the
	// panel could display something the tooling cannot read back.
	parsed, err := ParseDSN(dsn.String())
	if err != nil {
		t.Fatalf("the DSN we generated does not parse: %v", err)
	}
	if parsed != dsn {
		t.Errorf("round trip changed the DSN: %+v became %+v", dsn, parsed)
	}
}

func TestOriginString(t *testing.T) {
	// Configuration written by a human must round-trip too.
	const raw = "https://errors.example.com"
	origin, err := ParseOrigin(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if origin.String() != raw {
		t.Errorf("String() = %q, want %q", origin.String(), raw)
	}
}
