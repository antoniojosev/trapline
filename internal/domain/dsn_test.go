package domain

import (
	"errors"
	"testing"
)

func TestDSNString(t *testing.T) {
	dsn := DSN{Scheme: "https", PublicKey: "abababababababababababababababab", Host: "errors.example.com", ProjectID: 7}

	if got, want := dsn.String(), "https://abababababababababababababababab@errors.example.com/7"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := dsn.IngestURL(), "https://errors.example.com/api/7/envelope/"; got != want {
		t.Errorf("IngestURL() = %q, want %q", got, want)
	}
}

func TestParseDSNRoundTrip(t *testing.T) {
	// The round trip is the contract that matters: whatever the panel shows a
	// user must be exactly what the tooling can read back.
	cases := []DSN{
		{Scheme: "https", PublicKey: "abababababababababababababababab", Host: "errors.example.com", ProjectID: 1},
		{Scheme: "http", PublicKey: "0123456789abcdef0123456789abcdef", Host: "localhost:9000", ProjectID: 42},
		{Scheme: "https", PublicKey: "ffffffffffffffffffffffffffffffff", Host: "127.0.0.1:8080", ProjectID: 9007199254740991},
	}
	for _, want := range cases {
		t.Run(want.String(), func(t *testing.T) {
			got, err := ParseDSN(want.String())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != want {
				t.Errorf("ParseDSN(%q) = %+v, want %+v", want.String(), got, want)
			}
		})
	}
}

func TestParseDSNTolerance(t *testing.T) {
	t.Run("tolerates surrounding whitespace", func(t *testing.T) {
		// Users paste DSNs. A trailing newline is not a malformed DSN.
		if _, err := ParseDSN("  https://abababababababababababababababab@errors.example.com/7\n"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("tolerates a trailing slash", func(t *testing.T) {
		got, err := ParseDSN("https://abababababababababababababababab@errors.example.com/7/")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ProjectID != 7 {
			t.Errorf("ProjectID = %d, want 7", got.ProjectID)
		}
	})
}

func TestParseDSNRejections(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"no scheme":          "abababababababababababababababab@errors.example.com/7",
		"wrong scheme":       "ftp://abababababababababababababababab@errors.example.com/7",
		"no public key":      "https://errors.example.com/7",
		"empty public key":   "https://@errors.example.com/7",
		"legacy secret key":  "https://public:secret@errors.example.com/7",
		"no project id":      "https://abababababababababababababababab@errors.example.com",
		"non numeric id":     "https://abababababababababababababababab@errors.example.com/seven",
		"zero project id":    "https://abababababababababababababababab@errors.example.com/0",
		"negative id":        "https://abababababababababababababababab@errors.example.com/-1",
		"id with path depth": "https://abababababababababababababababab@errors.example.com/7/extra",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDSN(raw); !errors.Is(err, ErrInvalidDSN) {
				t.Errorf("ParseDSN(%q) error = %v, want ErrInvalidDSN", raw, err)
			}
		})
	}
}
