package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

var testNow = time.Date(2026, 8, 22, 15, 4, 5, 0, time.UTC)

func newRepo(t *testing.T) (*ProjectRepository, context.Context) {
	t.Helper()
	return NewProjectRepository(openTemp(t)), context.Background()
}

// createProject is the two-step the domain requires: mint an unassigned key,
// then let the store bind it.
func createProject(t *testing.T, repo *ProjectRepository, name string) (domain.Project, domain.Key) {
	t.Helper()
	ctx := context.Background()
	project, err := domain.NewProject(name, testNow)
	if err != nil {
		t.Fatalf("building project: %v", err)
	}
	key, err := domain.NewKey(testNow)
	if err != nil {
		t.Fatalf("minting key: %v", err)
	}
	saved, savedKey, err := repo.Create(ctx, project, key)
	if err != nil {
		t.Fatalf("creating project: %v", err)
	}
	return saved, savedKey
}

func TestCreateAssignsIDAndBindsKey(t *testing.T) {
	repo, ctx := newRepo(t)

	project, key := createProject(t, repo, "venekambio")

	if project.ID <= 0 {
		t.Errorf("ID = %d, want a positive assigned id", project.ID)
	}
	if key.ProjectID != project.ID {
		t.Errorf("key bound to project %d, want %d", key.ProjectID, project.ID)
	}
	if !key.Active() {
		t.Error("the first key must be active")
	}

	// The round trip is what matters: what was written must read back equal.
	found, err := repo.FindByID(ctx, project.ID)
	if err != nil {
		t.Fatalf("finding project: %v", err)
	}
	if found.Name != "venekambio" {
		t.Errorf("Name = %q, want %q", found.Name, "venekambio")
	}
	if !found.CreatedAt.Equal(testNow) {
		t.Errorf("CreatedAt = %v, want %v", found.CreatedAt, testNow)
	}
	if found.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", found.CreatedAt.Location())
	}
}

func TestCreateAssignsIncreasingIDs(t *testing.T) {
	repo, _ := newRepo(t)

	first, _ := createProject(t, repo, "first")
	second, _ := createProject(t, repo, "second")

	if second.ID <= first.ID {
		t.Errorf("ids are not increasing: %d then %d", first.ID, second.ID)
	}
}

func TestFindByIDMissing(t *testing.T) {
	repo, ctx := newRepo(t)

	if _, err := repo.FindByID(ctx, 404); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("error = %v, want ErrProjectNotFound", err)
	}
}

func TestList(t *testing.T) {
	repo, ctx := newRepo(t)

	empty, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("listing an empty store: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("got %d projects from an empty store", len(empty))
	}

	createProject(t, repo, "despacha")
	createProject(t, repo, "repuestos")

	projects, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(projects))
	}
	if projects[0].Name != "despacha" || projects[1].Name != "repuestos" {
		t.Errorf("order = %q then %q, want oldest first", projects[0].Name, projects[1].Name)
	}
}

func TestDeleteCascadesToKeys(t *testing.T) {
	repo, ctx := newRepo(t)
	project, key := createProject(t, repo, "gone")

	if err := repo.Delete(ctx, project.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if _, err := repo.FindByID(ctx, project.ID); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("project still found after delete: %v", err)
	}
	// The cascade only works because foreign_keys is ON; SQLite defaults it
	// off, so this assertion is really testing the pragma.
	if _, err := repo.FindActiveKey(ctx, key.PublicKey); !errors.Is(err, domain.ErrKeyNotFound) {
		t.Errorf("key survived the project: %v", err)
	}
}

func TestDeleteMissing(t *testing.T) {
	repo, ctx := newRepo(t)

	if err := repo.Delete(ctx, 404); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("error = %v, want ErrProjectNotFound", err)
	}
}
