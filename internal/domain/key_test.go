package domain

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestGenerateKey(t *testing.T) {
	t.Run("produces a hex key of the protocol's width", func(t *testing.T) {
		key, err := GenerateKey(42, testNow, bytes.NewReader(bytes.Repeat([]byte{0xAB}, PublicKeyLen)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(key.PublicKey) != PublicKeyLen {
			t.Errorf("len(PublicKey) = %d, want %d", len(key.PublicKey), PublicKeyLen)
		}
		if key.PublicKey != "abababababababababababababababab" {
			t.Errorf("PublicKey = %q, not the hex encoding of the given randomness", key.PublicKey)
		}
		if key.ProjectID != 42 {
			t.Errorf("ProjectID = %d, want 42", key.ProjectID)
		}
		if !key.Active() {
			t.Error("a freshly minted key must be active")
		}
	})

	t.Run("rejects a project id the store has not assigned", func(t *testing.T) {
		if _, err := GenerateKey(0, testNow, bytes.NewReader(make([]byte, 64))); !errors.Is(err, ErrInvalidProject) {
			t.Errorf("error = %v, want ErrInvalidProject", err)
		}
	})

	t.Run("fails loudly when randomness runs short", func(t *testing.T) {
		// A truncated read must never yield a short, guessable key.
		_, err := GenerateKey(1, testNow, bytes.NewReader([]byte{0x01, 0x02}))
		if err == nil {
			t.Fatal("expected an error, got none")
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("error = %v, want it to wrap io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("two keys differ", func(t *testing.T) {
		first, err := NewKey(testNow)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		second, err := NewKey(testNow)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if first.PublicKey == second.PublicKey {
			t.Error("two generated keys are identical")
		}
	})
}

func TestAssignTo(t *testing.T) {
	minted, err := NewKey(testNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("binds the project id", func(t *testing.T) {
		assigned, err := minted.AssignTo(7)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if assigned.ProjectID != 7 {
			t.Errorf("ProjectID = %d, want 7", assigned.ProjectID)
		}
		if assigned.PublicKey != minted.PublicKey {
			t.Error("assigning must not change the key material")
		}
		if minted.ProjectID != 0 {
			t.Error("AssignTo must not mutate the receiver")
		}
	})

	t.Run("rejects an id the store has not assigned", func(t *testing.T) {
		if _, err := minted.AssignTo(0); !errors.Is(err, ErrInvalidProject) {
			t.Errorf("error = %v, want ErrInvalidProject", err)
		}
	})

	t.Run("rejects an unminted key", func(t *testing.T) {
		if _, err := (Key{}).AssignTo(1); !errors.Is(err, ErrInvalidProject) {
			t.Errorf("error = %v, want ErrInvalidProject", err)
		}
	})
}

func TestKeyRevoke(t *testing.T) {
	key, err := NewKey(testNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	revoked := key.Revoke(testNow)
	if revoked.Active() {
		t.Error("a revoked key must not be active")
	}
	if key.Active() != true {
		t.Error("Revoke must not mutate the receiver")
	}

	later := testNow.Add(time.Hour)
	again := revoked.Revoke(later)
	if !again.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Errorf("re-revoking moved the timestamp to %v; it must stay at the first revocation", again.RevokedAt)
	}
}
