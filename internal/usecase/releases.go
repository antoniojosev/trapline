package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Releases is the deploy side of the product: what shipped, what went into
// it, where it went, and what it broke.
//
// It is the use case that makes "is this new or is it back, and which release
// brought it?" answerable — the question the whole phase exists to answer.
type Releases struct {
	repo       ports.ReleaseRepository
	resolution ports.IssueResolutionRepository
	projects   ports.ProjectRepository
	clock      ports.Clock
}

// NewReleases wires the use case.
func NewReleases(
	repo ports.ReleaseRepository,
	resolution ports.IssueResolutionRepository,
	projects ports.ProjectRepository,
	clock ports.Clock,
) *Releases {
	return &Releases{repo: repo, resolution: resolution, projects: projects, clock: clock}
}

// List returns a project's releases, most recently discovered first.
func (r *Releases) List(ctx context.Context, projectID int64, limit int) ([]domain.Release, error) {
	if _, err := r.projects.FindByID(ctx, projectID); err != nil {
		return nil, err
	}
	return r.repo.List(ctx, projectID, limit)
}

// Create registers a release, as a deploy tool does.
//
// Idempotent by design: a pipeline that reruns a step, or a deploy annotation
// that arrives after the first error already created the release implicitly,
// must both succeed. The boolean is for the transport to answer 201 or 200,
// not for anybody to branch on.
func (r *Releases) Create(
	ctx context.Context, projectID int64, version string, dateReleased *time.Time,
) (domain.Release, bool, error) {
	if _, err := r.projects.FindByID(ctx, projectID); err != nil {
		return domain.Release{}, false, err
	}
	release, err := domain.NewRelease(projectID, version, r.clock.Now())
	if err != nil {
		return domain.Release{}, false, err
	}
	if dateReleased != nil {
		released := dateReleased.UTC()
		release.DateReleased = &released
	}
	return r.repo.Create(ctx, release)
}

// Get returns one release with everything a person or an agent reads at once.
//
// Commits, deploys and the issue counts are fetched together rather than
// behind separate calls because every consumer wants all of them: the panel
// renders them on one page, and an agent asking "what did this release do"
// should not have to make four round trips to find out.
func (r *Releases) Get(ctx context.Context, projectID int64, version string) (ports.ReleaseDetail, error) {
	if _, err := r.projects.FindByID(ctx, projectID); err != nil {
		return ports.ReleaseDetail{}, err
	}
	release, err := r.repo.Find(ctx, projectID, version)
	if err != nil {
		return ports.ReleaseDetail{}, err
	}
	stats, err := r.repo.Stats(ctx, projectID, release.Version)
	if err != nil {
		return ports.ReleaseDetail{}, err
	}
	commits, err := r.repo.Commits(ctx, release.ID)
	if err != nil {
		return ports.ReleaseDetail{}, err
	}
	deploys, err := r.repo.Deploys(ctx, release.ID)
	if err != nil {
		return ports.ReleaseDetail{}, err
	}
	return ports.ReleaseDetail{Release: release, Stats: stats, Commits: commits, Deploys: deploys}, nil
}

// Finalize records that a release was declared shipped.
func (r *Releases) Finalize(
	ctx context.Context, projectID int64, version string, at *time.Time,
) (domain.Release, error) {
	if _, err := r.projects.FindByID(ctx, projectID); err != nil {
		return domain.Release{}, err
	}
	when := r.clock.Now()
	if at != nil {
		when = at.UTC()
	}
	return r.repo.Finalize(ctx, projectID, version, when)
}

// SetCommits replaces a release's commit set.
//
// Creating the release when it does not exist yet, because `set-commits` is
// often the first thing a pipeline runs and failing it would make the order
// of two deploy steps matter for no reason a user could guess.
func (r *Releases) SetCommits(
	ctx context.Context, projectID int64, version string, commits []domain.Commit,
) (int, error) {
	release, err := r.findOrCreate(ctx, projectID, version)
	if err != nil {
		return 0, err
	}
	cleaned, err := domain.CleanCommits(commits)
	if err != nil {
		return 0, err
	}
	if err := r.repo.SetCommits(ctx, release.ID, cleaned); err != nil {
		return 0, err
	}
	return len(cleaned), nil
}

// AddDeploy records one release going out to one environment.
func (r *Releases) AddDeploy(
	ctx context.Context, projectID int64, version string, deploy domain.Deploy,
) (domain.Deploy, error) {
	release, err := r.findOrCreate(ctx, projectID, version)
	if err != nil {
		return domain.Deploy{}, err
	}
	built, err := domain.NewDeploy(release.ID, deploy.Environment, r.clock.Now())
	if err != nil {
		return domain.Deploy{}, err
	}
	built.Name = deploy.Name
	built.URL = deploy.URL
	built.StartedAt = deploy.StartedAt
	if deploy.FinishedAt != nil {
		built.FinishedAt = deploy.FinishedAt
	}
	return r.repo.AddDeploy(ctx, built)
}

// Resolution reads an issue's release-aware resolution state.
func (r *Releases) Resolution(
	ctx context.Context, projectID, issueID int64,
) (ports.IssueResolution, error) {
	return r.resolution.Resolution(ctx, projectID, issueID)
}

// EnsureVersion returns a release, registering it if this is the first anyone
// has mentioned it.
//
// Exported for the artefact upload path, which has the same problem
// `set-commits` has and solves it the same way: uploading source maps for
// `app@1.4.0` is often the first step of a pipeline, and failing it because
// nobody has run `releases new` yet would make the order of two deploy steps
// matter for a reason no user could guess.
func (r *Releases) EnsureVersion(ctx context.Context, projectID int64, version string) (domain.Release, error) {
	return r.findOrCreate(ctx, projectID, version)
}

// findOrCreate returns a release, registering it if this is the first anyone
// has mentioned it.
func (r *Releases) findOrCreate(ctx context.Context, projectID int64, version string) (domain.Release, error) {
	if _, err := r.projects.FindByID(ctx, projectID); err != nil {
		return domain.Release{}, err
	}
	release, err := r.repo.Find(ctx, projectID, version)
	if err == nil {
		return release, nil
	}
	if !isReleaseMissing(err) {
		return domain.Release{}, err
	}

	created, err := domain.NewRelease(projectID, version, r.clock.Now())
	if err != nil {
		return domain.Release{}, err
	}
	saved, _, err := r.repo.Create(ctx, created)
	if err != nil {
		return domain.Release{}, fmt.Errorf("registering release %q: %w", version, err)
	}
	return saved, nil
}

// isReleaseMissing reports whether an error is "no such release", which is a
// normal outcome here rather than a failure.
func isReleaseMissing(err error) bool {
	return errors.Is(err, domain.ErrReleaseNotFound)
}
