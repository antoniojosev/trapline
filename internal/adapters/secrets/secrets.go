// Package secrets encrypts the credentials this product has to keep.
//
// It exists for one row type: an alert channel's configuration holds a bot
// token, an incoming-webhook URL or an SMTP password, and those are
// credentials for somebody else's system. Storing them in a plain column
// would put them in every backup, every `.dump` and every screenshot of a
// database browser, and the blast radius of a leaked trapline file would stop
// being "the error reports" and become "the Slack workspace".
//
// The key lives beside the database rather than inside it, because a key
// stored in the thing it encrypts protects nothing. That has a consequence
// worth stating loudly and in more than one place: `backup` copies the
// database and not the key, so a restore without the key file leaves the
// channels present, listed, and mute (ADR 015).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// KeySize is the key length: AES-256.
const KeySize = 32

// KeyFileMode is the only mode a key file may have. Checked rather than
// assumed: a key that any user on the box can read is a key that is not one.
const KeyFileMode fs.FileMode = 0o600

// ErrKey means the key file is missing, malformed or readable by too many.
var ErrKey = errors.New("secret key")

// ErrDecrypt means a ciphertext did not open with this key.
//
// It is its own error because it has exactly one likely cause and the message
// has to say it: this is a database restored without its key file.
var ErrDecrypt = errors.New("could not decrypt with this key")

// Cipher seals and opens values with AES-256-GCM.
//
// It resolves its key lazily, on first use. That is what lets the key file be
// created by the act of configuring the first alert channel rather than by
// starting the server: an installation that never uses alerting never grows a
// `<db>.key` beside its database, and does not have to be told what it is for.
// It is the same claim ADR 005 makes about goroutines, applied to a file.
type Cipher struct {
	path string
	once sync.Once
	aead cipher.AEAD
	err  error
}

// At names the key file a cipher will use, without touching it.
func At(path string) *Cipher { return &Cipher{path: path} }

// Path is where this cipher's key lives.
func (c *Cipher) Path() string { return c.path }

// ensure resolves the key exactly once, creating it if this is the first
// secret the installation has ever stored.
func (c *Cipher) ensure() error {
	c.once.Do(func() {
		key, err := loadOrCreateKey(c.path)
		if err != nil {
			c.err = err
			return
		}
		c.aead, c.err = newAEAD(key)
	})
	return c.err
}

// Check reports whether the key exists, is protected and is usable, without
// creating one. It is what `doctor` calls: creating a key while diagnosing
// would turn "your key file is missing" into a silent, permanent loss of every
// configured channel.
func (c *Cipher) Check() error {
	_, err := readKey(c.path)
	return err
}

// KeyPathFor is where the key for a database lives: the database path with
// `.key` appended. Derived rather than configured by default, so an
// installation that never thinks about this still gets encryption, and an
// operator who moves the database finds the key next to it.
func KeyPathFor(dbPath string) string { return dbPath + ".key" }

// loadOrCreateKey reads the key at path, generating one if it is not there.
//
// Generating on first use rather than demanding configuration is deliberate:
// the alternative is an installation that cannot add a channel until somebody
// reads a page about key management, and the realistic outcome of that is a
// product whose alerting is switched off.
func loadOrCreateKey(path string) ([]byte, error) {
	key, err := readKey(path)
	switch {
	case err == nil:
		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}

	key = make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("%w: generating: %w", ErrKey, err)
	}
	if err := writeKey(path, key); err != nil {
		return nil, err
	}
	return key, nil
}

// CheckPermissions reports whether the key file is readable by anyone other
// than its owner.
func CheckPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrKey, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%w: %s is mode %#o, and must be %#o: "+
			"a key every user on this machine can read is not a key",
			ErrKey, path, mode, KeyFileMode.Perm())
	}
	return nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}
	return aead, nil
}

func readKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own configuration
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s does not exist: %w", ErrKey, path, err)
		}
		return nil, fmt.Errorf("%w: reading %s: %w", ErrKey, path, err)
	}
	if err := CheckPermissions(path); err != nil {
		return nil, err
	}

	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not %d hex characters: %w", ErrKey, path, KeySize*2, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: %s holds %d bytes, expected %d", ErrKey, path, len(key), KeySize)
	}
	return key, nil
}

// writeKey creates the file with its final permissions, never with looser ones
// it then tightens: between the create and the chmod, the key would be
// world-readable, and that window is all an attacker on the box needs.
func writeKey(path string, key []byte) error {
	if directory := filepath.Dir(path); directory != "" {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("%w: creating %s: %w", ErrKey, directory, err)
		}
	}
	//nolint:gosec // the path is the operator's own configuration, same as the database's
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, KeyFileMode)
	if err != nil {
		return fmt.Errorf("%w: creating %s: %w", ErrKey, path, err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return fmt.Errorf("%w: writing %s: %w", ErrKey, path, err)
	}
	// Synced before the caller is told the key exists. A channel encrypted
	// with a key that the crash a second later left unwritten is a channel
	// nobody can ever read again.
	if err := file.Sync(); err != nil {
		return fmt.Errorf("%w: syncing %s: %w", ErrKey, path, err)
	}
	return nil
}

// Seal encrypts plaintext, returning nonce || ciphertext.
//
// The nonce is random and prefixed rather than derived from a counter: a
// counter needs durable state, and a repeated nonce with GCM does not degrade
// gracefully, it hands over the authentication key.
func (c *Cipher) Seal(plaintext []byte) ([]byte, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating a nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts what Seal produced.
func (c *Cipher) Open(box []byte) ([]byte, error) {
	if err := c.ensure(); err != nil {
		return nil, err
	}
	nonceSize := c.aead.NonceSize()
	if len(box) < nonceSize {
		return nil, fmt.Errorf("%w: the stored value is %d bytes, shorter than a nonce", ErrDecrypt, len(box))
	}
	plaintext, err := c.aead.Open(nil, box[:nonceSize], box[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: this is what a database restored without its .key file looks like", ErrDecrypt)
	}
	return plaintext, nil
}
