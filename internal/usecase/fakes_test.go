package usecase

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
	"github.com/antoniojosev/trapline/internal/ports"
)

// fixedClock makes timestamps assertable instead of merely plausible.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// fakeRepo is an in-memory ports.ProjectRepository.
//
// The SQLite adapter is tested against a real database; this fake exists so
// use case tests exercise orchestration, including failure paths a real store
// would be awkward to provoke.
type fakeRepo struct {
	projects []domain.Project
	keys     []domain.Key
	nextID   int64

	failOn  map[string]error
	configs map[int64]domain.ProjectConfig
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{nextID: 1, failOn: map[string]error{}, configs: map[int64]domain.ProjectConfig{}}
}

// ProjectConfig and SetProjectConfig make the fake satisfy
// ports.ProjectConfigStore as well, so a use case test wires one object rather
// than two that have to agree about which projects exist.
func (r *fakeRepo) ProjectConfig(_ context.Context, projectID int64) (domain.ProjectConfig, error) {
	if err := r.check("ProjectConfig"); err != nil {
		return domain.ProjectConfig{}, err
	}
	return r.configs[projectID], nil
}

func (r *fakeRepo) SetProjectConfig(_ context.Context, projectID int64, config domain.ProjectConfig) error {
	if err := r.check("SetProjectConfig"); err != nil {
		return err
	}
	r.configs[projectID] = config
	return nil
}

func (r *fakeRepo) fail(method string, err error) { r.failOn[method] = err }

func (r *fakeRepo) check(method string) error { return r.failOn[method] }

func (r *fakeRepo) Create(_ context.Context, project domain.Project, key domain.Key) (domain.Project, domain.Key, error) {
	if err := r.check("Create"); err != nil {
		return domain.Project{}, domain.Key{}, err
	}
	project.ID = r.nextID
	r.nextID++

	bound, err := key.AssignTo(project.ID)
	if err != nil {
		return domain.Project{}, domain.Key{}, err
	}
	r.projects = append(r.projects, project)
	r.keys = append(r.keys, bound)
	return project, bound, nil
}

func (r *fakeRepo) FindByID(_ context.Context, id int64) (domain.Project, error) {
	if err := r.check("FindByID"); err != nil {
		return domain.Project{}, err
	}
	for _, project := range r.projects {
		if project.ID == id {
			return project, nil
		}
	}
	return domain.Project{}, fmt.Errorf("%w: id %d", domain.ErrProjectNotFound, id)
}

func (r *fakeRepo) FindBySlug(_ context.Context, slug string) (domain.Project, error) {
	if err := r.check("FindBySlug"); err != nil {
		return domain.Project{}, err
	}
	for _, project := range r.projects {
		if project.Slug == slug {
			return project, nil
		}
	}
	return domain.Project{}, fmt.Errorf("%w: slug %q", domain.ErrProjectNotFound, slug)
}

func (r *fakeRepo) List(_ context.Context) ([]domain.Project, error) {
	if err := r.check("List"); err != nil {
		return nil, err
	}
	return slices.Clone(r.projects), nil
}

func (r *fakeRepo) Delete(_ context.Context, id int64) error {
	if err := r.check("Delete"); err != nil {
		return err
	}
	for i, project := range r.projects {
		if project.ID == id {
			r.projects = slices.Delete(r.projects, i, i+1)
			r.keys = slices.DeleteFunc(r.keys, func(k domain.Key) bool { return k.ProjectID == id })
			return nil
		}
	}
	return fmt.Errorf("%w: id %d", domain.ErrProjectNotFound, id)
}

func (r *fakeRepo) AddKey(ctx context.Context, projectID int64, key domain.Key) (domain.Key, error) {
	if err := r.check("AddKey"); err != nil {
		return domain.Key{}, err
	}
	if _, err := r.FindByID(ctx, projectID); err != nil {
		return domain.Key{}, err
	}
	active, err := r.ActiveKeys(ctx, projectID)
	if err != nil {
		return domain.Key{}, err
	}
	if len(active) >= domain.MaxActiveKeys {
		return domain.Key{}, fmt.Errorf("%w: project %d", domain.ErrTooManyActiveKeys, projectID)
	}
	bound, err := key.AssignTo(projectID)
	if err != nil {
		return domain.Key{}, err
	}
	r.keys = append(r.keys, bound)
	return bound, nil
}

func (r *fakeRepo) ActiveKeys(_ context.Context, projectID int64) ([]domain.Key, error) {
	if err := r.check("ActiveKeys"); err != nil {
		return nil, err
	}
	var active []domain.Key
	for _, key := range r.keys {
		if key.ProjectID == projectID && key.Active() {
			active = append(active, key)
		}
	}
	return active, nil
}

func (r *fakeRepo) FindActiveKey(_ context.Context, publicKey string) (domain.Key, error) {
	if err := r.check("FindActiveKey"); err != nil {
		return domain.Key{}, err
	}
	for _, key := range r.keys {
		if key.PublicKey == publicKey && key.Active() {
			return key, nil
		}
	}
	return domain.Key{}, domain.ErrKeyNotFound
}

func (r *fakeRepo) RevokeKey(_ context.Context, publicKey string) error {
	if err := r.check("RevokeKey"); err != nil {
		return err
	}
	for i, key := range r.keys {
		if key.PublicKey == publicKey && key.Active() {
			r.keys[i] = key.Revoke(time.Now())
		}
	}
	return nil
}

// movableClock is a clock a test can advance, for expiry.
type movableClock struct{ now time.Time }

func (c *movableClock) Now() time.Time { return c.now }

// fakeTokenRepo is an in-memory ports.TokenRepository.
type fakeTokenRepo struct {
	tokens   []domain.APIToken
	touched  int
	touchErr error
}

func newFakeTokenRepo() *fakeTokenRepo { return &fakeTokenRepo{} }

func (r *fakeTokenRepo) Create(_ context.Context, token domain.APIToken) error {
	r.tokens = append(r.tokens, token)
	return nil
}

func (r *fakeTokenRepo) FindValid(_ context.Context, tokenHash string, now time.Time) (domain.APIToken, error) {
	for _, token := range r.tokens {
		if token.TokenHash == tokenHash && !token.Expired(now) {
			return token, nil
		}
	}
	return domain.APIToken{}, domain.ErrTokenNotFound
}

func (r *fakeTokenRepo) List(_ context.Context) ([]domain.APIToken, error) {
	return slices.Clone(r.tokens), nil
}

func (r *fakeTokenRepo) Delete(_ context.Context, tokenHash string) error {
	r.tokens = slices.DeleteFunc(r.tokens, func(t domain.APIToken) bool {
		return t.TokenHash == tokenHash
	})
	return nil
}

func (r *fakeTokenRepo) TouchLastUsed(_ context.Context, _ string, _ time.Time) error {
	r.touched++
	return r.touchErr
}

// allowAll and refuseAll are rate limiters with no opinion, so an ingest test
// can isolate the behaviour it is actually about.
type allowAll struct{}

func (allowAll) Allow(context.Context, int64, engine.Category) (bool, error) { return true, nil }

type refuseAll struct{}

func (refuseAll) Allow(context.Context, int64, engine.Category) (bool, error) { return false, nil }

// fakeIssues records what reached the repository, which is the last point
// before persistence.
type fakeIssues struct {
	recorded []ports.RecordEventInput
	issues   map[string]domain.Issue
	nextID   int64

	// Retention bookkeeping.
	deletable   int64
	deleteCalls []deleteCall
	deleteErr   error
}

func newFakeIssues() *fakeIssues {
	return &fakeIssues{issues: map[string]domain.Issue{}, nextID: 1}
}

func (f *fakeIssues) RecordEvent(_ context.Context, input *ports.RecordEventInput) (ports.RecordEventResult, error) {
	f.recorded = append(f.recorded, *input)

	existing, found := f.issues[input.Fingerprint]
	if !found {
		created, err := domain.NewIssue(input.ProjectID, input.Fingerprint, input.Observation)
		if err != nil {
			return ports.RecordEventResult{}, err
		}
		created.ID = f.nextID
		f.nextID++
		f.issues[input.Fingerprint] = created
		return ports.RecordEventResult{Issue: created, New: true}, nil
	}

	updated, regressed := existing.Observe(input.Observation)
	f.issues[input.Fingerprint] = updated
	return ports.RecordEventResult{Issue: updated, Regressed: regressed}, nil
}

func (f *fakeIssues) List(ctx context.Context, filter ports.IssueFilter) (ports.IssuePage, error) {
	issues := make([]domain.Issue, 0, len(f.issues))
	for _, issue := range f.issues {
		issues = append(issues, issue)
	}
	counts, err := f.CountsByStatus(ctx, filter.ProjectID)
	if err != nil {
		return ports.IssuePage{}, err
	}
	return ports.IssuePage{Issues: issues, Counts: counts}, nil
}

func (f *fakeIssues) CountsByStatus(context.Context, int64) (map[domain.IssueStatus]int64, error) {
	counts := map[domain.IssueStatus]int64{
		domain.StatusUnresolved: 0,
		domain.StatusResolved:   0,
		domain.StatusIgnored:    0,
	}
	for _, issue := range f.issues {
		counts[issue.Status]++
	}
	return counts, nil
}

func (f *fakeIssues) FindByID(_ context.Context, _, issueID int64) (domain.Issue, error) {
	for _, issue := range f.issues {
		if issue.ID == issueID {
			return issue, nil
		}
	}
	return domain.Issue{}, domain.ErrIssueNotFound
}

func (f *fakeIssues) SetStatus(_ context.Context, _, issueID int64, status domain.IssueStatus) error {
	for fingerprint, issue := range f.issues {
		if issue.ID == issueID {
			issue.Status = status
			f.issues[fingerprint] = issue
			return nil
		}
	}
	return domain.ErrIssueNotFound
}

func (f *fakeIssues) LatestEvents(context.Context, int64, int) ([]ports.StoredEvent, error) {
	return nil, nil
}

func (f *fakeIssues) Tags(context.Context, int64) (map[string][]ports.TagCount, error) {
	return nil, nil
}

func (f *fakeIssues) DeleteEventsBefore(
	_ context.Context, projectID int64, cutoff time.Time, limit int,
) (int64, error) {
	f.deleteCalls = append(f.deleteCalls, deleteCall{projectID: projectID, cutoff: cutoff, limit: limit})
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	batch := min(f.deletable, int64(limit))
	f.deletable -= batch
	return batch, nil
}

// deleteCall records one retention batch, so a test can assert the cutoff and
// the batching rather than only the total.
type deleteCall struct {
	projectID int64
	cutoff    time.Time
	limit     int
}

// fakeStats stands in for the aggregate store during retention tests.
//
// Deliberately separate from fakeIssues rather than a few more fields on it:
// the two windows are independent, and a test that could not tell which sweep
// deleted what would be unable to prove the property that matters — that the
// buckets outlive the events (ADR 010).
type fakeStats struct {
	deletable   int64
	deleteCalls []deleteCall
	deleteErr   error
}

func newFakeStats() *fakeStats { return &fakeStats{} }

func (f *fakeStats) ProjectSeries(context.Context, int64, domain.Range) ([]ports.LevelBucket, error) {
	return nil, nil
}

func (f *fakeStats) TopIssues(context.Context, int64, domain.Range, int) ([]ports.IssueCount, error) {
	return nil, nil
}

func (f *fakeStats) Breakdown(
	context.Context, int64, domain.Dimension, domain.Range, int,
) ([]ports.DimensionCount, error) {
	return nil, nil
}

func (f *fakeStats) IssueSeries(
	context.Context, int64, int64, domain.Range,
) ([]ports.HourCount, error) {
	return nil, nil
}

func (f *fakeStats) IssuesSeries(
	context.Context, int64, []int64, domain.Range,
) (map[int64][]ports.HourCount, error) {
	return nil, nil
}

func (f *fakeStats) DeleteAggregatesBefore(
	_ context.Context, projectID int64, cutoff time.Time, limit int,
) (int64, error) {
	f.deleteCalls = append(f.deleteCalls, deleteCall{projectID: projectID, cutoff: cutoff, limit: limit})
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	batch := min(f.deletable, int64(limit))
	f.deletable -= batch
	return batch, nil
}
