package argon2id

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	hasher := New()
	const password = "una contraseña larga y buena"

	hash, err := hasher.Hash(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	t.Run("the hash reveals nothing", func(t *testing.T) {
		if strings.Contains(hash, password) {
			t.Error("the hash contains the plaintext")
		}
	})

	t.Run("the parameters travel with the hash", func(t *testing.T) {
		// Without this, raising the cost later would invalidate every
		// existing password.
		if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
			t.Errorf("hash = %q, want the PHC format with its parameters", hash)
		}
	})

	t.Run("the right password verifies", func(t *testing.T) {
		ok, err := hasher.Verify(hash, password)
		if err != nil {
			t.Fatalf("verifying: %v", err)
		}
		if !ok {
			t.Error("the correct password did not verify")
		}
	})

	t.Run("a wrong password does not", func(t *testing.T) {
		for _, wrong := range []string{"", "otra cosa", password + " ", strings.ToUpper(password)} {
			ok, err := hasher.Verify(hash, wrong)
			if err != nil {
				t.Fatalf("verifying %q: %v", wrong, err)
			}
			if ok {
				t.Errorf("%q verified against a different password's hash", wrong)
			}
		}
	})
}

func TestHashIsSaltedPerCall(t *testing.T) {
	hasher := New()
	const password = "la misma contraseña exacta"

	first, err := hasher.Hash(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	second, err := hasher.Hash(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	// Equal hashes for equal passwords would let anyone read the database and
	// see which accounts share a password.
	if first == second {
		t.Error("hashing the same password twice produced the same hash; the salt is not random")
	}

	// Both must still verify.
	for i, hash := range []string{first, second} {
		ok, err := hasher.Verify(hash, password)
		if err != nil {
			t.Fatalf("verifying hash %d: %v", i, err)
		}
		if !ok {
			t.Errorf("hash %d did not verify", i)
		}
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	hasher := New()
	cases := map[string]string{
		"empty":              "",
		"not a hash":         "hunter2",
		"wrong algorithm":    "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"bcrypt":             "$2a$10$abcdefghijklmnopqrstuv",
		"missing parameters": "$argon2id$v=19$c2FsdA$a2V5",
		"bad version":        "$argon2id$v=99$m=65536,t=3,p=2$c2FsdA$a2V5",
		"unreadable salt":    "$argon2id$v=19$m=65536,t=3,p=2$not!base64$a2V5",
		"empty key":          "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
		"short key":          "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"zero cost":          "$argon2id$v=19$m=0,t=3,p=2$c2FsdA$a2V5",
	}
	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := hasher.Verify(hash, "cualquier contraseña")
			if ok {
				t.Error("a malformed hash verified")
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Errorf("error = %v, want ErrInvalidHash", err)
			}
		})
	}
}
