package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Against a real SQLite file, never a mock (ADR 009). What is interesting here
// is an upsert that adds rather than replaces, and a mock would happily
// pretend that worked.

func bucketFor(projectID int64, release, environment, hour string, counts domain.SessionCounts) ports.SessionBucket {
	return ports.SessionBucket{
		Key: domain.HealthKey{
			ProjectID: projectID, Release: release, Environment: environment, Hour: hour,
		},
		Counts: counts,
	}
}

// TestSessionCountsAddRatherThanReplace is the invariant the whole table rests
// on: every writer is folding one flush's verdicts into whatever is already
// there, and a store that replaced would silently discard the flush before it.
func TestSessionCountsAddRatherThanReplace(t *testing.T) {
	db := openTemp(t)
	repo := NewHealthRepository(db)
	projectID := newProjectFor(t, db)
	ctx := context.Background()

	first := bucketFor(projectID, "shop@1.4.2", "production", "2026-08-29T14",
		domain.SessionCounts{Started: 60, Errored: 2, Crashed: 3})
	second := bucketFor(projectID, "shop@1.4.2", "production", "2026-08-29T14",
		domain.SessionCounts{Started: 40, Crashed: 2, Abnormal: 1})

	for _, bucket := range []ports.SessionBucket{first, second} {
		if err := repo.AddSessionCounts(ctx, []ports.SessionBucket{bucket}); err != nil {
			t.Fatalf("writing: %v", err)
		}
	}

	hours, err := repo.ReleaseSeries(ctx, projectID, "shop@1.4.2", "2026-08-29T00", "2026-08-29T23")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(hours) != 1 {
		t.Fatalf("got %d hours, want 1", len(hours))
	}
	want := domain.SessionCounts{Started: 100, Errored: 2, Crashed: 5, Abnormal: 1}
	if hours[0].Counts != want {
		t.Fatalf("got %+v, want %+v", hours[0].Counts, want)
	}
	if rate, known := hours[0].Counts.CrashFreeRate(); !known || rate != 0.95 {
		t.Fatalf("crash-free rate %v (known %v), want 0.95", rate, known)
	}
}

// TestASeriesSumsEnvironmentsAndKeepsHoursApart: "how is 1.4.2 doing" is a
// question about a release, not about one of its deployments, but an hour is
// the resolution the chart draws.
func TestASeriesSumsEnvironmentsAndKeepsHoursApart(t *testing.T) {
	db := openTemp(t)
	repo := NewHealthRepository(db)
	projectID := newProjectFor(t, db)
	ctx := context.Background()

	buckets := []ports.SessionBucket{
		bucketFor(projectID, "shop@1.4.2", "production", "2026-08-29T13", domain.SessionCounts{Started: 10}),
		bucketFor(projectID, "shop@1.4.2", "staging", "2026-08-29T13", domain.SessionCounts{Started: 5, Crashed: 1}),
		bucketFor(projectID, "shop@1.4.2", "production", "2026-08-29T14", domain.SessionCounts{Started: 20}),
		bucketFor(projectID, "shop@1.4.1", "production", "2026-08-29T14", domain.SessionCounts{Started: 99}),
	}
	if err := repo.AddSessionCounts(ctx, buckets); err != nil {
		t.Fatalf("writing: %v", err)
	}

	hours, err := repo.ReleaseSeries(ctx, projectID, "shop@1.4.2", "2026-08-29T13", "2026-08-29T14")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(hours) != 2 {
		t.Fatalf("got %d hours, want 2", len(hours))
	}
	if hours[0].Hour != "2026-08-29T13" || hours[1].Hour != "2026-08-29T14" {
		t.Fatalf("hours out of order: %+v", hours)
	}
	if hours[0].Counts != (domain.SessionCounts{Started: 15, Crashed: 1}) {
		t.Fatalf("the two environments were not summed: %+v", hours[0].Counts)
	}
	if hours[1].Counts.Started != 20 {
		t.Fatalf("another release leaked into the series: %+v", hours[1].Counts)
	}
}

// TestARangeExcludesWhatIsOutsideIt: the buckets are fixed-width text, so a
// range is a lexicographic comparison and an off-by-one here would be an hour
// silently missing from every chart (ADR 033).
func TestARangeExcludesWhatIsOutsideIt(t *testing.T) {
	db := openTemp(t)
	repo := NewHealthRepository(db)
	projectID := newProjectFor(t, db)
	ctx := context.Background()

	for _, hour := range []string{"2026-08-29T12", "2026-08-29T13", "2026-08-29T14", "2026-08-29T15"} {
		bucket := bucketFor(projectID, "r@1", "production", hour, domain.SessionCounts{Started: 1})
		if err := repo.AddSessionCounts(ctx, []ports.SessionBucket{bucket}); err != nil {
			t.Fatalf("writing: %v", err)
		}
	}

	hours, err := repo.ReleaseSeries(ctx, projectID, "r@1", "2026-08-29T13", "2026-08-29T14")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(hours) != 2 {
		t.Fatalf("got %d hours, want the two inside the closed range", len(hours))
	}
}

// TestReleaseTotalsRankTheBusiestFirst answers the question somebody opens the
// page with: which of these is the bad one.
func TestReleaseTotalsRankTheBusiestFirst(t *testing.T) {
	db := openTemp(t)
	repo := NewHealthRepository(db)
	projectID := newProjectFor(t, db)
	ctx := context.Background()

	buckets := []ports.SessionBucket{
		bucketFor(projectID, "quiet@0.9", "production", "2026-08-29T10", domain.SessionCounts{Started: 3}),
		bucketFor(projectID, "busy@1.0", "production", "2026-08-29T13", domain.SessionCounts{Started: 100, Crashed: 5}),
		bucketFor(projectID, "busy@1.0", "production", "2026-08-29T14", domain.SessionCounts{Started: 50}),
	}
	if err := repo.AddSessionCounts(ctx, buckets); err != nil {
		t.Fatalf("writing: %v", err)
	}

	totals, err := repo.ReleaseTotals(ctx, projectID, "2026-08-29T00", "2026-08-29T23", 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("got %d releases, want 2", len(totals))
	}
	if totals[0].Release != "busy@1.0" || totals[0].Counts.Started != 150 {
		t.Fatalf("ranking wrong: %+v", totals)
	}
	if totals[0].FirstHour != "2026-08-29T13" || totals[0].LastHour != "2026-08-29T14" {
		t.Fatalf("the ends of the release's own range are wrong: %+v", totals[0])
	}

	limited, err := repo.ReleaseTotals(ctx, projectID, "2026-08-29T00", "2026-08-29T23", 1)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(limited) != 1 || limited[0].Release != "busy@1.0" {
		t.Fatalf("the limit did not keep the busiest: %+v", limited)
	}
}

// TestAFlushForADeletedProjectIsSkippedNotFatal: without this, a project
// deleted between a session arriving and the flush running would fail the same
// batch every minute forever, taking every other project's counters with it.
func TestAFlushForADeletedProjectIsSkippedNotFatal(t *testing.T) {
	db := openTemp(t)
	repo := NewHealthRepository(db)
	projectID := newProjectFor(t, db)
	ctx := context.Background()

	buckets := []ports.SessionBucket{
		bucketFor(projectID+9999, "gone@1", "production", "2026-08-29T14", domain.SessionCounts{Started: 7}),
		bucketFor(projectID, "here@1", "production", "2026-08-29T14", domain.SessionCounts{Started: 3}),
		bucketFor(projectID, "empty@1", "production", "2026-08-29T14", domain.SessionCounts{}),
	}
	if err := repo.AddSessionCounts(ctx, buckets); err != nil {
		t.Fatalf("the whole flush failed because of one dead project: %v", err)
	}

	totals, err := repo.ReleaseTotals(ctx, projectID, "2026-08-29T00", "2026-08-29T23", 0)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(totals) != 1 || totals[0].Release != "here@1" {
		t.Fatalf("got %+v, want only the live project's release", totals)
	}
}

func TestAnEmptyFlushTouchesNothing(t *testing.T) {
	db := openTemp(t)
	repo := NewHealthRepository(db)
	if err := repo.AddSessionCounts(context.Background(), nil); err != nil {
		t.Fatalf("an empty flush failed: %v", err)
	}
}

// TestHasSessionsAsksTheConfigurationNotTheTable: a project switched on a
// minute ago has no rows yet, and a flush job that waited for the first row
// before existing would have nowhere to put the window that produced it.
func TestHasSessionsAsksTheConfigurationNotTheTable(t *testing.T) {
	db := openTemp(t)
	repo := NewHealthRepository(db)
	projects := NewProjectRepository(db)
	projectID := newProjectFor(t, db)
	ctx := context.Background()

	wanted, err := repo.HasSessions(ctx)
	if err != nil {
		t.Fatalf("asking: %v", err)
	}
	if wanted {
		t.Fatal("sessions are off by default and were reported on")
	}

	config, err := projects.ProjectConfig(ctx, projectID)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	config.EnabledCategories = []string{"error", "session"}
	if err := projects.SetProjectConfig(ctx, projectID, config); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	if wanted, err := repo.HasSessions(ctx); err != nil || !wanted {
		t.Fatalf("HasSessions = %v (err %v) with a project accepting sessions", wanted, err)
	}
}

// TestPruneSessionsDeletesOnlyWhatExpired keeps release health from being the
// one table that grows forever.
func TestPruneSessionsDeletesOnlyWhatExpired(t *testing.T) {
	db := openTemp(t)
	repo := NewHealthRepository(db)
	projectID := newProjectFor(t, db)
	ctx := context.Background()

	for _, hour := range []string{"2025-01-01T10", "2025-01-02T10", "2026-08-29T14"} {
		bucket := bucketFor(projectID, "r@1", "production", hour, domain.SessionCounts{Started: 1})
		if err := repo.AddSessionCounts(ctx, []ports.SessionBucket{bucket}); err != nil {
			t.Fatalf("writing: %v", err)
		}
	}

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	deleted, err := repo.PruneSessionsBefore(ctx, projectID, cutoff, 0)
	if err != nil {
		t.Fatalf("pruning: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted %d buckets, want 2", deleted)
	}

	hours, err := repo.ReleaseSeries(ctx, projectID, "r@1", "2000-01-01T00", "2030-01-01T00")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(hours) != 1 || hours[0].Hour != "2026-08-29T14" {
		t.Fatalf("the wrong buckets survived: %+v", hours)
	}

	// Batched, like every sweep in this product: one long delete transaction
	// stalls the endpoint the product exists to keep answering.
	if deleted, err := repo.PruneSessionsBefore(ctx, projectID, cutoff, 1); err != nil || deleted != 0 {
		t.Fatalf("a second pass deleted %d (err %v)", deleted, err)
	}
}
