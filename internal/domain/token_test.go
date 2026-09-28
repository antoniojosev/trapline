package domain

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestNewAPIToken(t *testing.T) {
	random := bytes.NewReader(bytes.Repeat([]byte{0x11}, TokenBytes))

	token, plaintext, err := NewAPIToken("cli", AllScopes(), testNow, nil, random)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("the plaintext carries a scannable prefix", func(t *testing.T) {
		// The prefix is what lets a secret scanner or a pre-commit hook spot
		// this credential once it leaks into a repository.
		if !strings.HasPrefix(plaintext, TokenPrefix) {
			t.Errorf("plaintext = %q, want the %q prefix", plaintext, TokenPrefix)
		}
	})

	t.Run("only the hash is kept", func(t *testing.T) {
		if strings.Contains(token.TokenHash, plaintext) {
			t.Error("the stored hash contains the plaintext")
		}
		if token.TokenHash != HashAPIToken(plaintext) {
			t.Error("TokenHash is not the hash of the returned plaintext")
		}
	})

	t.Run("no expiry means never expires", func(t *testing.T) {
		if token.ExpiresAt != nil {
			t.Error("ExpiresAt should be nil when none was asked for")
		}
		if token.Expired(testNow.Add(100 * 365 * 24 * time.Hour)) {
			t.Error("a token with no expiry expired")
		}
	})

	t.Run("scopes are copied, not aliased", func(t *testing.T) {
		scopes := []Scope{ScopeProjectsRead}
		built, _, err := NewAPIToken("x", scopes, testNow, nil, bytes.NewReader(make([]byte, TokenBytes)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		scopes[0] = ScopeProjectsWrite
		if built.Allows(ScopeProjectsWrite) {
			t.Error("mutating the caller's slice changed the token's scopes")
		}
	})
}

func TestAPITokenExpiry(t *testing.T) {
	expiry := testNow.Add(time.Hour)
	token, _, err := NewAPIToken("temporal", []Scope{ScopeProjectsRead}, testNow, &expiry, bytes.NewReader(make([]byte, TokenBytes)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := map[string]struct {
		now  time.Time
		want bool
	}{
		"before": {testNow, false},
		"at":     {expiry, true},
		"after":  {expiry.Add(time.Second), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := token.Expired(tc.now); got != tc.want {
				t.Errorf("Expired = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewAPITokenRejections(t *testing.T) {
	past := testNow.Add(-time.Hour)
	cases := map[string]struct {
		name    string
		scopes  []Scope
		expires *time.Time
	}{
		"empty name":      {"", AllScopes(), nil},
		"whitespace name": {"   ", AllScopes(), nil},
		"long name":       {strings.Repeat("a", MaxProjectNameLen+1), AllScopes(), nil},
		"no scopes":       {"cli", nil, nil},
		"unknown scope":   {"cli", []Scope{"projects:destroy"}, nil},
		"past expiry":     {"cli", AllScopes(), &past},
		"expiry now":      {"cli", AllScopes(), &testNow},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := NewAPIToken(tc.name, tc.scopes, testNow, tc.expires, bytes.NewReader(make([]byte, TokenBytes)))
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("error = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestNewAPITokenFailsOnShortRandomness(t *testing.T) {
	_, _, err := NewAPIToken("cli", AllScopes(), testNow, nil, bytes.NewReader([]byte{0x01}))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v, want it to wrap io.ErrUnexpectedEOF", err)
	}
}

func TestTokensAreUnique(t *testing.T) {
	first, firstPlain, err := MintAPIToken("a", AllScopes(), testNow, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, secondPlain, err := MintAPIToken("b", AllScopes(), testNow, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if firstPlain == secondPlain || first.TokenHash == second.TokenHash {
		t.Error("two tokens came out identical")
	}
}

func TestScopeAllows(t *testing.T) {
	readOnly := APIToken{Scopes: []Scope{ScopeProjectsRead}}

	if !readOnly.Allows(ScopeProjectsRead) {
		t.Error("a read scope does not allow reading")
	}
	// The read/write split is the one distinction that matters in a
	// single-admin product: a read-only token must not delete a project.
	if readOnly.Allows(ScopeProjectsWrite) {
		t.Error("a read-only token allows writing")
	}
}

func TestScopeRoundTrip(t *testing.T) {
	encoded := EncodeScopes(AllScopes())
	decoded := DecodeScopes(encoded)

	if len(decoded) != len(AllScopes()) {
		t.Fatalf("round trip gave %d scopes, want %d", len(decoded), len(AllScopes()))
	}
	for i, scope := range AllScopes() {
		if decoded[i] != scope {
			t.Errorf("scope %d = %q, want %q", i, decoded[i], scope)
		}
	}
}

func TestDecodeScopesDropsUnknownOnes(t *testing.T) {
	// A token written by a newer build must not become unusable; it simply is
	// not granted a scope this build cannot enforce.
	decoded := DecodeScopes("projects:read, projects:teleport ,,projects:write")

	if len(decoded) != 2 {
		t.Fatalf("got %v, want the two known scopes", decoded)
	}
	for _, scope := range decoded {
		if !scope.Valid() {
			t.Errorf("an unknown scope %q survived decoding", scope)
		}
	}
}
