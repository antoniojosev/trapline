package domain

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestNewSession(t *testing.T) {
	random := bytes.NewReader(bytes.Repeat([]byte{0x42}, SessionTokenBytes))

	session, token, err := NewSession(7, testNow, random)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("the token is not on the struct", func(t *testing.T) {
		// The plaintext token must exist only in the response and the user's
		// cookie. If it were a field, something would eventually persist or
		// log it.
		if strings.Contains(session.TokenHash, token) {
			t.Error("the stored hash contains the token")
		}
		if session.TokenHash != HashSessionToken(token) {
			t.Error("TokenHash is not the hash of the returned token")
		}
	})

	t.Run("the token is URL-safe", func(t *testing.T) {
		// It goes straight into a cookie value, so it must need no further
		// encoding.
		if strings.ContainsAny(token, "+/= ;,") {
			t.Errorf("token %q contains characters that are unsafe in a cookie", token)
		}
	})

	t.Run("expiry is the configured lifetime away", func(t *testing.T) {
		if want := testNow.Add(SessionLifetime); !session.ExpiresAt.Equal(want) {
			t.Errorf("ExpiresAt = %v, want %v", session.ExpiresAt, want)
		}
		if session.AdminID != 7 {
			t.Errorf("AdminID = %d, want 7", session.AdminID)
		}
	})
}

func TestNewSessionRejectsAnUnsavedAdmin(t *testing.T) {
	if _, _, err := NewSession(0, testNow, bytes.NewReader(make([]byte, 64))); !errors.Is(err, ErrInvalidAdmin) {
		t.Error("a session was minted for an admin with no id")
	}
}

func TestNewSessionFailsOnShortRandomness(t *testing.T) {
	// A truncated read must never produce a short, guessable token.
	_, _, err := NewSession(1, testNow, bytes.NewReader([]byte{0x01}))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v, want it to wrap io.ErrUnexpectedEOF", err)
	}
}

func TestSessionTokensAreUnique(t *testing.T) {
	first, firstToken, err := NewSessionToken(1, testNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, secondToken, err := NewSessionToken(1, testNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if firstToken == secondToken || first.TokenHash == second.TokenHash {
		t.Error("two sessions minted identical tokens")
	}
}

func TestHashSessionTokenIsStable(t *testing.T) {
	const token = "un-token-cualquiera"

	// Assigned first: comparing the two calls inline is a tautology the
	// compiler can fold away, which would test nothing.
	first := HashSessionToken(token)
	second := HashSessionToken(token)
	if first != second {
		t.Error("hashing the same token twice gave different results")
	}
	if HashSessionToken(token) == HashSessionToken(token+"x") {
		t.Error("different tokens hashed to the same value")
	}
	if len(HashSessionToken(token)) != 64 {
		t.Errorf("hash length = %d, want 64 hex characters for SHA-256", len(HashSessionToken(token)))
	}
}

func TestSessionExpired(t *testing.T) {
	session := Session{ExpiresAt: testNow}

	cases := map[string]struct {
		now  time.Time
		want bool
	}{
		"before expiry":     {testNow.Add(-time.Second), false},
		"exactly at expiry": {testNow, true},
		"after expiry":      {testNow.Add(time.Second), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := session.Expired(tc.now); got != tc.want {
				t.Errorf("Expired(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}
