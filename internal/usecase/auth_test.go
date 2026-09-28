package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

const (
	testUser     = "antonio"
	testPassword = "una contraseña larga y buena"
)

func newAuth(t *testing.T) (*Auth, *fakeAdmins, *fakeSessions, *countingHasher, context.Context) {
	t.Helper()
	admins, sessions, hasher := newFakeAdmins(), newFakeSessions(), &countingHasher{}
	auth := NewAuth(admins, sessions, hasher, fixedClock{now: testNow})
	return auth, admins, sessions, hasher, context.Background()
}

func TestSetupIsOneTime(t *testing.T) {
	auth, _, _, _, ctx := newAuth(t)

	needed, err := auth.NeedsSetup(ctx)
	if err != nil {
		t.Fatalf("checking setup: %v", err)
	}
	if !needed {
		t.Fatal("a fresh installation should need setup")
	}

	admin, err := auth.Setup(ctx, testUser, testPassword)
	if err != nil {
		t.Fatalf("setting up: %v", err)
	}
	if admin.ID != 1 {
		t.Errorf("ID = %d, want 1", admin.ID)
	}
	if admin.PasswordHash == testPassword {
		t.Error("the password was stored in the clear")
	}

	needed, err = auth.NeedsSetup(ctx)
	if err != nil {
		t.Fatalf("checking setup: %v", err)
	}
	if needed {
		t.Error("setup is still on offer after an admin exists")
	}

	// There is no open registration: the second call must be refused, or the
	// setup endpoint is an account-creation endpoint for anyone who finds it.
	if _, err := auth.Setup(ctx, "intruso", testPassword); !errors.Is(err, domain.ErrSetupComplete) {
		t.Errorf("error = %v, want ErrSetupComplete", err)
	}
}

func TestSetupEnforcesThePasswordPolicy(t *testing.T) {
	auth, admins, _, _, ctx := newAuth(t)

	if _, err := auth.Setup(ctx, testUser, "corta"); !errors.Is(err, domain.ErrWeakPassword) {
		t.Errorf("error = %v, want ErrWeakPassword", err)
	}
	if len(admins.admins) != 0 {
		t.Error("an admin was created despite the weak password")
	}
}

func TestLoginOpensASession(t *testing.T) {
	auth, _, sessions, _, ctx := newAuth(t)
	if _, err := auth.Setup(ctx, testUser, testPassword); err != nil {
		t.Fatalf("setting up: %v", err)
	}

	token, err := auth.Login(ctx, testUser, testPassword)
	if err != nil {
		t.Fatalf("logging in: %v", err)
	}
	if token == "" {
		t.Fatal("login returned an empty token")
	}

	// What is stored must be a hash, never the token itself: a database dump
	// must not be a set of working cookies.
	if _, storedVerbatim := sessions.sessions[token]; storedVerbatim {
		t.Error("the session is stored under the raw token")
	}
	if _, storedHashed := sessions.sessions[domain.HashSessionToken(token)]; !storedHashed {
		t.Error("no session stored under the token hash")
	}

	admin, err := auth.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticating: %v", err)
	}
	if admin.Username != testUser {
		t.Errorf("Username = %q, want %q", admin.Username, testUser)
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	auth, _, _, hasher, ctx := newAuth(t)
	if _, err := auth.Setup(ctx, testUser, testPassword); err != nil {
		t.Fatalf("setting up: %v", err)
	}

	before := hasher.verifyCount()

	wrongPassword := func() error {
		_, err := auth.Login(ctx, testUser, "la contraseña equivocada")
		return err
	}
	unknownUser := func() error {
		_, err := auth.Login(ctx, "nadie", testPassword)
		return err
	}

	for name, attempt := range map[string]func() error{"wrong password": wrongPassword, "unknown user": unknownUser} {
		t.Run(name, func(t *testing.T) {
			if err := attempt(); !errors.Is(err, domain.ErrInvalidCredentials) {
				t.Errorf("error = %v, want ErrInvalidCredentials", err)
			}
		})
	}

	// Both paths must verify a hash. If the unknown-username path skipped it,
	// the faster response would tell an attacker which usernames exist.
	if got := hasher.verifyCount() - before; got != 2 {
		t.Errorf("%d verifications for two failed logins, want 2 — the timing defence is not working", got)
	}
}

func TestAuthenticateRejectsBadTokens(t *testing.T) {
	auth, _, _, _, ctx := newAuth(t)
	if _, err := auth.Setup(ctx, testUser, testPassword); err != nil {
		t.Fatalf("setting up: %v", err)
	}
	token, err := auth.Login(ctx, testUser, testPassword)
	if err != nil {
		t.Fatalf("logging in: %v", err)
	}

	for name, candidate := range map[string]string{
		"empty":     "",
		"garbage":   "no-es-un-token",
		"truncated": token[:len(token)-1],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := auth.Authenticate(ctx, candidate); !errors.Is(err, domain.ErrSessionNotFound) {
				t.Errorf("error = %v, want ErrSessionNotFound", err)
			}
		})
	}
}

func TestLogoutInvalidatesTheSession(t *testing.T) {
	auth, _, _, _, ctx := newAuth(t)
	if _, err := auth.Setup(ctx, testUser, testPassword); err != nil {
		t.Fatalf("setting up: %v", err)
	}
	token, err := auth.Login(ctx, testUser, testPassword)
	if err != nil {
		t.Fatalf("logging in: %v", err)
	}

	if err := auth.Logout(ctx, token); err != nil {
		t.Fatalf("logging out: %v", err)
	}
	// Server-side sessions exist precisely so this is true.
	if _, err := auth.Authenticate(ctx, token); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Errorf("the token still works after logout: %v", err)
	}

	// Logging out twice, or with an empty cookie, must not be an error.
	if err := auth.Logout(ctx, token); err != nil {
		t.Errorf("logging out twice: %v", err)
	}
	if err := auth.Logout(ctx, ""); err != nil {
		t.Errorf("logging out with no token: %v", err)
	}
}

func TestExpiredSessionIsRejectedAndSwept(t *testing.T) {
	admins, sessions, hasher := newFakeAdmins(), newFakeSessions(), &countingHasher{}
	clock := &movableClock{now: testNow}
	auth := NewAuth(admins, sessions, hasher, clock)
	ctx := context.Background()

	if _, err := auth.Setup(ctx, testUser, testPassword); err != nil {
		t.Fatalf("setting up: %v", err)
	}
	token, err := auth.Login(ctx, testUser, testPassword)
	if err != nil {
		t.Fatalf("logging in: %v", err)
	}

	clock.now = testNow.Add(domain.SessionLifetime)
	if _, err := auth.Authenticate(ctx, token); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Errorf("an expired session authenticated: %v", err)
	}

	// The next login sweeps it: no scheduler needed for something that can
	// ride along on work that already happens (ADR 005).
	if _, err := auth.Login(ctx, testUser, testPassword); err != nil {
		t.Fatalf("logging in again: %v", err)
	}
	if _, stillThere := sessions.sessions[domain.HashSessionToken(token)]; stillThere {
		t.Error("the expired session was not swept")
	}
}
