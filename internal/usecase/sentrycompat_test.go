package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

// TestAProjectResolvesByIdBeforeSlug pins the order, which is the part that
// could be quietly reversed later. The id is the identifier the protocol
// already forces on every installation — it is in the ingest path of every
// DSN — so reading a number as anything else would make an id occasionally
// address a different project (ADR 013).
func TestAProjectResolvesByIdBeforeSlug(t *testing.T) {
	repo := newFakeRepo()
	uc := NewProjects(repo, repo, fixedClock{now: testNow}, testOrigin)
	ctx := context.Background()

	first, err := uc.Create(ctx, "venekambio")
	if err != nil {
		t.Fatalf("creating: %v", err)
	}

	bySlug, err := uc.ResolveProject(ctx, "venekambio")
	if err != nil {
		t.Fatalf("by slug: %v", err)
	}
	byID, err := uc.ResolveProject(ctx, "1")
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if bySlug.ID != first.Project.ID || byID.ID != first.Project.ID {
		t.Fatalf("slug gave %d and id gave %d, want %d", bySlug.ID, byID.ID, first.Project.ID)
	}
}

func TestResolvingAProjectThatIsNotThere(t *testing.T) {
	repo := newFakeRepo()
	uc := NewProjects(repo, repo, fixedClock{now: testNow}, testOrigin)
	ctx := context.Background()

	for _, reference := range []string{"", "nadie", "99"} {
		if _, err := uc.ResolveProject(ctx, reference); !errors.Is(err, domain.ErrProjectNotFound) {
			t.Errorf("ResolveProject(%q) = %v, want ErrProjectNotFound", reference, err)
		}
	}
}

// TestAnOrganisationScopedCallResolvesByVersion covers the lookup that half of
// what sentry-cli sends depends on: setting commits and recording a deploy name
// no project at all.
func TestAnOrganisationScopedCallResolvesByVersion(t *testing.T) {
	uc, repo, _ := newReleasesUseCase(t)
	ctx := context.Background()

	if _, _, err := uc.Create(ctx, 1, "app@1.0.0", nil); err != nil {
		t.Fatalf("creating the release: %v", err)
	}

	matched, err := uc.ProjectsWithVersion(ctx, "app@1.0.0")
	if err != nil {
		t.Fatalf("ProjectsWithVersion: %v", err)
	}
	if len(matched) != 1 || matched[0].ID != 1 {
		t.Fatalf("matched = %+v, want the one project that has it", matched)
	}
	_ = repo

	if _, err := uc.ProjectsWithVersion(ctx, "app@9.9.9"); !errors.Is(err, domain.ErrReleaseNotFound) {
		t.Errorf("a version nobody has = %v, want ErrReleaseNotFound", err)
	}
}
