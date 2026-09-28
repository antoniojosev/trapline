package usecase

import (
	"context"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Issues is the read and triage side of the product: what is broken, and
// marking it resolved or ignored.
type Issues struct {
	repo ports.IssueRepository
	// resolution owns the release-aware half of the lifecycle: when something
	// was resolved, in which release, and whether it has come back. Optional
	// for the same reason ingestion's is — an assembly without it behaves
	// exactly as the product did before releases existed.
	resolution ports.IssueResolutionRepository
	clock      ports.Clock
}

// NewIssues wires the use case.
func NewIssues(repo ports.IssueRepository) *Issues {
	return &Issues{repo: repo}
}

// WithResolution turns on release-aware resolution.
func (i *Issues) WithResolution(resolution ports.IssueResolutionRepository, clock ports.Clock) *Issues {
	i.resolution = resolution
	i.clock = clock
	return i
}

// IssueDetail is one issue with everything a person or an agent needs to act
// on it.
type IssueDetail struct {
	Issue  domain.Issue
	Events []ports.StoredEvent
	Tags   map[string][]ports.TagCount
}

// List returns one page of a project's issues, with the status counts a
// filter bar needs to label itself.
func (i *Issues) List(ctx context.Context, filter ports.IssueFilter) (ports.IssuePage, error) {
	return i.repo.List(ctx, filter)
}

// Get returns one issue with its latest events and aggregated tags.
//
// Events and tags are fetched together rather than behind separate calls
// because every consumer wants all three: the panel renders them on one page,
// and the issue bundle an agent reads is exactly this, formatted.
func (i *Issues) Get(ctx context.Context, projectID, issueID int64, eventLimit int) (IssueDetail, error) {
	issue, err := i.repo.FindByID(ctx, projectID, issueID)
	if err != nil {
		return IssueDetail{}, err
	}
	events, err := i.repo.LatestEvents(ctx, issueID, eventLimit)
	if err != nil {
		return IssueDetail{}, err
	}
	tags, err := i.repo.Tags(ctx, issueID)
	if err != nil {
		return IssueDetail{}, err
	}
	return IssueDetail{Issue: issue, Events: events, Tags: tags}, nil
}

// Resolve marks an issue fixed. A later event reopens it as a regression,
// which is the signal this state exists to produce.
func (i *Issues) Resolve(ctx context.Context, projectID, issueID int64) error {
	return i.setStatus(ctx, projectID, issueID, domain.StatusResolved, false)
}

// ResolveInNextRelease marks an issue fixed as of the release it is currently
// being seen in, so only something newer counts as a regression.
//
// This is the difference between a status a user believes and one they learn
// to ignore: without it, resolving an issue and deploying the fix reopens it
// within seconds, from the instances that were still running the broken build
// when the rollout began (ADR 012).
func (i *Issues) ResolveInNextRelease(ctx context.Context, projectID, issueID int64) error {
	if i.resolution == nil {
		// Degrading to a plain resolve rather than failing: the weaker
		// behaviour is the old behaviour, and refusing would turn a missing
		// wiring into a user-visible error about a concept they did not ask
		// about.
		return i.Resolve(ctx, projectID, issueID)
	}
	return i.setStatus(ctx, projectID, issueID, domain.StatusResolved, true)
}

// Ignore mutes an issue without claiming it is fixed. Events still count.
func (i *Issues) Ignore(ctx context.Context, projectID, issueID int64) error {
	return i.setStatus(ctx, projectID, issueID, domain.StatusIgnored, false)
}

// Reopen puts an issue back in the unresolved state by hand.
func (i *Issues) Reopen(ctx context.Context, projectID, issueID int64) error {
	return i.setStatus(ctx, projectID, issueID, domain.StatusUnresolved, false)
}

// setStatus routes a triage decision to whichever half of storage owns it.
//
// When the release lifecycle is wired, every transition goes through it: the
// resolution columns and the status have to move together, or an ignored
// issue keeps a pin that a later event would still be measured against.
func (i *Issues) setStatus(
	ctx context.Context, projectID, issueID int64, status domain.IssueStatus, inNextRelease bool,
) error {
	if i.resolution == nil {
		return i.repo.SetStatus(ctx, projectID, issueID, status)
	}
	if status == domain.StatusResolved {
		_, err := i.resolution.Resolve(ctx, projectID, issueID, i.clock.Now(), inNextRelease)
		return err
	}
	return i.resolution.SetStatus(ctx, projectID, issueID, status)
}
