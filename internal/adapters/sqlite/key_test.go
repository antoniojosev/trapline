package sqlite

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

func mintKey(t *testing.T) domain.Key {
	t.Helper()
	key, err := domain.NewKey(testNow)
	if err != nil {
		t.Fatalf("minting key: %v", err)
	}
	return key
}

func TestRotationAllowsTwoActiveKeys(t *testing.T) {
	repo, ctx := newRepo(t)
	project, first := createProject(t, repo, "rotating")

	// The whole point of two active keys: the new one works before the old
	// one is retired, so deployments roll over without dropping events.
	second, err := repo.AddKey(ctx, project.ID, mintKey(t))
	if err != nil {
		t.Fatalf("adding a second key: %v", err)
	}

	for _, key := range []domain.Key{first, second} {
		if _, err := repo.FindActiveKey(ctx, key.PublicKey); err != nil {
			t.Errorf("key %s should authenticate during rotation: %v", key.PublicKey, err)
		}
	}

	active, err := repo.ActiveKeys(ctx, project.ID)
	if err != nil {
		t.Fatalf("listing active keys: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("got %d active keys, want 2", len(active))
	}
}

func TestRotationRefusesAThirdKey(t *testing.T) {
	repo, ctx := newRepo(t)
	project, first := createProject(t, repo, "rotating")

	if _, err := repo.AddKey(ctx, project.ID, mintKey(t)); err != nil {
		t.Fatalf("adding a second key: %v", err)
	}
	if _, err := repo.AddKey(ctx, project.ID, mintKey(t)); !errors.Is(err, domain.ErrTooManyActiveKeys) {
		t.Errorf("error = %v, want ErrTooManyActiveKeys", err)
	}

	// Retiring the old key makes room again: that is rotation completing.
	if err := repo.RevokeKey(ctx, first.PublicKey); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if _, err := repo.AddKey(ctx, project.ID, mintKey(t)); err != nil {
		t.Errorf("adding a key after revoking one: %v", err)
	}
}

func TestAddKeyToMissingProject(t *testing.T) {
	repo, ctx := newRepo(t)

	if _, err := repo.AddKey(ctx, 404, mintKey(t)); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("error = %v, want ErrProjectNotFound", err)
	}
}

func TestRevokedKeyStopsAuthenticating(t *testing.T) {
	repo, ctx := newRepo(t)
	project, key := createProject(t, repo, "revoked")

	if err := repo.RevokeKey(ctx, key.PublicKey); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if _, err := repo.FindActiveKey(ctx, key.PublicKey); !errors.Is(err, domain.ErrKeyNotFound) {
		t.Errorf("error = %v, want ErrKeyNotFound", err)
	}

	active, err := repo.ActiveKeys(ctx, project.ID)
	if err != nil {
		t.Fatalf("listing active keys: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("got %d active keys after revoking the only one", len(active))
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	repo, ctx := newRepo(t)
	_, key := createProject(t, repo, "idempotent")

	for attempt := range 3 {
		if err := repo.RevokeKey(ctx, key.PublicKey); err != nil {
			t.Fatalf("revoke attempt %d: %v", attempt+1, err)
		}
	}
	// Revoking something that never existed is also not an error: a retried
	// command must be safe, and a CLI cannot know whether its first attempt
	// landed before the connection dropped.
	if err := repo.RevokeKey(ctx, "ffffffffffffffffffffffffffffffff"); err != nil {
		t.Errorf("revoking an unknown key: %v", err)
	}
}

func TestFindActiveKeyDoesNotLeakTheKey(t *testing.T) {
	repo, ctx := newRepo(t)

	// This error can reach an unauthenticated caller on the public ingest
	// endpoint, so it must not echo back what was tried.
	const attempted = "deadbeefdeadbeefdeadbeefdeadbeef"
	_, err := repo.FindActiveKey(ctx, attempted)
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if got := err.Error(); strings.Contains(got, attempted) {
		t.Errorf("error message %q echoes the attempted key", got)
	}
}

func TestConcurrentRotationCannotExceedTheLimit(t *testing.T) {
	// The count-then-insert must happen in one transaction. If it did not,
	// two concurrent rotations would each see room and both write.
	repo, ctx := newRepo(t)
	project, _ := createProject(t, repo, "racing")

	const attempts = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := domain.NewKey(testNow)
			if err != nil {
				return
			}
			if _, err := repo.AddKey(ctx, project.ID, key); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted != 1 {
		t.Errorf("%d of %d concurrent rotations were accepted, want exactly 1 (one slot was free)", accepted, attempts)
	}
	active, err := repo.ActiveKeys(ctx, project.ID)
	if err != nil {
		t.Fatalf("listing active keys: %v", err)
	}
	if len(active) > domain.MaxActiveKeys {
		t.Errorf("%d active keys, over the limit of %d", len(active), domain.MaxActiveKeys)
	}
}
