package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// AdminRepository stores the panel's human accounts.
type AdminRepository interface {
	// Create persists a new admin, returning it with its assigned id.
	Create(ctx context.Context, admin domain.Admin) (domain.Admin, error)
	// FindByUsername returns domain.ErrAdminNotFound if there is no match.
	FindByUsername(ctx context.Context, username string) (domain.Admin, error)
	// FindByID returns domain.ErrAdminNotFound if there is no match.
	FindByID(ctx context.Context, id int64) (domain.Admin, error)
	// Count reports how many admins exist. Setup is only offered while this
	// is zero, which is what makes it a one-time flow rather than open
	// registration.
	Count(ctx context.Context) (int, error)
}

// SessionRepository stores panel sessions.
type SessionRepository interface {
	Create(ctx context.Context, session domain.Session) error
	// FindValid returns a session by its token hash if it exists and has not
	// expired, otherwise domain.ErrSessionNotFound.
	FindValid(ctx context.Context, tokenHash string, now time.Time) (domain.Session, error)
	Delete(ctx context.Context, tokenHash string) error
	// DeleteExpired removes sessions that are past their expiry, returning
	// how many went. Called opportunistically rather than on a timer: this
	// product runs no scheduler for a subsystem that is switched off, and
	// sessions are not worth a goroutine of their own (ADR 005).
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
}

// PasswordHasher hashes and verifies human passwords.
//
// Separate from anything that hashes tokens, on purpose: passwords are
// low-entropy and need a deliberately slow, memory-hard function, while
// tokens are high-entropy random values where slowness only taxes every
// request. One interface for both would be wrong in one direction.
type PasswordHasher interface {
	Hash(plain string) (string, error)
	Verify(encodedHash, plain string) (bool, error)
}
