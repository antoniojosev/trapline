package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func newTokens(t *testing.T) (*Tokens, *fakeTokenRepo, context.Context) {
	t.Helper()
	repo := newFakeTokenRepo()
	return NewTokens(repo, fixedClock{now: testNow}), repo, context.Background()
}

func TestCreateReturnsThePlaintextOnce(t *testing.T) {
	tokens, repo, ctx := newTokens(t)

	token, plaintext, err := tokens.Create(ctx, "cli", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plaintext == "" {
		t.Fatal("no plaintext returned")
	}

	// What is stored must not be usable as a credential. The list is a place
	// to see and revoke tokens, never to harvest them.
	for _, stored := range repo.tokens {
		if stored.TokenHash == plaintext {
			t.Error("the plaintext was stored verbatim")
		}
	}
	listed, err := tokens.List(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(listed) != 1 || listed[0].TokenHash != token.TokenHash {
		t.Errorf("list = %+v, want the token just created", listed)
	}
}

func TestAuthenticateChecksTheScope(t *testing.T) {
	tokens, _, ctx := newTokens(t)

	_, readOnly, err := tokens.Create(ctx, "dashboard", []domain.Scope{domain.ScopeProjectsRead}, nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	if _, err := tokens.Authenticate(ctx, readOnly, domain.ScopeProjectsRead); err != nil {
		t.Errorf("a read token was refused a read: %v", err)
	}

	// ErrForbidden, not ErrTokenNotFound: a holder with a valid credential and
	// the wrong scope should not be sent off to re-authenticate a token that
	// is perfectly fine.
	_, err = tokens.Authenticate(ctx, readOnly, domain.ScopeProjectsWrite)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("error = %v, want ErrForbidden", err)
	}
}

func TestAuthenticateRejectsUnknownAndEmpty(t *testing.T) {
	tokens, _, ctx := newTokens(t)

	for name, candidate := range map[string]string{
		"empty":   "",
		"unknown": "ek_no-existe",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tokens.Authenticate(ctx, candidate, domain.ScopeProjectsRead); !errors.Is(err, domain.ErrTokenNotFound) {
				t.Errorf("error = %v, want ErrTokenNotFound", err)
			}
		})
	}
}

func TestAuthenticateRecordsLastUsedWithoutFailingTheRequest(t *testing.T) {
	tokens, repo, ctx := newTokens(t)
	_, plaintext, err := tokens.Create(ctx, "cli", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	if _, err := tokens.Authenticate(ctx, plaintext, domain.ScopeProjectsRead); err != nil {
		t.Fatalf("authenticating: %v", err)
	}
	if repo.touched != 1 {
		t.Errorf("last-used recorded %d times, want 1", repo.touched)
	}

	// Recording when a token was last used is a convenience for an operator.
	// It must never be able to fail the request it is describing.
	repo.touchErr = errors.New("disk full")
	if _, err := tokens.Authenticate(ctx, plaintext, domain.ScopeProjectsRead); err != nil {
		t.Errorf("a failure recording last-used broke authentication: %v", err)
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	repo := newFakeTokenRepo()
	clock := &movableClock{now: testNow}
	tokens := NewTokens(repo, clock)
	ctx := context.Background()

	expiry := testNow.Add(time.Hour)
	_, plaintext, err := tokens.Create(ctx, "temporal", domain.AllScopes(), &expiry)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	if _, err := tokens.Authenticate(ctx, plaintext, domain.ScopeProjectsRead); err != nil {
		t.Fatalf("a live token was refused: %v", err)
	}

	clock.now = expiry
	if _, err := tokens.Authenticate(ctx, plaintext, domain.ScopeProjectsRead); !errors.Is(err, domain.ErrTokenNotFound) {
		t.Errorf("error = %v, want ErrTokenNotFound for an expired token", err)
	}
}

func TestRevoke(t *testing.T) {
	tokens, _, ctx := newTokens(t)
	token, plaintext, err := tokens.Create(ctx, "cli", domain.AllScopes(), nil)
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}

	if err := tokens.Revoke(ctx, plaintext); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if _, err := tokens.Authenticate(ctx, plaintext, domain.ScopeProjectsRead); !errors.Is(err, domain.ErrTokenNotFound) {
		t.Errorf("a revoked token still authenticates: %v", err)
	}
	// Revoking twice, and revoking by hash, must both be safe.
	if err := tokens.Revoke(ctx, plaintext); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
	if err := tokens.RevokeByHash(ctx, token.TokenHash); err != nil {
		t.Errorf("revoking by hash: %v", err)
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	tokens, repo, ctx := newTokens(t)

	if _, _, err := tokens.Create(ctx, "", domain.AllScopes(), nil); !errors.Is(err, domain.ErrInvalidToken) {
		t.Errorf("error = %v, want ErrInvalidToken", err)
	}
	// Validation happens before the store is touched.
	if len(repo.tokens) != 0 {
		t.Error("a token was persisted despite failing validation")
	}
}
