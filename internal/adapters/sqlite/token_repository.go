package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var _ ports.TokenRepository = (*TokenRepository)(nil)

// TokenRepository is the SQLite implementation of ports.TokenRepository.
type TokenRepository struct {
	db *DB
}

// NewTokenRepository wires the repository to an open database.
func NewTokenRepository(db *DB) *TokenRepository {
	return &TokenRepository{db: db}
}

// Create persists a token.
func (r *TokenRepository) Create(ctx context.Context, token domain.APIToken) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO api_tokens (token_hash, name, scopes, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		token.TokenHash, token.Name, domain.EncodeScopes(token.Scopes),
		formatTime(token.CreatedAt), nullableTime(token.ExpiresAt))
	if err != nil {
		return fmt.Errorf("inserting token: %w", err)
	}
	return nil
}

// FindValid resolves a token hash for authentication.
//
// Expiry is filtered in SQL, so an expired token is indistinguishable from an
// unknown one and there is no path that loads a stale row and then trusts it.
func (r *TokenRepository) FindValid(ctx context.Context, tokenHash string, now time.Time) (domain.APIToken, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT token_hash, name, scopes, created_at, expires_at, last_used
		FROM api_tokens
		WHERE token_hash = ? AND (expires_at IS NULL OR expires_at > ?)`,
		tokenHash, formatTime(now))

	token, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.APIToken{}, domain.ErrTokenNotFound
	}
	if err != nil {
		return domain.APIToken{}, fmt.Errorf("reading token: %w", err)
	}
	return token, nil
}

// List returns every token, newest first. It never exposes a usable
// credential: only the hash, so the panel can show what exists and let it be
// revoked, without being a place to harvest tokens from.
func (r *TokenRepository) List(ctx context.Context) ([]domain.APIToken, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT token_hash, name, scopes, created_at, expires_at, last_used
		FROM api_tokens
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tokens []domain.APIToken
	for rows.Next() {
		token, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning token: %w", err)
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating tokens: %w", err)
	}
	return tokens, nil
}

// Delete revokes a token. Idempotent.
func (r *TokenRepository) Delete(ctx context.Context, tokenHash string) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM api_tokens WHERE token_hash = ?", tokenHash); err != nil {
		return fmt.Errorf("deleting token: %w", err)
	}
	return nil
}

// TouchLastUsed records that a token authenticated a request, so an operator
// can tell a live integration from a forgotten credential worth revoking.
func (r *TokenRepository) TouchLastUsed(ctx context.Context, tokenHash string, now time.Time) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE api_tokens SET last_used = ? WHERE token_hash = ?", formatTime(now), tokenHash)
	if err != nil {
		return fmt.Errorf("updating last used: %w", err)
	}
	return nil
}

func scanToken(row scanner) (domain.APIToken, error) {
	var (
		token     domain.APIToken
		scopes    string
		createdAt string
		expiresAt sql.NullString
		lastUsed  sql.NullString
	)
	if err := row.Scan(&token.TokenHash, &token.Name, &scopes, &createdAt, &expiresAt, &lastUsed); err != nil {
		return domain.APIToken{}, err
	}

	token.Scopes = domain.DecodeScopes(scopes)

	parsed, err := parseTime(createdAt)
	if err != nil {
		return domain.APIToken{}, err
	}
	token.CreatedAt = parsed

	if token.ExpiresAt, err = parseNullableTime(expiresAt); err != nil {
		return domain.APIToken{}, err
	}
	if token.LastUsed, err = parseNullableTime(lastUsed); err != nil {
		return domain.APIToken{}, err
	}
	return token, nil
}
