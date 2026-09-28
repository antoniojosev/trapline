package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Password bounds. The minimum is deliberately a length rule and nothing
// else: composition rules ("one digit, one symbol") measurably push people
// towards Password1! while banning the long passphrases that are actually
// strong. The maximum exists only to stop a multi-megabyte body from
// becoming an expensive hash.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 1024
)

// MaxUsernameLen bounds a username so it stays displayable.
const MaxUsernameLen = 64

// Admin is a human account for the panel.
//
// v1 is single-admin plus API tokens with scopes: no organisations, no teams,
// no granular permissions. Those are named in the anti-scope precisely
// because they are what an installation for one developer or a small team
// never needs, and what would drag in an identity subsystem.
type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

// NewAdmin validates a username and a hash that has already been computed.
// The domain never sees a plaintext password beyond ValidatePassword: the
// hashing algorithm is infrastructure, and a plaintext that lingers in a
// domain struct is a plaintext that ends up in a log line.
func NewAdmin(username, passwordHash string, now time.Time) (Admin, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return Admin{}, fmt.Errorf("%w: username is required", ErrInvalidAdmin)
	}
	if utf8.RuneCountInString(username) > MaxUsernameLen {
		return Admin{}, fmt.Errorf("%w: username exceeds %d characters", ErrInvalidAdmin, MaxUsernameLen)
	}
	if strings.ContainsAny(username, " \t\r\n") {
		return Admin{}, fmt.Errorf("%w: username must not contain whitespace", ErrInvalidAdmin)
	}
	if passwordHash == "" {
		return Admin{}, fmt.Errorf("%w: password hash is required", ErrInvalidAdmin)
	}
	return Admin{Username: username, PasswordHash: passwordHash, CreatedAt: now.UTC()}, nil
}

// ValidatePassword checks a plaintext password against the policy, before it
// is hashed.
func ValidatePassword(plain string) error {
	// Counted in runes, not bytes: a passphrase in Spanish with accents would
	// otherwise face a stricter rule than the same phrase without them.
	length := utf8.RuneCountInString(plain)
	if length < MinPasswordLen {
		return fmt.Errorf("%w: password must be at least %d characters", ErrWeakPassword, MinPasswordLen)
	}
	if length > MaxPasswordLen {
		return fmt.Errorf("%w: password must be at most %d characters", ErrWeakPassword, MaxPasswordLen)
	}
	return nil
}
