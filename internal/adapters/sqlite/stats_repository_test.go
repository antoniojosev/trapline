package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// newStatsRepos builds an issue repository and a stats repository over one
// database, because that is the pairing under test: one writes the buckets in
// the ingest transaction, the other reads them back.
func newStatsRepos(t *testing.T) (*IssueRepository, *StatsRepository, int64) {
	t.Helper()
	db := openTemp(t)
	project, _ := createProject(t, NewProjectRepository(db), "venekambio")
	return NewIssueRepository(db), NewStatsRepository(db), project.ID
}

// at builds an instant inside the fixed test day, so a test names an hour
// rather than doing arithmetic in the assertion.
func at(hour int) time.Time {
	return time.Date(2026, 8, 29, hour, 30, 0, 0, time.UTC)
}

func record(t *testing.T, repo *IssueRepository, projectID int64, fingerprint string, when time.Time, change func(*ports.RecordEventInput)) ports.RecordEventResult {
	t.Helper()
	input := recordInput(projectID, fingerprint, when)
	if change != nil {
		change(input)
	}
	result, err := repo.RecordEvent(context.Background(), input)
	if err != nil {
		t.Fatalf("recording: %v", err)
	}
	return result
}

func windowOver(t *testing.T, from, to int) domain.Range {
	t.Helper()
	window, err := domain.NewRange(at(from), at(to), at(to))
	if err != nil {
		t.Fatalf("building the range: %v", err)
	}
	return window
}

func TestIngestWritesTheHourlyBuckets(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	// Two hours, two levels, so the series has something to split.
	record(t, issues, projectID, "a", at(10), nil)
	record(t, issues, projectID, "a", at(10), nil)
	record(t, issues, projectID, "b", at(11), func(input *ports.RecordEventInput) {
		input.Observation.Level = domain.LevelWarning
	})

	series, err := stats.ProjectSeries(ctx, projectID, windowOver(t, 9, 12))
	if err != nil {
		t.Fatalf("reading the series: %v", err)
	}

	got := map[string]int64{}
	for _, bucket := range series {
		got[bucket.Hour+"/"+string(bucket.Level)] = bucket.Count
	}
	if got["2026-08-29T10/error"] != 2 {
		t.Errorf("the 10:00 error bucket = %d, want 2 (%+v)", got["2026-08-29T10/error"], series)
	}
	if got["2026-08-29T11/warning"] != 1 {
		t.Errorf("the 11:00 warning bucket = %d, want 1 (%+v)", got["2026-08-29T11/warning"], series)
	}
	if len(series) != 2 {
		t.Errorf("series has %d rows, want 2: an hour with no events must have no row", len(series))
	}
}

func TestBucketsFollowWhenTheEventHappenedNotWhenItArrived(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	// A mobile client that was offline: the crash was at 10, the request
	// arrived at 14. The chart has to show a spike at 10, or an outage looks
	// like it happened when the network came back.
	record(t, issues, projectID, "a", at(10), func(input *ports.RecordEventInput) {
		input.ReceivedAt = at(14)
	})

	series, err := stats.ProjectSeries(ctx, projectID, windowOver(t, 9, 15))
	if err != nil {
		t.Fatalf("reading the series: %v", err)
	}
	if len(series) != 1 || series[0].Hour != "2026-08-29T10" {
		t.Errorf("series = %+v, want a single bucket at 10:00", series)
	}
}

func TestTopIssuesRanksByTheRangeNotByLifetime(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	// "loud" has more events overall; "recent" has more inside the window.
	// A ranking that read issues.times would get this backwards, which is the
	// whole reason the buckets exist.
	for range 5 {
		record(t, issues, projectID, "loud", at(2), nil)
	}
	record(t, issues, projectID, "loud", at(11), nil)
	for range 3 {
		record(t, issues, projectID, "recent", at(11), nil)
	}

	top, err := stats.TopIssues(ctx, projectID, windowOver(t, 10, 12), 0)
	if err != nil {
		t.Fatalf("reading the top issues: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("top = %+v, want two issues", top)
	}
	if top[0].Issue.Fingerprint != "recent" || top[0].Count != 3 {
		t.Errorf("first = %q with %d, want recent with 3", top[0].Issue.Fingerprint, top[0].Count)
	}
	if top[1].Count != 1 {
		t.Errorf("second count = %d, want 1: only the event inside the range counts", top[1].Count)
	}
	if top[1].Issue.Times != 6 {
		t.Errorf("lifetime times = %d, want 6: the range total must not overwrite it", top[1].Issue.Times)
	}
}

func TestTopIssuesBoundsWhatACallerMayAskFor(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	record(t, issues, projectID, "a", at(10), nil)

	// Past the ceiling the list stops being something anyone reads at a
	// glance; the request is clamped rather than refused, because a dashboard
	// asking for too much should still draw.
	top, err := stats.TopIssues(context.Background(), projectID, windowOver(t, 9, 12), MaxTopIssues+50)
	if err != nil {
		t.Fatalf("reading the top issues: %v", err)
	}
	if len(top) != 1 {
		t.Errorf("top = %+v, want the one issue that exists", top)
	}
}

func TestBreakdownByReleaseAndEnvironment(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	record(t, issues, projectID, "a", at(10), nil) // v1.0.0 / production
	record(t, issues, projectID, "b", at(10), func(input *ports.RecordEventInput) {
		input.Observation.Release = "v1.1.0"
		input.Environment = "staging"
	})
	record(t, issues, projectID, "c", at(11), func(input *ports.RecordEventInput) {
		input.Observation.Release = "v1.1.0"
		input.Environment = "staging"
	})

	releases, err := stats.Breakdown(ctx, projectID, domain.DimensionRelease, windowOver(t, 9, 12), 0)
	if err != nil {
		t.Fatalf("reading the release breakdown: %v", err)
	}
	if len(releases) != 2 || releases[0].Value != "v1.1.0" || releases[0].Count != 2 {
		t.Errorf("releases = %+v, want v1.1.0 first with 2", releases)
	}

	environments, err := stats.Breakdown(ctx, projectID, domain.DimensionEnvironment, windowOver(t, 9, 12), 0)
	if err != nil {
		t.Fatalf("reading the environment breakdown: %v", err)
	}
	if len(environments) != 2 || environments[0].Value != "staging" {
		t.Errorf("environments = %+v, want staging first", environments)
	}
}

func TestAnEventWithNoReleaseWritesNoDimensionRow(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)

	record(t, issues, projectID, "a", at(10), func(input *ports.RecordEventInput) {
		input.Observation.Release = ""
		input.Environment = ""
	})

	for _, dimension := range domain.Dimensions() {
		values, err := stats.Breakdown(context.Background(), projectID, dimension, windowOver(t, 9, 12), 0)
		if err != nil {
			t.Fatalf("reading the %s breakdown: %v", dimension, err)
		}
		// An empty string as the biggest bucket tells nobody anything: the
		// absence is already the answer.
		if len(values) != 0 {
			t.Errorf("%s = %+v, want nothing", dimension, values)
		}
	}
}

func TestIssueSeriesIsScopedToItsProject(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	result := record(t, issues, projectID, "a", at(10), nil)
	record(t, issues, projectID, "a", at(10), nil)

	series, err := stats.IssueSeries(ctx, projectID, result.Issue.ID, windowOver(t, 9, 12))
	if err != nil {
		t.Fatalf("reading the issue series: %v", err)
	}
	if len(series) != 1 || series[0].Count != 2 {
		t.Errorf("series = %+v, want one bucket of 2", series)
	}

	// Another project's id must not be able to read these numbers, for the
	// same reason FindByID takes the project: a query that can cross the
	// boundary is one refactor from being an authorisation bug.
	other, err := stats.IssueSeries(ctx, projectID+1, result.Issue.ID, windowOver(t, 9, 12))
	if err != nil {
		t.Fatalf("reading another project's series: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("series = %+v, want nothing for a project the issue does not belong to", other)
	}
}

func TestRetentionKeepsTheBucketsAfterTheEventsAreGone(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	record(t, issues, projectID, "a", at(10), nil)
	record(t, issues, projectID, "a", at(11), nil)

	// Every event deleted: the window for events is over, which is the day
	// the dashboard matters most.
	deleted, err := issues.DeleteEventsBefore(ctx, projectID, at(23), 100)
	if err != nil {
		t.Fatalf("deleting events: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted %d events, want 2", deleted)
	}

	series, err := stats.ProjectSeries(ctx, projectID, windowOver(t, 9, 12))
	if err != nil {
		t.Fatalf("reading the series: %v", err)
	}
	if len(series) != 2 {
		t.Errorf("series = %+v, want the two buckets to have survived the sweep", series)
	}
}

func TestDeleteAggregatesBeforeKeepsThePartialHour(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	record(t, issues, projectID, "a", at(10), nil)
	record(t, issues, projectID, "a", at(11), nil)
	record(t, issues, projectID, "a", at(12), nil)

	// A cutoff halfway through 11:00. The 11:00 bucket holds events from both
	// sides of it, so deleting it would erase counts that are still inside the
	// window.
	deleted, err := stats.DeleteAggregatesBefore(ctx, projectID, at(11), 0)
	if err != nil {
		t.Fatalf("deleting aggregates: %v", err)
	}
	// One row in issue_hourly, one in project_hourly, two in the dimensions.
	if deleted != 4 {
		t.Errorf("deleted %d rows, want 4 (the 10:00 bucket in every table)", deleted)
	}

	series, err := stats.ProjectSeries(ctx, projectID, windowOver(t, 9, 13))
	if err != nil {
		t.Fatalf("reading the series: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("series = %+v, want 11:00 and 12:00", series)
	}
	if series[0].Hour != "2026-08-29T11" {
		t.Errorf("oldest surviving bucket = %q, want 11:00 kept whole", series[0].Hour)
	}
}

func TestDeleteAggregatesBeforeIsBatched(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	for hour := range 5 {
		record(t, issues, projectID, "a", at(hour), nil)
	}

	// A sweep on a live store must never hold one long transaction, so the
	// limit is a promise the caller loops against, not a hint.
	first, err := stats.DeleteAggregatesBefore(ctx, projectID, at(23), 2)
	if err != nil {
		t.Fatalf("deleting: %v", err)
	}
	// Two rows from each of the three tables.
	if first != 6 {
		t.Errorf("first batch deleted %d rows, want 6", first)
	}

	var total = first
	for range 20 {
		batch, err := stats.DeleteAggregatesBefore(ctx, projectID, at(23), 2)
		if err != nil {
			t.Fatalf("deleting: %v", err)
		}
		total += batch
		if batch == 0 {
			break
		}
	}
	if total != 20 {
		t.Errorf("deleted %d rows in total, want 20 (5 hours × (1 + 1 + 2) rows)", total)
	}
}

func TestAggregatesGoWithTheIssueTheyCount(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	result := record(t, issues, projectID, "a", at(10), nil)

	// Deleting an issue must not leave orphan buckets behind: they would be
	// counted by the project series forever, with nothing to click through to.
	if _, err := stats.db.ExecContext(ctx, "DELETE FROM issues WHERE id = ?", result.Issue.ID); err != nil {
		t.Fatalf("deleting the issue: %v", err)
	}
	series, err := stats.IssueSeries(ctx, projectID, result.Issue.ID, windowOver(t, 9, 12))
	if err != nil {
		t.Fatalf("reading the series: %v", err)
	}
	if len(series) != 0 {
		t.Errorf("series = %+v, want nothing: the buckets cascade with the issue", series)
	}
}

var _ ports.StatsRepository = (*StatsRepository)(nil)

func TestIssuesSeriesReadsAPageOfIssuesInOneQuery(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()

	loud := record(t, issues, projectID, "loud", at(10), nil)
	record(t, issues, projectID, "loud", at(10), nil)
	record(t, issues, projectID, "loud", at(11), nil)
	quiet := record(t, issues, projectID, "quiet", at(11), nil)

	series, err := stats.IssuesSeries(ctx, projectID,
		[]int64{loud.Issue.ID, quiet.Issue.ID}, windowOver(t, 9, 12))
	if err != nil {
		t.Fatalf("reading the bulk series: %v", err)
	}

	if got := len(series[loud.Issue.ID]); got != 2 {
		t.Fatalf("the loud issue has %d buckets, want two", got)
	}
	if series[loud.Issue.ID][0].Count != 2 || series[loud.Issue.ID][1].Count != 1 {
		t.Errorf("loud series = %+v, want 2 then 1 in hour order", series[loud.Issue.ID])
	}
	if got := len(series[quiet.Issue.ID]); got != 1 {
		t.Errorf("the quiet issue has %d buckets, want one", got)
	}
}

func TestIssuesSeriesCannotReachAcrossProjects(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()
	result := record(t, issues, projectID, "a", at(10), nil)

	series, err := stats.IssuesSeries(ctx, projectID+1, []int64{result.Issue.ID}, windowOver(t, 9, 12))
	if err != nil {
		t.Fatalf("reading another project's bulk series: %v", err)
	}
	if len(series) != 0 {
		t.Errorf("series = %+v, want nothing: naming somebody else's issue id must not "+
			"be a way to read their numbers", series)
	}
}

func TestIssuesSeriesBoundsWhatACallerMayAskFor(t *testing.T) {
	issues, stats, projectID := newStatsRepos(t)
	ctx := context.Background()
	record(t, issues, projectID, "a", at(10), nil)

	// The bound is applied here as well as at the transport, because the
	// alternative is a repository that trusts its caller to have checked.
	tooMany := make([]int64, domain.MaxSparklineIssues+50)
	for index := range tooMany {
		tooMany[index] = int64(index + 1)
	}
	if _, err := stats.IssuesSeries(ctx, projectID, tooMany, windowOver(t, 9, 12)); err != nil {
		t.Fatalf("reading an over-long list: %v", err)
	}

	// And an empty list is not an error, it is an empty answer: a listing
	// with no rows asks for no sparklines.
	series, err := stats.IssuesSeries(ctx, projectID, nil, windowOver(t, 9, 12))
	if err != nil {
		t.Fatalf("reading an empty list: %v", err)
	}
	if len(series) != 0 {
		t.Errorf("series = %+v, want nothing", series)
	}
}
