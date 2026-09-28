package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

var (
	testNow    = time.Date(2026, 8, 22, 15, 4, 5, 0, time.UTC)
	testOrigin = domain.Origin{Scheme: "https", Host: "errors.example.com"}
)

func newProjects(t *testing.T) (*Projects, *fakeRepo, context.Context) {
	t.Helper()
	repo := newFakeRepo()
	return NewProjects(repo, repo, fixedClock{now: testNow}, testOrigin), repo, context.Background()
}

func TestCreateReturnsAUsableDSN(t *testing.T) {
	projects, _, ctx := newProjects(t)

	view, err := projects.Create(ctx, "venekambio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if view.Project.ID != 1 {
		t.Errorf("ID = %d, want 1", view.Project.ID)
	}
	if !view.Project.CreatedAt.Equal(testNow) {
		t.Errorf("CreatedAt = %v, want the injected clock's %v", view.Project.CreatedAt, testNow)
	}
	if len(view.Keys) != 1 {
		t.Fatalf("got %d keys, want exactly 1 on creation", len(view.Keys))
	}

	dsn, ok := view.PrimaryDSN()
	if !ok {
		t.Fatal("a freshly created project has no primary DSN")
	}
	if dsn.ProjectID != view.Project.ID {
		t.Errorf("DSN points at project %d, want %d", dsn.ProjectID, view.Project.ID)
	}
	if dsn.Host != testOrigin.Host {
		t.Errorf("DSN host = %q, want the configured origin %q", dsn.Host, testOrigin.Host)
	}
	// The DSN is what the user pastes into an SDK; it has to parse.
	if _, err := domain.ParseDSN(dsn.String()); err != nil {
		t.Errorf("the DSN handed to the user does not parse: %v", err)
	}
}

func TestCreateRejectsAnInvalidName(t *testing.T) {
	projects, repo, ctx := newProjects(t)

	if _, err := projects.Create(ctx, "   "); !errors.Is(err, domain.ErrInvalidProject) {
		t.Errorf("error = %v, want ErrInvalidProject", err)
	}
	// Validation happens before the store is touched.
	if len(repo.projects) != 0 {
		t.Errorf("%d projects were persisted despite validation failing", len(repo.projects))
	}
}

func TestCreatePropagatesStoreFailure(t *testing.T) {
	projects, repo, ctx := newProjects(t)
	wanted := errors.New("disk on fire")
	repo.fail("Create", wanted)

	if _, err := projects.Create(ctx, "venekambio"); !errors.Is(err, wanted) {
		t.Errorf("error = %v, want it to wrap %v", err, wanted)
	}
}

func TestListIncludesKeys(t *testing.T) {
	projects, _, ctx := newProjects(t)

	empty, err := projects.List(ctx)
	if err != nil {
		t.Fatalf("listing nothing: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("got %d views from an empty store", len(empty))
	}

	if _, err := projects.Create(ctx, "despacha"); err != nil {
		t.Fatalf("creating: %v", err)
	}
	if _, err := projects.Create(ctx, "repuestos"); err != nil {
		t.Fatalf("creating: %v", err)
	}

	views, err := projects.List(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	for _, view := range views {
		if _, ok := view.PrimaryDSN(); !ok {
			t.Errorf("project %q has no DSN", view.Project.Name)
		}
	}
}

func TestRotateKeyKeepsTheOldKeyUsable(t *testing.T) {
	projects, _, ctx := newProjects(t)

	created, err := projects.Create(ctx, "rotating")
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	original, _ := created.PrimaryDSN()

	rotated, err := projects.RotateKey(ctx, created.Project.ID)
	if err != nil {
		t.Fatalf("rotating: %v", err)
	}
	if rotated.Key.PublicKey == original.PublicKey {
		t.Error("rotation returned the same key")
	}

	view, err := projects.Get(ctx, created.Project.ID)
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	if len(view.Keys) != 2 {
		t.Fatalf("got %d active keys after rotating, want 2", len(view.Keys))
	}

	// The primary DSN must not jump to the new key: deployments still carry
	// the old one, and rotation is complete only once it is revoked.
	primary, _ := view.PrimaryDSN()
	if primary.PublicKey != original.PublicKey {
		t.Errorf("primary DSN switched to the new key %q before the old one was retired", primary.PublicKey)
	}

	if err := projects.RevokeKey(ctx, original.PublicKey); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	after, err := projects.Get(ctx, created.Project.ID)
	if err != nil {
		t.Fatalf("getting: %v", err)
	}
	if len(after.Keys) != 1 {
		t.Fatalf("got %d active keys after completing rotation, want 1", len(after.Keys))
	}
	if newPrimary, _ := after.PrimaryDSN(); newPrimary.PublicKey != rotated.Key.PublicKey {
		t.Errorf("primary DSN = %q, want the rotated key %q", newPrimary.PublicKey, rotated.Key.PublicKey)
	}
}

func TestRotateKeyOnMissingProject(t *testing.T) {
	projects, _, ctx := newProjects(t)

	if _, err := projects.RotateKey(ctx, 404); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("error = %v, want ErrProjectNotFound", err)
	}
}

func TestGetAndDeleteMissingProject(t *testing.T) {
	projects, _, ctx := newProjects(t)

	if _, err := projects.Get(ctx, 404); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("Get error = %v, want ErrProjectNotFound", err)
	}
	if err := projects.Delete(ctx, 404); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("Delete error = %v, want ErrProjectNotFound", err)
	}
}

func TestPrimaryDSNOfAKeylessProject(t *testing.T) {
	// Not reachable through Create, but a view built from a store in an
	// unexpected state must not panic.
	if _, ok := (ProjectView{}).PrimaryDSN(); ok {
		t.Error("a project with no keys reported a primary DSN")
	}
}
