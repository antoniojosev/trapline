package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Tokens manages API tokens: the credential the CLI, an agent over MCP, or
// any integration uses.
type Tokens struct {
	repo  ports.TokenRepository
	clock ports.Clock
}

// NewTokens wires the use case.
func NewTokens(repo ports.TokenRepository, clock ports.Clock) *Tokens {
	return &Tokens{repo: repo, clock: clock}
}

// Create mints a token and returns the plaintext, which is shown once and
// never recoverable. Storing it retrievably would make the token list a place
// to harvest credentials from, which is the opposite of what it is for.
func (t *Tokens) Create(
	ctx context.Context, name string, scopes []domain.Scope, expiresAt *time.Time,
) (token domain.APIToken, plaintext string, err error) {
	token, plaintext, err = domain.MintAPIToken(name, scopes, t.clock.Now(), expiresAt)
	if err != nil {
		return domain.APIToken{}, "", err
	}
	if err := t.repo.Create(ctx, token); err != nil {
		return domain.APIToken{}, "", err
	}
	return token, plaintext, nil
}

// Authenticate resolves a plaintext token and checks it carries a scope.
//
// The scope check happens here rather than in each handler so a new endpoint
// cannot forget it. Returning domain.ErrForbidden distinctly from
// ErrTokenNotFound is intentional: a caller with a valid credential and the
// wrong scope deserves to be told which of the two is wrong, and revealing it
// leaks nothing they do not already hold.
func (t *Tokens) Authenticate(ctx context.Context, plaintext string, required domain.Scope) (domain.APIToken, error) {
	if plaintext == "" {
		return domain.APIToken{}, domain.ErrTokenNotFound
	}
	hash := domain.HashAPIToken(plaintext)
	now := t.clock.Now()

	token, err := t.repo.FindValid(ctx, hash, now)
	if err != nil {
		return domain.APIToken{}, err
	}
	if !token.Allows(required) {
		return domain.APIToken{}, fmt.Errorf("%w: token %q lacks scope %q", domain.ErrForbidden, token.Name, required)
	}

	// Best effort: an operator wants to tell a live integration from a
	// forgotten credential, but failing to record that must never fail the
	// request it describes.
	_ = t.repo.TouchLastUsed(ctx, hash, now)

	return token, nil
}

// List returns every token. The plaintext is not among the fields, by
// construction.
func (t *Tokens) List(ctx context.Context) ([]domain.APIToken, error) {
	return t.repo.List(ctx)
}

// Revoke deletes a token by its plaintext, for the holder revoking their own.
func (t *Tokens) Revoke(ctx context.Context, plaintext string) error {
	return t.repo.Delete(ctx, domain.HashAPIToken(plaintext))
}

// RevokeByHash deletes a token by its stored hash, which is what the panel and
// the CLI list shows — the only handle available for a token whose plaintext
// is, correctly, gone.
func (t *Tokens) RevokeByHash(ctx context.Context, tokenHash string) error {
	return t.repo.Delete(ctx, tokenHash)
}
