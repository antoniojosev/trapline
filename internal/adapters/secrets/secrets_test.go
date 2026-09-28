package secrets_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/adapters/secrets"
)

func TestKeyPathIsBesideTheDatabase(t *testing.T) {
	if got := secrets.KeyPathFor("/var/lib/trapline/trapline.db"); got != "/var/lib/trapline/trapline.db.key" {
		t.Errorf("key path is %q", got)
	}
}

// TestNamingAKeyDoesNotCreateIt is ADR 005 applied to a file: an installation
// that never configures a channel should not grow a key file it has to be told
// about, and `doctor` must never mint one as a side effect of diagnosing.
func TestNamingAKeyDoesNotCreateIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trapline.db.key")
	cipher := secrets.At(path)

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("naming a key created %s", path)
	}
	if err := cipher.Check(); err == nil {
		t.Error("Check reported a key that does not exist as fine")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Check created the key file; a diagnosis must never mint a key")
	}
}

func TestFirstUseCreatesAProtectedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trapline.db.key")
	cipher := secrets.At(path)

	sealed, err := cipher.Seal([]byte("hunter2"))
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the key was not created: %v", err)
	}
	if mode := info.Mode().Perm(); mode != secrets.KeyFileMode.Perm() {
		t.Errorf("the key is mode %#o, want %#o: a key every user on the box can read is not a key",
			mode, secrets.KeyFileMode.Perm())
	}
	if err := cipher.Check(); err != nil {
		t.Errorf("the key this code just wrote does not pass its own check: %v", err)
	}

	opened, err := cipher.Open(sealed)
	if err != nil {
		t.Fatalf("opening what we sealed: %v", err)
	}
	if string(opened) != "hunter2" {
		t.Errorf("the plaintext came back as %q", opened)
	}
}

// TestCiphertextIsNotThePlaintext is the whole point: what lands in the
// database, in every backup and in every `.dump`, must not be the credential.
func TestCiphertextIsNotThePlaintext(t *testing.T) {
	cipher := secrets.At(filepath.Join(t.TempDir(), "k"))
	sealed, err := cipher.Seal([]byte("xoxb-a-real-slack-token"))
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	if strings.Contains(string(sealed), "xoxb") {
		t.Error("the ciphertext contains the plaintext")
	}
}

// TestSealingTwiceProducesDifferentBytes: GCM with a repeated nonce does not
// degrade gracefully, it hands over the authentication key.
func TestSealingTwiceProducesDifferentBytes(t *testing.T) {
	cipher := secrets.At(filepath.Join(t.TempDir(), "k"))
	first, err := cipher.Seal([]byte("same"))
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	second, err := cipher.Seal([]byte("same"))
	if err != nil {
		t.Fatalf("sealing again: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Error("two seals of the same plaintext are identical, so the nonce is not random")
	}
}

// TestAnotherKeyCannotOpenIt is the restore-without-the-key case, which is the
// one that actually happens.
func TestAnotherKeyCannotOpenIt(t *testing.T) {
	original := secrets.At(filepath.Join(t.TempDir(), "one.key"))
	sealed, err := original.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}

	replacement := secrets.At(filepath.Join(t.TempDir(), "two.key"))
	_, err = replacement.Open(sealed)
	if err == nil {
		t.Fatal("a different key opened the ciphertext")
	}
	if !errors.Is(err, secrets.ErrDecrypt) {
		t.Errorf("error %v does not wrap ErrDecrypt", err)
	}
	if !strings.Contains(err.Error(), ".key") {
		t.Errorf("error %q does not point at the missing key file, which is the likely cause", err)
	}
}

func TestOpenRejectsSomethingTooShortToBeACiphertext(t *testing.T) {
	cipher := secrets.At(filepath.Join(t.TempDir(), "k"))
	if _, err := cipher.Open([]byte{1, 2, 3}); err == nil {
		t.Fatal("three bytes opened as a ciphertext")
	}
}

func TestALooseKeyIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loose.key")
	if err := os.WriteFile(path, []byte(strings.Repeat("ab", secrets.KeySize)+"\n"), 0o644); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	cipher := secrets.At(path)
	err := cipher.Check()
	if err == nil {
		t.Fatal("a world-readable key was accepted")
	}
	if !errors.Is(err, secrets.ErrKey) {
		t.Errorf("error %v does not wrap ErrKey", err)
	}
	if _, err := cipher.Seal([]byte("x")); err == nil {
		t.Error("a world-readable key was still used to encrypt")
	}
}

func TestAMalformedKeyIsRefused(t *testing.T) {
	directory := t.TempDir()

	cases := map[string]string{
		"not hex":   "zzzz\n",
		"too short": "abcd\n",
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, strings.ReplaceAll(name, " ", "-")+".key")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("writing the fixture: %v", err)
			}
			if err := secrets.At(path).Check(); err == nil {
				t.Fatalf("a key file holding %q was accepted", contents)
			}
		})
	}
}

// TestTheKeyIsStableAcrossProcesses: a second cipher over the same path has to
// open what the first one sealed, or every restart would lose every channel.
func TestTheKeyIsStableAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trapline.db.key")

	sealed, err := secrets.At(path).Seal([]byte("survives"))
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	opened, err := secrets.At(path).Open(sealed)
	if err != nil {
		t.Fatalf("opening with a fresh cipher over the same key: %v", err)
	}
	if string(opened) != "survives" {
		t.Errorf("the plaintext came back as %q", opened)
	}
}

func TestPathIsReported(t *testing.T) {
	if got := secrets.At("/tmp/x.key").Path(); got != "/tmp/x.key" {
		t.Errorf("Path is %q", got)
	}
}
