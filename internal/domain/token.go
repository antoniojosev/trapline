package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// TokenPrefix marks an API token.
//
// It is not decoration. A recognisable prefix is what lets secret scanners —
// GitHub's, a pre-commit hook, a grep over a config repository — spot a leaked
// credential, and what lets a human reading a log line know instantly what
// they are looking at.
const TokenPrefix = "ek_"

// TokenBytes is the entropy of an API token.
const TokenBytes = 32

// Scope is a permission an API token carries.
type Scope string

// The scopes v1 defines. They are coarse on purpose: v1 is single-admin, so
// fine-grained permissions would be ceremony with nobody to protect from.
// The split that does matter is read from write, so a token handed to a
// dashboard or a read-only agent cannot delete a project.
const (
	ScopeProjectsRead  Scope = "projects:read"
	ScopeProjectsWrite Scope = "projects:write"
	// The alerting surface gets its own pair, and it is the first area that
	// does. Channels hold credentials — a bot token, an SMTP password — so
	// "can read the issue list" and "can read where this installation sends
	// its notifications" are genuinely different permissions, and a token
	// handed to a dashboard should not carry the second. Every route under
	// /alerts/ demands these and never projects:*.
	ScopeAlertsRead  Scope = "alerts:read"
	ScopeAlertsWrite Scope = "alerts:write"
	// Monitors get their own pair for a different reason than alerts do, and
	// each family supplies half of it. A cron monitor holds a ping key, and
	// that key is a credential: anything that can read it can report a backup
	// as successful from anywhere on the internet, so a token minted for a
	// dashboard has no business holding the one thing that can silence a
	// monitor. An uptime monitor is a URL this server will fetch, unattended,
	// forever, from inside the operator's network, so `monitors:write` is
	// also the permission to make this installation issue outbound requests
	// of somebody's choosing — not the same authority as "can create a
	// project", even with the SSRF guard standing behind it (ADR 016).
	ScopeMonitorsRead  Scope = "monitors:read"
	ScopeMonitorsWrite Scope = "monitors:write"
)

// AllScopes lists every scope, for validation and for the widest token.
func AllScopes() []Scope {
	return []Scope{
		ScopeProjectsRead, ScopeProjectsWrite,
		ScopeAlertsRead, ScopeAlertsWrite,
		ScopeMonitorsRead, ScopeMonitorsWrite,
	}
}

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool { return slices.Contains(AllScopes(), s) }

// APIToken is a machine credential.
type APIToken struct {
	TokenHash string
	Name      string
	Scopes    []Scope
	CreatedAt time.Time
	ExpiresAt *time.Time
	LastUsed  *time.Time
}

// NewAPIToken mints a token, returning the record to store and the plaintext
// to show once. As with sessions, the plaintext is never a field: it is
// returned, displayed, and then only its hash exists.
//
// expiresAt is optional. A token with no expiry is a deliberate choice for an
// unattended integration, not an oversight, so it is expressed as an explicit
// nil rather than a far-future date pretending to be forever.
func NewAPIToken(
	name string, scopes []Scope, now time.Time, expiresAt *time.Time, random io.Reader,
) (token APIToken, plaintext string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return APIToken{}, "", fmt.Errorf("%w: name is required", ErrInvalidToken)
	}
	if len(name) > MaxProjectNameLen {
		return APIToken{}, "", fmt.Errorf("%w: name exceeds %d characters", ErrInvalidToken, MaxProjectNameLen)
	}
	if len(scopes) == 0 {
		return APIToken{}, "", fmt.Errorf("%w: at least one scope is required", ErrInvalidToken)
	}
	for _, scope := range scopes {
		if !scope.Valid() {
			return APIToken{}, "", fmt.Errorf("%w: unknown scope %q", ErrInvalidToken, scope)
		}
	}
	if expiresAt != nil && !expiresAt.After(now) {
		return APIToken{}, "", fmt.Errorf("%w: expiry must be in the future", ErrInvalidToken)
	}

	raw := make([]byte, TokenBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return APIToken{}, "", fmt.Errorf("generating token: %w", err)
	}
	plaintext = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	if expiresAt != nil {
		utc := expiresAt.UTC()
		expiresAt = &utc
	}
	return APIToken{
		TokenHash: HashAPIToken(plaintext),
		Name:      name,
		Scopes:    slices.Clone(scopes),
		CreatedAt: now.UTC(),
		ExpiresAt: expiresAt,
	}, plaintext, nil
}

// MintAPIToken mints a token using the system's randomness.
func MintAPIToken(name string, scopes []Scope, now time.Time, expiresAt *time.Time) (APIToken, string, error) {
	return NewAPIToken(name, scopes, now, expiresAt, rand.Reader)
}

// HashAPIToken maps a token to its stored form.
func HashAPIToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Expired reports whether the token is past its expiry at now. A token with
// no expiry never is.
func (t APIToken) Expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

// Allows reports whether the token carries a scope.
func (t APIToken) Allows(scope Scope) bool { return slices.Contains(t.Scopes, scope) }

// EncodeScopes joins scopes for storage.
func EncodeScopes(scopes []Scope) string {
	parts := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		parts = append(parts, string(scope))
	}
	return strings.Join(parts, ",")
}

// DecodeScopes parses stored scopes, dropping anything unrecognised.
//
// Dropping rather than failing is deliberate: a token written by a newer
// build with a scope this one does not know must not become unusable, it must
// simply not be granted the scope it cannot enforce. Failing closed on the
// unknown part while honouring the known ones is the safe reading.
func DecodeScopes(encoded string) []Scope {
	var scopes []Scope
	for _, part := range strings.Split(encoded, ",") {
		scope := Scope(strings.TrimSpace(part))
		if scope.Valid() {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}
