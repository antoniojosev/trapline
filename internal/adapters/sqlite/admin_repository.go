package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Compile-time proof that this adapter satisfies the port. Without it, a
// signature drifting apart is only discovered wherever the two are first
// wired together, which may be a long way from either.
var _ ports.AdminRepository = (*AdminRepository)(nil)

// AdminRepository is the SQLite implementation of ports.AdminRepository.
type AdminRepository struct {
	db *DB
}

// NewAdminRepository wires the repository to an open database.
func NewAdminRepository(db *DB) *AdminRepository {
	return &AdminRepository{db: db}
}

// Create persists a new admin.
func (r *AdminRepository) Create(ctx context.Context, admin domain.Admin) (domain.Admin, error) {
	result, err := r.db.ExecContext(ctx,
		"INSERT INTO admins (username, password_hash, created_at) VALUES (?, ?, ?)",
		admin.Username, admin.PasswordHash, formatTime(admin.CreatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return domain.Admin{}, fmt.Errorf("%w: username %q is taken", domain.ErrInvalidAdmin, admin.Username)
		}
		return domain.Admin{}, fmt.Errorf("inserting admin: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Admin{}, fmt.Errorf("reading assigned admin id: %w", err)
	}
	admin.ID = id
	return admin, nil
}

// FindByUsername looks an admin up for login.
func (r *AdminRepository) FindByUsername(ctx context.Context, username string) (domain.Admin, error) {
	return r.findAdmin(ctx,
		"SELECT id, username, password_hash, created_at FROM admins WHERE username = ?", username)
}

// FindByID looks an admin up from a session.
func (r *AdminRepository) FindByID(ctx context.Context, id int64) (domain.Admin, error) {
	return r.findAdmin(ctx,
		"SELECT id, username, password_hash, created_at FROM admins WHERE id = ?", id)
}

func (r *AdminRepository) findAdmin(ctx context.Context, query string, arg any) (domain.Admin, error) {
	var (
		admin     domain.Admin
		createdAt string
	)
	err := r.db.QueryRowContext(ctx, query, arg).
		Scan(&admin.ID, &admin.Username, &admin.PasswordHash, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Admin{}, domain.ErrAdminNotFound
	}
	if err != nil {
		return domain.Admin{}, fmt.Errorf("reading admin: %w", err)
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return domain.Admin{}, err
	}
	admin.CreatedAt = parsed
	return admin, nil
}

// Count reports how many admins exist.
func (r *AdminRepository) Count(ctx context.Context) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admins").Scan(&count); err != nil {
		return 0, fmt.Errorf("counting admins: %w", err)
	}
	return count, nil
}

// isUniqueViolation reports whether err is a UNIQUE constraint failure.
//
// Matched on the message because the pure-Go driver's error type is not part
// of its API surface, and importing its internals to read a code would couple
// this adapter to a private detail. The message is stable and the fallback is
// benign: a missed match becomes a generic insert error rather than a wrong
// one.
func isUniqueViolation(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}
