package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// ReleaseStats are the numbers a release detail is read for.
//
// They answer the two questions a deploy raises — "did I break anything new"
// and "did I bring something back" — and they are counted from indexed
// columns on `issues`, never by scanning events (ADR 001).
type ReleaseStats struct {
	// NewIssues are issues whose first event carried this release.
	NewIssues int64
	// RegressedIssues are issues that have come back at least once and were
	// last seen in this release.
	RegressedIssues int64
	// Events is how many events this release produced, summed from the hourly
	// aggregates (ADR 010) and never by scanning the event table (ADR 001).
	// The aggregates outlive the payloads they were counted from, so this
	// number stays right after retention has deleted the events.
	Events int64
}

// ReleaseDetail is a release with everything a person or an agent reads at
// once, in one round trip.
type ReleaseDetail struct {
	Release domain.Release
	Stats   ReleaseStats
	Commits []domain.Commit
	Deploys []domain.Deploy
}

// ReleaseRepository stores releases, what went into them and where they went.
type ReleaseRepository interface {
	// Ensure records that a release exists and that an event arrived from it,
	// creating it when it is new. It is one statement, and it is the same one
	// the ingest transaction runs inline (ADR 032); this is the form for a
	// caller that has no transaction of its own.
	Ensure(ctx context.Context, projectID int64, version string, at time.Time) error

	// Create registers a release explicitly, as a deploy tool does. It
	// reports whether the release was new, so a retried pipeline step is not
	// an error.
	Create(ctx context.Context, release domain.Release) (saved domain.Release, created bool, err error)

	// Find returns one release, or domain.ErrReleaseNotFound.
	Find(ctx context.Context, projectID int64, version string) (domain.Release, error)

	// List returns a project's releases, newest first sight first.
	List(ctx context.Context, projectID int64, limit int) ([]domain.Release, error)

	// Finalize records when a release was declared shipped. Finalising twice
	// keeps the first date.
	Finalize(ctx context.Context, projectID int64, version string, at time.Time) (domain.Release, error)

	// SetCommits replaces a release's commit set, paths included. Replacing
	// rather than appending, because a deploy tool sends the whole set and a
	// re-run must not double it.
	SetCommits(ctx context.Context, releaseID int64, commits []domain.Commit) error

	// Commits returns a release's commits in the order they were sent.
	Commits(ctx context.Context, releaseID int64) ([]domain.Commit, error)

	// AddDeploy records one release going out to one environment.
	AddDeploy(ctx context.Context, deploy domain.Deploy) (domain.Deploy, error)

	// Deploys returns a release's deploys, most recent first.
	Deploys(ctx context.Context, releaseID int64) ([]domain.Deploy, error)

	// Stats counts what a release did to a project's issues.
	Stats(ctx context.Context, projectID int64, version string) (ReleaseStats, error)

	// Order is the order a project's releases were first seen in, which is
	// the only way to compare release identifiers that are not versions.
	Order(ctx context.Context, projectID int64) (domain.ReleaseOrder, error)
}

// IssueResolution is the part of an issue's lifecycle that releases own.
//
// It is a type of its own rather than more fields on every listing because it
// is read on one screen — the issue detail — and carrying six more columns
// through every page of a list would cost every reader for one reader's
// benefit.
type IssueResolution struct {
	Status                     domain.IssueStatus
	FirstRelease               string
	ResolvedAt                 *time.Time
	ResolvedInRelease          string
	ResolveNextRelease         bool
	Regressions                int64
	RegressedInRelease         string
	SeenInResolvedReleaseCount int64
}

// IssueResolutionRepository writes the resolution columns of `issues` when a
// person moves an issue by hand.
//
// A port of its own, separate from IssueRepository, because these are the
// deliberate transitions — somebody clicking resolve, ignore or reopen — and
// they have nothing to do with the arrival of an event. What an event does to
// the same columns is decided and written by RecordEvent, in the transaction
// that stores the event (ADR 032).
type IssueResolutionRepository interface {
	// Resolve marks an issue fixed, either as of now or as of the release it
	// is currently being seen in.
	Resolve(ctx context.Context, projectID, issueID int64, at time.Time, inNextRelease bool) (domain.Issue, error)

	// SetStatus moves an issue to a status that is not "resolved", forgetting
	// any resolution that no longer applies.
	SetStatus(ctx context.Context, projectID, issueID int64, status domain.IssueStatus) error

	// Resolution reads an issue's resolution columns.
	Resolution(ctx context.Context, projectID, issueID int64) (IssueResolution, error)
}
