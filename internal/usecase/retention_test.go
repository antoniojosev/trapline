package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
)

func newRetention(t *testing.T) (*Retention, *fakeRepo, *fakeIssues, context.Context) {
	t.Helper()
	retention, projects, issues, _, ctx := newRetentionWithStats(t)
	return retention, projects, issues, ctx
}

func newRetentionWithStats(t *testing.T) (*Retention, *fakeRepo, *fakeIssues, *fakeStats, context.Context) {
	t.Helper()
	projects, issues, stats := newFakeRepo(), newFakeIssues(), newFakeStats()
	retention := NewRetention(projects, projects, issues, stats, fixedClock{now: testNow})
	// Batches and pauses are the production defaults; a test that waits fifty
	// milliseconds per batch is a test somebody eventually deletes.
	retention.BatchSize = 3
	retention.Pause = 0
	return retention, projects, issues, stats, context.Background()
}

func TestSweepUsesTheConfiguredWindow(t *testing.T) {
	retention, projects, issues, ctx := newRetention(t)

	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	if err := projects.SetProjectConfig(ctx, 1, domain.ProjectConfig{
		RetentionDays: map[string]int{"error": 7},
	}); err != nil {
		t.Fatalf("configuring: %v", err)
	}

	if _, err := retention.Sweep(ctx); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	if len(issues.deleteCalls) == 0 {
		t.Fatal("no delete was attempted")
	}
	want := testNow.Add(-7 * 24 * time.Hour)
	if got := issues.deleteCalls[0].cutoff; !got.Equal(want) {
		t.Errorf("cutoff = %v, want %v (the project's own seven days)", got, want)
	}
}

func TestSweepFallsBackToTheEngineDefault(t *testing.T) {
	retention, projects, issues, ctx := newRetention(t)
	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}

	if _, err := retention.Sweep(ctx); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	want := testNow.Add(-engine.DefaultRetention()[engine.CategoryError])
	if got := issues.deleteCalls[0].cutoff; !got.Equal(want) {
		t.Errorf("cutoff = %v, want the engine default %v", got, want)
	}
}

func TestSweepDeletesInBatchesUntilDrained(t *testing.T) {
	// Batched because a long write transaction on a single-writer store stalls
	// ingestion, and an error tracker that stops accepting events while
	// tidying up has failed at the only moment that matters.
	retention, projects, issues, ctx := newRetention(t)
	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	issues.deletable = 7 // batch size is 3, so: 3, 3, 1

	result, err := retention.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	if result.Deleted != 7 {
		t.Errorf("Deleted = %d, want 7", result.Deleted)
	}
	if len(issues.deleteCalls) != 3 {
		t.Errorf("%d delete calls, want 3 batches", len(issues.deleteCalls))
	}
	for _, call := range issues.deleteCalls {
		if call.limit != 3 {
			t.Errorf("a batch asked for %d rows, want the configured 3", call.limit)
		}
	}
}

func TestSweepCoversEveryProject(t *testing.T) {
	retention, projects, issues, ctx := newRetention(t)
	for _, name := range []string{"a", "b", "c"} {
		if _, _, err := projects.Create(ctx, domain.Project{Name: name, CreatedAt: testNow}, mustKey(t)); err != nil {
			t.Fatalf("creating project: %v", err)
		}
	}

	result, err := retention.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if result.Projects != 3 {
		t.Errorf("Projects = %d, want 3", result.Projects)
	}

	seen := map[int64]bool{}
	for _, call := range issues.deleteCalls {
		seen[call.projectID] = true
	}
	if len(seen) != 3 {
		t.Errorf("swept %d projects, want all 3", len(seen))
	}
}

func TestSweepStopsOnAStoreFailure(t *testing.T) {
	retention, projects, issues, ctx := newRetention(t)
	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	wanted := errors.New("disk on fire")
	issues.deleteErr = wanted

	if _, err := retention.Sweep(ctx); !errors.Is(err, wanted) {
		t.Errorf("error = %v, want it to wrap %v", err, wanted)
	}
}

func TestJobDescribesItself(t *testing.T) {
	// The scheduler reads both of these once, when it starts the job, and
	// reports them to whoever asks what is running. A job that renamed itself
	// or lied about its interval would make that report useless.
	retention, _, _, _ := newRetention(t)
	retention.Interval = 42 * time.Minute

	job := retention.Job()
	if job.Name() != "retention" {
		t.Errorf("Name() = %q, want retention", job.Name())
	}
	if job.Interval() != 42*time.Minute {
		t.Errorf("Interval() = %s, want the sweeper's own", job.Interval())
	}
}

func TestJobSweepsAndReportsFailure(t *testing.T) {
	// The loop that used to live here logged its own failures and carried on.
	// Now the job returns them and the scheduler decides — which is what stops
	// every future job from having to remember to do the same.
	retention, projects, issues, ctx := newRetention(t)
	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}

	if err := retention.Job().Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want a clean sweep", err)
	}
	if len(issues.deleteCalls) == 0 {
		t.Error("Run did not sweep")
	}

	wanted := errors.New("disk on fire")
	issues.deleteErr = wanted
	if err := retention.Job().Run(ctx); !errors.Is(err, wanted) {
		t.Errorf("Run() = %v, want it to wrap %v", err, wanted)
	}
}

func mustKey(t *testing.T) domain.Key {
	t.Helper()
	key, err := domain.NewKey(testNow)
	if err != nil {
		t.Fatalf("minting key: %v", err)
	}
	return key
}

func TestAggregatesAreSweptOnTheirOwnMuchLongerClock(t *testing.T) {
	retention, projects, issues, stats, ctx := newRetentionWithStats(t)
	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}

	if _, err := retention.Sweep(ctx); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	// Two sweeps with two cutoffs, and the gap between them is the product
	// decision: the buckets outlive the payloads they were counted from, so
	// the dashboard still has a history after the events expire (ADR 010).
	if len(issues.deleteCalls) == 0 || len(stats.deleteCalls) == 0 {
		t.Fatalf("events swept %d times, aggregates %d — both must run",
			len(issues.deleteCalls), len(stats.deleteCalls))
	}
	wantEvents := testNow.Add(-engine.DefaultRetention()[engine.CategoryError])
	if got := issues.deleteCalls[0].cutoff; !got.Equal(wantEvents) {
		t.Errorf("event cutoff = %v, want %v", got, wantEvents)
	}
	wantAggregates := testNow.Add(-engine.DefaultRetention()[engine.CategoryAggregates])
	if got := stats.deleteCalls[0].cutoff; !got.Equal(wantAggregates) {
		t.Errorf("aggregate cutoff = %v, want %v", got, wantAggregates)
	}
	if !stats.deleteCalls[0].cutoff.Before(issues.deleteCalls[0].cutoff) {
		t.Error("the aggregates are swept no earlier than the events, so the history dies with the payloads")
	}
}

func TestEventsAtZeroDaysPurgeWhileTheBucketsStay(t *testing.T) {
	// The case the dashboard exists for, expressed as configuration: keep no
	// events at all, keep the counts. Zero used to mean "use the default",
	// which made it a setting somebody could write, read back and watch do
	// nothing (ADR 031, proposed).
	retention, projects, issues, stats, ctx := newRetentionWithStats(t)
	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	if err := projects.SetProjectConfig(ctx, 1, domain.ProjectConfig{
		RetentionDays: map[string]int{"error": 0},
	}); err != nil {
		t.Fatalf("configuring: %v", err)
	}

	if _, err := retention.Sweep(ctx); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	if got := issues.deleteCalls[0].cutoff; !got.Equal(testNow) {
		t.Errorf("cutoff = %v, want now (%v): zero days keeps nothing", got, testNow)
	}
	wantAggregates := testNow.Add(-engine.DefaultRetention()[engine.CategoryAggregates])
	if got := stats.deleteCalls[0].cutoff; !got.Equal(wantAggregates) {
		t.Errorf("aggregate cutoff = %v, want the aggregates' own %v", got, wantAggregates)
	}
}

func TestOmittingACategoryStillInheritsTheDefault(t *testing.T) {
	// The way back to the default is absence, not zero. This is the half of
	// the change that keeps it honest: with zero taken, omission has to work.
	retention, projects, issues, _, ctx := newRetentionWithStats(t)
	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	if err := projects.SetProjectConfig(ctx, 1, domain.ProjectConfig{
		RetentionDays: map[string]int{"transaction": 3},
	}); err != nil {
		t.Fatalf("configuring: %v", err)
	}

	if _, err := retention.Sweep(ctx); err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	want := testNow.Add(-engine.DefaultRetention()[engine.CategoryError])
	if got := issues.deleteCalls[0].cutoff; !got.Equal(want) {
		t.Errorf("cutoff = %v, want the engine default %v", got, want)
	}
}

func TestAFailingAggregateSweepIsReported(t *testing.T) {
	retention, projects, _, stats, ctx := newRetentionWithStats(t)
	if _, _, err := projects.Create(ctx, domain.Project{Name: "a", CreatedAt: testNow}, mustKey(t)); err != nil {
		t.Fatalf("creating project: %v", err)
	}
	stats.deleteErr = errors.New("disk")

	// Returned rather than swallowed: the scheduler counts it and the jobs
	// endpoint shows it, which is the only way anyone learns the buckets have
	// stopped being cleaned.
	if _, err := retention.Sweep(ctx); err == nil {
		t.Error("a failing aggregate sweep reported success")
	}
}
