package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewAdmin(t *testing.T) {
	t.Run("trims the username and stores UTC", func(t *testing.T) {
		caracas := time.FixedZone("-04", -4*60*60)
		admin, err := NewAdmin("  antonio  ", "hashed", testNow.In(caracas))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if admin.Username != "antonio" {
			t.Errorf("Username = %q, want %q", admin.Username, "antonio")
		}
		if admin.CreatedAt.Location() != time.UTC {
			t.Errorf("CreatedAt location = %v, want UTC", admin.CreatedAt.Location())
		}
		if admin.ID != 0 {
			t.Errorf("ID = %d, want 0 until saved", admin.ID)
		}
	})

	t.Run("accepts a non-ASCII username", func(t *testing.T) {
		// Bounds are counted in runes, so an accented name is not held to a
		// stricter rule than the same name without accents.
		if _, err := NewAdmin(strings.Repeat("ñ", MaxUsernameLen), "hashed", testNow); err != nil {
			t.Errorf("unexpected error at the rune limit: %v", err)
		}
		if _, err := NewAdmin(strings.Repeat("ñ", MaxUsernameLen+1), "hashed", testNow); !errors.Is(err, ErrInvalidAdmin) {
			t.Error("a username one rune over the limit was accepted")
		}
	})

	t.Run("rejects invalid input", func(t *testing.T) {
		cases := map[string]struct{ username, hash string }{
			"empty username":      {"", "hashed"},
			"whitespace username": {"   ", "hashed"},
			"space inside":        {"antonio vila", "hashed"},
			"tab inside":          {"antonio\tvila", "hashed"},
			"newline inside":      {"antonio\nvila", "hashed"},
			"no hash":             {"antonio", ""},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				if _, err := NewAdmin(tc.username, tc.hash, testNow); !errors.Is(err, ErrInvalidAdmin) {
					t.Errorf("error = %v, want ErrInvalidAdmin", err)
				}
			})
		}
	})
}

func TestValidatePassword(t *testing.T) {
	t.Run("accepts a passphrase", func(t *testing.T) {
		if err := ValidatePassword("una contraseña larga y buena"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("measures length in runes", func(t *testing.T) {
		// A policy counting bytes would let this pass while rejecting the
		// same number of plain ASCII characters, which is arbitrary.
		short := strings.Repeat("ñ", MinPasswordLen-1)
		if err := ValidatePassword(short); !errors.Is(err, ErrWeakPassword) {
			t.Errorf("a %d-rune password was accepted", MinPasswordLen-1)
		}
		exact := strings.Repeat("ñ", MinPasswordLen)
		if err := ValidatePassword(exact); err != nil {
			t.Errorf("a password at the exact minimum was rejected: %v", err)
		}
	})

	t.Run("rejects out of range lengths", func(t *testing.T) {
		cases := map[string]string{
			"empty":     "",
			"too short": "corta",
			"too long":  strings.Repeat("a", MaxPasswordLen+1),
		}
		for name, password := range cases {
			t.Run(name, func(t *testing.T) {
				if err := ValidatePassword(password); !errors.Is(err, ErrWeakPassword) {
					t.Errorf("error = %v, want ErrWeakPassword", err)
				}
			})
		}
	})
}
