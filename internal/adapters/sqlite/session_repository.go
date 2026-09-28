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

var _ ports.SessionRepository = (*SessionRepository)(nil)

// SessionRepository is the SQLite implementation of ports.SessionRepository.
type SessionRepository struct {
	db *DB
}

// NewSessionRepository wires the repository to an open database.
func NewSessionRepository(db *DB) *SessionRepository {
	return &SessionRepository{db: db}
}

// Create persists a session.
func (r *SessionRepository) Create(ctx context.Context, session domain.Session) error {
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO sessions (token_hash, admin_id, created_at, expires_at) VALUES (?, ?, ?, ?)",
		session.TokenHash, session.AdminID, formatTime(session.CreatedAt), formatTime(session.ExpiresAt))
	if err != nil {
		return fmt.Errorf("inserting session: %w", err)
	}
	return nil
}

// FindValid resolves a session token hash, rejecting expired ones.
//
// Expiry is filtered in SQL rather than checked after reading so an expired
// session is indistinguishable from a missing one to the caller, and there is
// no path where a stale row is loaded and then trusted by mistake.
func (r *SessionRepository) FindValid(ctx context.Context, tokenHash string, now time.Time) (domain.Session, error) {
	var (
		session   domain.Session
		createdAt string
		expiresAt string
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT token_hash, admin_id, created_at, expires_at
		FROM sessions
		WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, formatTime(now),
	).Scan(&session.TokenHash, &session.AdminID, &createdAt, &expiresAt)

	if errors.Is(err, sql.ErrNoRows) {
		return domain.Session{}, domain.ErrSessionNotFound
	}
	if err != nil {
		return domain.Session{}, fmt.Errorf("reading session: %w", err)
	}

	if session.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.Session{}, err
	}
	if session.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return domain.Session{}, err
	}
	return session, nil
}

// Delete signs a session out.
func (r *SessionRepository) Delete(ctx context.Context, tokenHash string) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// DeleteExpired sweeps sessions past their expiry.
func (r *SessionRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("deleting expired sessions: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reading delete result: %w", err)
	}
	return deleted, nil
}
