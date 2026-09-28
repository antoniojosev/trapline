package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// PublicKeyLen is the length in hex characters of a DSN public key. It
// matches the width official SDKs are known to accept.
const PublicKeyLen = 32

// MaxActiveKeys is 2 so a key can be rotated without downtime: the new key
// is issued, deployments pick it up, and only then is the old one revoked.
// Allowing more would turn "rotation" into "an unbounded pile of live
// credentials", which is the state rotation exists to avoid.
const MaxActiveKeys = 2

// Key is a DSN credential for one project.
//
// It is deliberately not hashed at rest, unlike an admin password or an API
// token. A DSN public key ships inside browser bundles and mobile apps: it
// is a public identifier, not a secret. Its only authority is "append
// events to this project", and the panel has to be able to display it back
// to the user. Treating it as a secret would be security theatre — the real
// controls are rate limiting and spike protection (SECURITY.md).
type Key struct {
	PublicKey string
	ProjectID int64
	CreatedAt time.Time
	RevokedAt *time.Time
}

// MintKey creates a key that is not yet bound to a project.
//
// Minting and assigning are separate steps because of an ordering fact: a
// project's id is assigned by the store, but the key's randomness must be
// generated in the domain. Splitting them lets a store create a project and
// its first key in one transaction without generating secrets itself.
//
// random is injected so tests are deterministic; production passes
// crypto/rand.Reader.
func MintKey(now time.Time, random io.Reader) (Key, error) {
	raw := make([]byte, PublicKeyLen/2)
	if _, err := io.ReadFull(random, raw); err != nil {
		return Key{}, fmt.Errorf("generating key: %w", err)
	}
	return Key{
		PublicKey: hex.EncodeToString(raw),
		CreatedAt: now.UTC(),
	}, nil
}

// NewKey mints an unassigned key using the system's cryptographic randomness.
func NewKey(now time.Time) (Key, error) {
	return MintKey(now, rand.Reader)
}

// AssignTo binds an unassigned key to a project.
func (k Key) AssignTo(projectID int64) (Key, error) {
	if projectID <= 0 {
		return Key{}, fmt.Errorf("%w: project id must be positive", ErrInvalidProject)
	}
	if k.PublicKey == "" {
		return Key{}, fmt.Errorf("%w: key has not been minted", ErrInvalidProject)
	}
	k.ProjectID = projectID
	return k, nil
}

// GenerateKey mints a key already bound to a project.
func GenerateKey(projectID int64, now time.Time, random io.Reader) (Key, error) {
	key, err := MintKey(now, random)
	if err != nil {
		return Key{}, err
	}
	return key.AssignTo(projectID)
}

// Active reports whether the key may still authenticate ingestion.
func (k Key) Active() bool { return k.RevokedAt == nil }

// Revoke marks the key unusable. Revoking an already revoked key keeps the
// original timestamp: the first revocation is the one that matters, and
// making this idempotent lets a retried CLI call be safe.
func (k Key) Revoke(now time.Time) Key {
	if k.RevokedAt != nil {
		return k
	}
	revoked := now.UTC()
	k.RevokedAt = &revoked
	return k
}

// NewEventID mints an identifier for an accepted event.
//
// 32 hex characters with no dashes, which is the shape the protocol uses and
// what SDKs log when asked to be verbose.
func NewEventID() string {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		// The system's randomness failing is not a condition a caller can act
		// on, and an event id is not a security boundary — it is a handle for
		// a log line. A timestamp-derived fallback keeps ingestion working.
		return hex.EncodeToString([]byte(time.Now().UTC().Format("20060102150405.000000000")))[:32]
	}
	return hex.EncodeToString(raw)
}
