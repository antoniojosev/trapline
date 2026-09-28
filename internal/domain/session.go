package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// SessionTokenBytes is the entropy of a session token. 32 bytes is far past
// any brute-force concern and keeps the cookie short.
const SessionTokenBytes = 32

// SessionLifetime is how long a panel session lasts. Long enough not to be
// annoying for an operator checking errors through the day, short enough that
// a forgotten open tab does not stay valid for weeks.
const SessionLifetime = 12 * time.Hour

// Session is an authenticated panel session.
//
// TokenHash, not the token: the plaintext token exists only in the response
// that creates it and in the user's cookie. A database dump must not be a
// collection of working credentials, which is exactly what storing tokens
// verbatim would make it.
type Session struct {
	TokenHash string
	AdminID   int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewSession mints a session and returns it alongside the plaintext token to
// hand to the client. The token is returned separately, and never stored on
// the struct, so there is no way to accidentally persist or log it.
func NewSession(adminID int64, now time.Time, random io.Reader) (session Session, token string, err error) {
	if adminID <= 0 {
		return Session{}, "", fmt.Errorf("%w: admin id must be positive", ErrInvalidAdmin)
	}
	raw := make([]byte, SessionTokenBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return Session{}, "", fmt.Errorf("generating session token: %w", err)
	}

	// URL-safe so it is a valid cookie value without further encoding.
	token = base64.RawURLEncoding.EncodeToString(raw)

	return Session{
		TokenHash: HashSessionToken(token),
		AdminID:   adminID,
		CreatedAt: now.UTC(),
		ExpiresAt: now.UTC().Add(SessionLifetime),
	}, token, nil
}

// NewSessionToken mints a session using the system's randomness.
func NewSessionToken(adminID int64, now time.Time) (Session, string, error) {
	return NewSession(adminID, now, rand.Reader)
}

// HashSessionToken maps a token to its stored form.
//
// A plain SHA-256 is right here, unlike for a password: a session token is
// 32 bytes of cryptographic randomness, so there is no dictionary to attack
// and a slow hash would only tax every authenticated request.
func HashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Expired reports whether the session is no longer valid at now.
func (s Session) Expired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}
