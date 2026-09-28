package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Auth is authentication for the panel: the one-time setup, login, logout and
// resolving a session back to an admin.
type Auth struct {
	admins   ports.AdminRepository
	sessions ports.SessionRepository
	hasher   ports.PasswordHasher
	clock    ports.Clock

	// decoyHash is a well-formed hash of a value nobody knows, used to spend
	// the same work on an unknown username as on a wrong password.
	//
	// It is computed by the injected hasher rather than hard-coded, and
	// memoised so the cost is paid once. A hard-coded literal would be a
	// silent liability: if it ever failed to decode, verification would
	// return early with an error and the timing defence it exists for would
	// be gone, with nothing failing to say so.
	decoyHash func() (string, error)
}

// NewAuth wires the use case.
func NewAuth(
	admins ports.AdminRepository,
	sessions ports.SessionRepository,
	hasher ports.PasswordHasher,
	clock ports.Clock,
) *Auth {
	return &Auth{
		admins:   admins,
		sessions: sessions,
		hasher:   hasher,
		clock:    clock,
		decoyHash: sync.OnceValues(func() (string, error) {
			return hasher.Hash("a value that is never a real password")
		}),
	}
}

// NeedsSetup reports whether the installation has no admin yet.
func (a *Auth) NeedsSetup(ctx context.Context) (bool, error) {
	count, err := a.admins.Count(ctx)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// Setup creates the first admin. It is available only while there is none:
// there is no open registration, so this closes itself after one use.
func (a *Auth) Setup(ctx context.Context, username, password string) (domain.Admin, error) {
	needed, err := a.NeedsSetup(ctx)
	if err != nil {
		return domain.Admin{}, err
	}
	if !needed {
		return domain.Admin{}, domain.ErrSetupComplete
	}

	if err := domain.ValidatePassword(password); err != nil {
		return domain.Admin{}, err
	}
	hash, err := a.hasher.Hash(password)
	if err != nil {
		return domain.Admin{}, fmt.Errorf("hashing password: %w", err)
	}
	admin, err := domain.NewAdmin(username, hash, a.clock.Now())
	if err != nil {
		return domain.Admin{}, err
	}
	return a.admins.Create(ctx, admin)
}

// Login verifies credentials and opens a session, returning the token to hand
// to the client.
//
// Every failure path returns domain.ErrInvalidCredentials, including "no such
// user". Distinguishing them turns the login form into an oracle for valid
// usernames.
func (a *Auth) Login(ctx context.Context, username, password string) (token string, err error) {
	admin, err := a.admins.FindByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, domain.ErrAdminNotFound) {
			// Verify anyway. Returning early on an unknown username makes
			// that case measurably faster than a wrong password, which leaks
			// which usernames exist through response timing alone.
			a.spendVerificationWork(password)
			return "", domain.ErrInvalidCredentials
		}
		return "", err
	}

	ok, err := a.hasher.Verify(admin.PasswordHash, password)
	if err != nil {
		return "", fmt.Errorf("verifying password: %w", err)
	}
	if !ok {
		return "", domain.ErrInvalidCredentials
	}

	now := a.clock.Now()
	session, token, err := domain.NewSessionToken(admin.ID, now)
	if err != nil {
		return "", fmt.Errorf("opening session: %w", err)
	}
	if err := a.sessions.Create(ctx, session); err != nil {
		return "", err
	}

	// Opportunistic sweep: sessions do not warrant a scheduler of their own,
	// and a login is exactly the moment when a little extra work is free
	// (ADR 005).
	_, _ = a.sessions.DeleteExpired(ctx, now)

	return token, nil
}

// Authenticate resolves a session token to its admin.
func (a *Auth) Authenticate(ctx context.Context, token string) (domain.Admin, error) {
	if token == "" {
		return domain.Admin{}, domain.ErrSessionNotFound
	}
	session, err := a.sessions.FindValid(ctx, domain.HashSessionToken(token), a.clock.Now())
	if err != nil {
		return domain.Admin{}, err
	}
	return a.admins.FindByID(ctx, session.AdminID)
}

// Logout ends a session. Unknown tokens are not an error: signing out twice,
// or with an already expired cookie, must succeed.
func (a *Auth) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return a.sessions.Delete(ctx, domain.HashSessionToken(token))
}

// spendVerificationWork performs a password verification whose result is
// discarded, so an unknown username costs the same as a wrong password.
func (a *Auth) spendVerificationWork(password string) {
	decoy, err := a.decoyHash()
	if err != nil {
		return
	}
	_, _ = a.hasher.Verify(decoy, password)
}
