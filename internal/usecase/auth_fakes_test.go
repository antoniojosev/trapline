package usecase

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// fakeAdmins is an in-memory ports.AdminRepository.
type fakeAdmins struct {
	admins []domain.Admin
	nextID int64
	failOn map[string]error
}

func newFakeAdmins() *fakeAdmins {
	return &fakeAdmins{nextID: 1, failOn: map[string]error{}}
}

func (r *fakeAdmins) Create(_ context.Context, admin domain.Admin) (domain.Admin, error) {
	if err := r.failOn["Create"]; err != nil {
		return domain.Admin{}, err
	}
	for _, existing := range r.admins {
		if existing.Username == admin.Username {
			return domain.Admin{}, fmt.Errorf("%w: username taken", domain.ErrInvalidAdmin)
		}
	}
	admin.ID = r.nextID
	r.nextID++
	r.admins = append(r.admins, admin)
	return admin, nil
}

func (r *fakeAdmins) FindByUsername(_ context.Context, username string) (domain.Admin, error) {
	if err := r.failOn["FindByUsername"]; err != nil {
		return domain.Admin{}, err
	}
	for _, admin := range r.admins {
		if admin.Username == username {
			return admin, nil
		}
	}
	return domain.Admin{}, domain.ErrAdminNotFound
}

func (r *fakeAdmins) FindByID(_ context.Context, id int64) (domain.Admin, error) {
	if err := r.failOn["FindByID"]; err != nil {
		return domain.Admin{}, err
	}
	for _, admin := range r.admins {
		if admin.ID == id {
			return admin, nil
		}
	}
	return domain.Admin{}, domain.ErrAdminNotFound
}

func (r *fakeAdmins) Count(_ context.Context) (int, error) {
	if err := r.failOn["Count"]; err != nil {
		return 0, err
	}
	return len(r.admins), nil
}

// fakeSessions is an in-memory ports.SessionRepository.
type fakeSessions struct {
	sessions map[string]domain.Session
	swept    int
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{sessions: map[string]domain.Session{}}
}

func (r *fakeSessions) Create(_ context.Context, session domain.Session) error {
	r.sessions[session.TokenHash] = session
	return nil
}

func (r *fakeSessions) FindValid(_ context.Context, tokenHash string, now time.Time) (domain.Session, error) {
	session, found := r.sessions[tokenHash]
	if !found || session.Expired(now) {
		return domain.Session{}, domain.ErrSessionNotFound
	}
	return session, nil
}

func (r *fakeSessions) Delete(_ context.Context, tokenHash string) error {
	delete(r.sessions, tokenHash)
	return nil
}

func (r *fakeSessions) DeleteExpired(_ context.Context, now time.Time) (int64, error) {
	var deleted int64
	for hash, session := range r.sessions {
		if session.Expired(now) {
			delete(r.sessions, hash)
			deleted++
		}
	}
	r.swept++
	return deleted, nil
}

// countingHasher is a fast stand-in for Argon2id that records how much work
// it was asked to do, so a test can assert that an unknown username costs the
// same verification as a wrong password.
type countingHasher struct {
	mu       sync.Mutex
	verifies []string
}

func (h *countingHasher) Hash(plain string) (string, error) {
	return "hashed:" + plain, nil
}

func (h *countingHasher) Verify(encodedHash, plain string) (bool, error) {
	h.mu.Lock()
	h.verifies = append(h.verifies, encodedHash)
	h.mu.Unlock()

	if !strings.HasPrefix(encodedHash, "hashed:") {
		return false, fmt.Errorf("%w: not one of ours", domain.ErrInvalidAdmin)
	}
	return strings.TrimPrefix(encodedHash, "hashed:") == plain, nil
}

func (h *countingHasher) verifyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.verifies)
}
