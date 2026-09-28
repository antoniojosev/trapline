package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// fakeReleases is an in-memory stand-in for the release store, so the use
// case's own decisions — validate, create-when-missing, read together — are
// tested without a database deciding half of them.
type fakeReleases struct {
	releases   map[string]domain.Release
	commits    map[int64][]domain.Commit
	deploys    map[int64][]domain.Deploy
	nextID     int64
	ensured    []string
	statsError error
}

func newFakeReleases() *fakeReleases {
	return &fakeReleases{
		releases: map[string]domain.Release{},
		commits:  map[int64][]domain.Commit{},
		deploys:  map[int64][]domain.Deploy{},
	}
}

func (f *fakeReleases) Ensure(_ context.Context, projectID int64, version string, at time.Time) error {
	f.ensured = append(f.ensured, version)
	release, found := f.releases[version]
	if !found {
		f.nextID++
		release = domain.Release{ID: f.nextID, ProjectID: projectID, Version: version, CreatedAt: at}
	}
	if release.FirstEventAt == nil {
		release.FirstEventAt = &at
	}
	release.LastEventAt = &at
	f.releases[version] = release
	return nil
}

func (f *fakeReleases) Create(_ context.Context, release domain.Release) (domain.Release, bool, error) {
	if existing, found := f.releases[release.Version]; found {
		return existing, false, nil
	}
	f.nextID++
	release.ID = f.nextID
	f.releases[release.Version] = release
	return release, true, nil
}

func (f *fakeReleases) Find(_ context.Context, _ int64, version string) (domain.Release, error) {
	release, found := f.releases[version]
	if !found {
		return domain.Release{}, domain.ErrReleaseNotFound
	}
	return release, nil
}

func (f *fakeReleases) List(_ context.Context, _ int64, _ int) ([]domain.Release, error) {
	var releases []domain.Release
	for _, release := range f.releases {
		releases = append(releases, release)
	}
	return releases, nil
}

func (f *fakeReleases) Finalize(_ context.Context, _ int64, version string, at time.Time) (domain.Release, error) {
	release, found := f.releases[version]
	if !found {
		return domain.Release{}, domain.ErrReleaseNotFound
	}
	release = release.Finalize(at)
	f.releases[version] = release
	return release, nil
}

func (f *fakeReleases) SetCommits(_ context.Context, releaseID int64, commits []domain.Commit) error {
	f.commits[releaseID] = commits
	return nil
}

func (f *fakeReleases) Commits(_ context.Context, releaseID int64) ([]domain.Commit, error) {
	return f.commits[releaseID], nil
}

func (f *fakeReleases) AddDeploy(_ context.Context, deploy domain.Deploy) (domain.Deploy, error) {
	deploy.ID = int64(len(f.deploys[deploy.ReleaseID]) + 1)
	f.deploys[deploy.ReleaseID] = append(f.deploys[deploy.ReleaseID], deploy)
	return deploy, nil
}

func (f *fakeReleases) Deploys(_ context.Context, releaseID int64) ([]domain.Deploy, error) {
	return f.deploys[releaseID], nil
}

func (f *fakeReleases) Stats(_ context.Context, _ int64, _ string) (ports.ReleaseStats, error) {
	if f.statsError != nil {
		return ports.ReleaseStats{}, f.statsError
	}
	return ports.ReleaseStats{NewIssues: 2, RegressedIssues: 1, Events: 41}, nil
}

func (f *fakeReleases) Order(_ context.Context, _ int64) (domain.ReleaseOrder, error) {
	order := domain.ReleaseOrder{}
	for version, release := range f.releases {
		order[version] = release.ID
	}
	return order, nil
}

// fakeResolution stands in for the hand-driven half of an issue's lifecycle.
type fakeResolution struct{}

func (fakeResolution) Resolve(_ context.Context, _, _ int64, _ time.Time, _ bool) (domain.Issue, error) {
	return domain.Issue{}, nil
}

func (fakeResolution) SetStatus(_ context.Context, _, _ int64, _ domain.IssueStatus) error {
	return nil
}

func (fakeResolution) Resolution(_ context.Context, _, _ int64) (ports.IssueResolution, error) {
	return ports.IssueResolution{FirstRelease: "app@1.0.0"}, nil
}

func newReleasesUseCase(t *testing.T) (*Releases, *fakeReleases, *fakeResolution) {
	t.Helper()
	repo := newFakeReleases()
	resolution := &fakeResolution{}
	projects := &fakeProjectRepo{}
	return NewReleases(repo, resolution, projects, fixedClock{now: testNow}), repo, resolution
}

func TestReleasesRefuseToWorkOnAProjectThatDoesNotExist(t *testing.T) {
	// An empty listing is what a quiet project looks like, and a typo must
	// not be able to impersonate one.
	repo := newFakeReleases()
	uc := NewReleases(repo, &fakeResolution{}, &fakeProjectRepo{missing: true}, fixedClock{now: testNow})
	ctx := context.Background()

	if _, err := uc.List(ctx, 7, 0); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("List: %v", err)
	}
	if _, _, err := uc.Create(ctx, 7, "app@1.0.0", nil); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("Create: %v", err)
	}
	if _, err := uc.Get(ctx, 7, "app@1.0.0"); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("Get: %v", err)
	}
	if _, err := uc.Finalize(ctx, 7, "app@1.0.0", nil); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("Finalize: %v", err)
	}
	if _, err := uc.SetCommits(ctx, 7, "app@1.0.0", nil); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("SetCommits: %v", err)
	}
	if _, err := uc.AddDeploy(ctx, 7, "app@1.0.0", domain.Deploy{Environment: "production"}); !errors.Is(err, domain.ErrProjectNotFound) {
		t.Errorf("AddDeploy: %v", err)
	}
}

func TestCreateRejectsAVersionThatIsNotOne(t *testing.T) {
	uc, _, _ := newReleasesUseCase(t)
	if _, _, err := uc.Create(context.Background(), 1, "  ", nil); !errors.Is(err, domain.ErrInvalidRelease) {
		t.Errorf("error = %v, want ErrInvalidRelease", err)
	}
}

func TestCreateHonoursAnExplicitReleaseDate(t *testing.T) {
	uc, _, _ := newReleasesUseCase(t)
	shipped := testNow.Add(-48 * time.Hour)

	release, created, err := uc.Create(context.Background(), 1, "app@1.0.0", &shipped)
	if err != nil || !created {
		t.Fatalf("Create: %v, created=%v", err, created)
	}
	if release.DateReleased == nil || !release.DateReleased.Equal(shipped) {
		t.Errorf("DateReleased = %v, want %v", release.DateReleased, shipped)
	}
}

func TestSetCommitsRegistersAReleaseNobodyCreatedYet(t *testing.T) {
	// set-commits is often the first thing a pipeline runs; failing it would
	// make the order of two deploy steps matter for no reason a user could
	// guess.
	uc, repo, _ := newReleasesUseCase(t)

	count, err := uc.SetCommits(context.Background(), 1, "app@1.0.0", []domain.Commit{
		{SHA: "aaa"}, {SHA: ""}, {SHA: "aaa"},
	})
	if err != nil {
		t.Fatalf("SetCommits: %v", err)
	}
	if count != 1 {
		t.Errorf("stored %d commits, want 1 — the empty and the duplicate go", count)
	}
	if _, found := repo.releases["app@1.0.0"]; !found {
		t.Error("the release was not registered")
	}
}

func TestAddDeployRegistersAReleaseNobodyCreatedYet(t *testing.T) {
	uc, repo, _ := newReleasesUseCase(t)

	deploy, err := uc.AddDeploy(context.Background(), 1, "app@1.0.0", domain.Deploy{
		Environment: "production", Name: "pipeline-42",
	})
	if err != nil {
		t.Fatalf("AddDeploy: %v", err)
	}
	if deploy.Name != "pipeline-42" || deploy.FinishedAt == nil {
		t.Errorf("deploy = %+v", deploy)
	}
	if _, found := repo.releases["app@1.0.0"]; !found {
		t.Error("the release was not registered")
	}
}

func TestAddDeployNeedsAnEnvironment(t *testing.T) {
	uc, _, _ := newReleasesUseCase(t)
	if _, err := uc.AddDeploy(context.Background(), 1, "app@1.0.0", domain.Deploy{}); !errors.Is(err, domain.ErrInvalidRelease) {
		t.Errorf("error = %v, want ErrInvalidRelease", err)
	}
}

func TestGetReadsEverythingInOneGo(t *testing.T) {
	// Every consumer wants all of it: the panel renders it on one page, and
	// an agent should not need four round trips to learn what a release did.
	uc, repo, _ := newReleasesUseCase(t)
	ctx := context.Background()
	if _, _, err := uc.Create(ctx, 1, "app@1.0.0", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := uc.SetCommits(ctx, 1, "app@1.0.0", []domain.Commit{{SHA: "aaa"}}); err != nil {
		t.Fatalf("SetCommits: %v", err)
	}
	if _, err := uc.AddDeploy(ctx, 1, "app@1.0.0", domain.Deploy{Environment: "production"}); err != nil {
		t.Fatalf("AddDeploy: %v", err)
	}

	detail, err := uc.Get(ctx, 1, "app@1.0.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.Stats.NewIssues != 2 || detail.Stats.RegressedIssues != 1 || detail.Stats.Events != 41 {
		t.Errorf("stats = %+v", detail.Stats)
	}
	if len(detail.Commits) != 1 || len(detail.Deploys) != 1 {
		t.Errorf("detail = %+v", detail)
	}
	_ = repo
}

func TestGetPropagatesAFailureToCount(t *testing.T) {
	uc, repo, _ := newReleasesUseCase(t)
	repo.statsError = errors.New("counting broke")
	if _, _, err := uc.Create(context.Background(), 1, "app@1.0.0", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := uc.Get(context.Background(), 1, "app@1.0.0"); err == nil {
		t.Error("a failed count was reported as a successful read")
	}
}

func TestFinalizeDefaultsToNow(t *testing.T) {
	uc, _, _ := newReleasesUseCase(t)
	ctx := context.Background()
	if _, _, err := uc.Create(ctx, 1, "app@1.0.0", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}

	release, err := uc.Finalize(ctx, 1, "app@1.0.0", nil)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if release.DateReleased == nil || !release.DateReleased.Equal(testNow) {
		t.Errorf("DateReleased = %v, want the clock's now", release.DateReleased)
	}
}

func TestResolutionIsReadThroughTheUseCase(t *testing.T) {
	uc, _, _ := newReleasesUseCase(t)
	resolution, err := uc.Resolution(context.Background(), 1, 5)
	if err != nil {
		t.Fatalf("Resolution: %v", err)
	}
	if resolution.FirstRelease != "app@1.0.0" {
		t.Errorf("FirstRelease = %q", resolution.FirstRelease)
	}
}

// fakeProjectRepo answers only the one question the release use case asks of
// projects: does this one exist? The rest of ports.ProjectRepository is
// unreachable from here, so it is present to satisfy the interface and
// nothing else.
type fakeProjectRepo struct{ missing bool }

func (f *fakeProjectRepo) FindByID(_ context.Context, id int64) (domain.Project, error) {
	if f.missing {
		return domain.Project{}, domain.ErrProjectNotFound
	}
	return domain.Project{ID: id, Name: "venekambio"}, nil
}

// FindBySlug is the compatibility surface's lookup (ADR 013); the release use
// case reaches projects by id, so this fake answers the same project either
// way rather than pretending slugs do not exist.
func (f *fakeProjectRepo) FindBySlug(_ context.Context, slug string) (domain.Project, error) {
	if f.missing {
		return domain.Project{}, domain.ErrProjectNotFound
	}
	return domain.Project{ID: 1, Name: "venekambio", Slug: slug}, nil
}

func (f *fakeProjectRepo) Create(context.Context, domain.Project, domain.Key) (domain.Project, domain.Key, error) {
	panic("the release use case never creates a project")
}

// List is what the compatibility surface walks when a call names no project
// (ADR 013). One project is enough here; that a commit set reaches *every*
// project holding a version needs two releases with the same version in
// different projects, which this fake cannot hold — it is tested against the
// real database instead (httpapi/sentrycompat_handler_test.go).
func (f *fakeProjectRepo) List(context.Context) ([]domain.Project, error) {
	if f.missing {
		return nil, nil
	}
	return []domain.Project{{ID: 1, Name: "venekambio", Slug: "venekambio"}}, nil
}
func (f *fakeProjectRepo) Delete(context.Context, int64) error {
	panic("the release use case never deletes a project")
}
func (f *fakeProjectRepo) AddKey(context.Context, int64, domain.Key) (domain.Key, error) {
	panic("the release use case never touches keys")
}
func (f *fakeProjectRepo) ActiveKeys(context.Context, int64) ([]domain.Key, error) {
	panic("the release use case never touches keys")
}
func (f *fakeProjectRepo) FindActiveKey(context.Context, string) (domain.Key, error) {
	panic("the release use case never touches keys")
}
func (f *fakeProjectRepo) RevokeKey(context.Context, string) error {
	panic("the release use case never touches keys")
}
