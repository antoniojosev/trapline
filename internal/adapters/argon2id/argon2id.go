// Package argon2id hashes admin passwords with Argon2id.
//
// Argon2id rather than bcrypt because it is memory-hard: an attacker with a
// GPU farm gains far less than against a purely CPU-bound function. It won
// the Password Hashing Competition and is the current default recommendation.
//
// Note this is only for human passwords. DSN keys and API tokens are
// high-entropy random values where a slow hash buys nothing and costs a lot
// on every request — those use a fast hash or, for DSN public keys, none at
// all (SECURITY.md).
package argon2id

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Parameters for hashing: OWASP's current recommendation for Argon2id.
//
// Measured, not assumed. RFC 9106's memory-constrained option (64 MiB, t=3)
// was the first choice here and it broke the product's headline: a single
// login took resident memory from 9.6 MB to 142 MB and it never came back,
// because Go does not return a freed heap to the operating system on its own.
// A footprint claim that survives only until someone logs in is not a claim.
//
// 19 MiB with two passes is strong — it is the figure OWASP publishes as the
// minimum for Argon2id — and it is a working set this product can afford.
// Combined with the scavenge below, a login no longer leaves a permanent mark.
const (
	memoryKiB   = 19 * 1024
	iterations  = 2
	parallelism = 1
	saltLen     = 16
	keyLen      = 32
)

// ErrInvalidHash means a stored hash is not in the expected encoding.
var ErrInvalidHash = errors.New("invalid password hash")

// Hasher hashes and verifies passwords.
type Hasher struct{}

// New returns a hasher.
func New() Hasher { return Hasher{} }

// scavengeAfter releases the hashing working set back to the operating system.
//
// This is a deliberate exception to "never call debug.FreeOSMemory". Argon2id
// is memory-hard by design, so every hash allocates megabytes on purpose; Go
// then keeps that heap for reuse, and resident memory stays at the high-water
// mark forever. For a product whose first promise is a small footprint, a
// login must not permanently change the answer.
//
// It is coalesced rather than immediate. Calling it inline would hand an
// attacker a lever: a flood of login attempts would force one stop-the-world
// scavenge per request, turning a rate-limited endpoint into an amplified
// denial of service. Instead a burst of hashes produces at most one scavenge
// per interval, which is the right shape for something that happens a handful
// of times a day.
var scavengeAfter = newScavenger(5 * time.Second)

type scavenger struct {
	interval time.Duration
	signal   chan struct{}
	start    sync.Once
}

func newScavenger(interval time.Duration) *scavenger {
	return &scavenger{interval: interval, signal: make(chan struct{}, 1)}
}

// request asks for a scavenge, never blocking. The goroutine is created on
// first use, so an installation that never authenticates never starts one.
func (s *scavenger) request() {
	s.start.Do(func() {
		go s.loop()
	})
	select {
	case s.signal <- struct{}{}:
	default:
		// One already pending; it will cover this hash too.
	}
}

func (s *scavenger) loop() {
	for range s.signal {
		// Wait out the burst so a sequence of hashes shares one scavenge.
		time.Sleep(s.interval)
		debug.FreeOSMemory()
	}
}

// Hash derives a hash for a plaintext password, in the standard PHC string
// format so the parameters travel with the hash. Storing them separately
// would make raising the cost later impossible without invalidating every
// existing password.
func (Hasher) Hash(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("reading salt: %w", err)
	}

	key := argon2.IDKey([]byte(plain), salt, iterations, memoryKiB, parallelism, keyLen)
	scavengeAfter.request()

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// stored is a hash decoded back into its parts.
type stored struct {
	salt    []byte
	key     []byte
	memory  uint32
	time    uint32
	threads uint8
}

// Verify reports whether plain matches encodedHash.
//
// The parameters are read back from the hash rather than assumed, so a hash
// written by an older build with a lower cost still verifies. That is what
// makes it possible to raise the cost later and re-hash on next login.
func (Hasher) Verify(encodedHash, plain string) (bool, error) {
	want, err := decode(encodedHash)
	if err != nil {
		return false, err
	}

	// keyLen is the package constant, not len(want.key): decode has already
	// established they are equal, and using the constant means there is no
	// width conversion here at all.
	got := argon2.IDKey([]byte(plain), want.salt, want.time, want.memory, want.threads, keyLen)
	scavengeAfter.request()

	// Constant time: a timing difference here leaks how much of the hash
	// matched, which is enough to reconstruct it byte by byte.
	return subtle.ConstantTimeCompare(got, want.key) == 1, nil
}

func decode(encodedHash string) (stored, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return stored{}, fmt.Errorf("%w: unexpected format", ErrInvalidHash)
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return stored{}, fmt.Errorf("%w: unreadable version", ErrInvalidHash)
	}
	if version != argon2.Version {
		return stored{}, fmt.Errorf("%w: unsupported argon2 version %d", ErrInvalidHash, version)
	}

	var decoded stored
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &decoded.memory, &decoded.time, &decoded.threads); err != nil {
		return stored{}, fmt.Errorf("%w: unreadable parameters", ErrInvalidHash)
	}
	if decoded.memory == 0 || decoded.time == 0 || decoded.threads == 0 {
		return stored{}, fmt.Errorf("%w: zero cost parameter", ErrInvalidHash)
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return stored{}, fmt.Errorf("%w: unreadable salt", ErrInvalidHash)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return stored{}, fmt.Errorf("%w: unreadable key", ErrInvalidHash)
	}
	if len(salt) == 0 {
		return stored{}, fmt.Errorf("%w: empty salt", ErrInvalidHash)
	}
	// The key length is fixed rather than read from the hash. Everything this
	// package produces uses keyLen, so a different width means the hash was
	// not written by us — and pinning it removes an attacker-controlled
	// length from the derivation on every login attempt.
	if len(key) != keyLen {
		return stored{}, fmt.Errorf("%w: key is %d bytes, want %d", ErrInvalidHash, len(key), keyLen)
	}

	decoded.salt, decoded.key = salt, key
	return decoded, nil
}
