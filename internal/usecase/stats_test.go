package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// seriesStats is a stats store that answers with whatever a test hands it, so
// the assertions here are about the shaping — filling the quiet hours, folding
// the levels — and not about SQL, which is the adapter's own tests' job.
type seriesStats struct {
	*fakeStats
	buckets []ports.LevelBucket
	issue   []ports.HourCount
	err     error
}

func (s *seriesStats) ProjectSeries(context.Context, int64, domain.Range) ([]ports.LevelBucket, error) {
	return s.buckets, s.err
}

func (s *seriesStats) IssueSeries(context.Context, int64, int64, domain.Range) ([]ports.HourCount, error) {
	return s.issue, s.err
}

func newStats(t *testing.T) (*Stats, *seriesStats, *fakeIssues) {
	t.Helper()
	store := &seriesStats{fakeStats: newFakeStats()}
	issues := newFakeIssues()
	return NewStats(store, issues, fixedClock{now: testNow}), store, issues
}

func TestProjectSeriesFillsTheQuietHours(t *testing.T) {
	stats, store, _ := newStats(t)
	window, err := domain.NewRange(testNow.Add(-2*time.Hour), testNow, testNow)
	if err != nil {
		t.Fatalf("building the range: %v", err)
	}
	store.buckets = []ports.LevelBucket{
		{Hour: domain.HourBucket(testNow), Level: domain.LevelError, Count: 4},
		{Hour: domain.HourBucket(testNow), Level: domain.LevelWarning, Count: 1},
	}

	series, err := stats.ProjectSeries(context.Background(), 1, window)
	if err != nil {
		t.Fatalf("reading the series: %v", err)
	}

	// Three points for three hours, two of them empty. A chart drawn from only
	// the hours that had events draws a quiet night as a straight line between
	// two spikes.
	if len(series.Points) != 3 {
		t.Fatalf("points = %d, want 3 (one per hour in the range)", len(series.Points))
	}
	if series.Points[0].Count != 0 || series.Points[0].ByLevel == nil {
		t.Errorf("a quiet hour = %+v, want a zero with an empty level map", series.Points[0])
	}
	last := series.Points[2]
	if last.Count != 5 || last.ByLevel["error"] != 4 || last.ByLevel["warning"] != 1 {
		t.Errorf("last hour = %+v, want 5 split 4/1", last)
	}
	if series.Total != 5 {
		t.Errorf("total = %d, want 5", series.Total)
	}
}

func TestProjectSeriesIgnoresBucketsOutsideTheRange(t *testing.T) {
	stats, store, _ := newStats(t)
	window, err := domain.NewRange(testNow, testNow, testNow)
	if err != nil {
		t.Fatalf("building the range: %v", err)
	}
	// A row the store should never have returned. Appending it would make the
	// series a different length than the axis expects, which is worse than
	// dropping it.
	store.buckets = []ports.LevelBucket{
		{Hour: "1999-01-01T00", Level: domain.LevelError, Count: 9},
	}

	series, err := stats.ProjectSeries(context.Background(), 1, window)
	if err != nil {
		t.Fatalf("reading the series: %v", err)
	}
	if len(series.Points) != 1 || series.Total != 0 {
		t.Errorf("series = %+v, want one empty hour", series)
	}
}

func TestProjectSeriesPropagatesAFailure(t *testing.T) {
	stats, store, _ := newStats(t)
	store.err = errors.New("disk")
	window, _ := domain.NewRange(testNow, testNow, testNow)

	if _, err := stats.ProjectSeries(context.Background(), 1, window); err == nil {
		t.Error("a failing store produced a chart")
	}
}

func TestIssueSeriesRefusesAnIssueThatIsNotThere(t *testing.T) {
	stats, _, _ := newStats(t)

	// A chart of zeroes for an id nobody owns would let a typo impersonate a
	// quiet issue.
	_, err := stats.IssueSeries(context.Background(), 1, 404, domain.Window24h)
	if !errors.Is(err, domain.ErrIssueNotFound) {
		t.Errorf("error = %v, want ErrIssueNotFound", err)
	}
}

func TestIssueSeriesCoversTheWholeWindow(t *testing.T) {
	stats, store, issues := newStats(t)
	recorded, err := issues.RecordEvent(context.Background(), &ports.RecordEventInput{
		ProjectID:   1,
		Fingerprint: "abc",
		Observation: domain.Observation{At: testNow, Level: domain.LevelError, Title: "boom"},
	})
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	store.issue = []ports.HourCount{{Hour: domain.HourBucket(testNow), Count: 7}}

	series, err := stats.IssueSeries(context.Background(), 1, recorded.Issue.ID, domain.Window14d)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if got := len(series.Points); got != 14*24 {
		t.Errorf("points = %d, want %d", got, 14*24)
	}
	if series.Total != 7 {
		t.Errorf("total = %d, want 7", series.Total)
	}
	if last := series.Points[len(series.Points)-1]; last.Count != 7 {
		t.Errorf("the current hour = %+v, want the 7 events", last)
	}
}

func TestRangeReadsEverySpellingAndRefusesTheRest(t *testing.T) {
	stats, _, _ := newStats(t)

	for _, raw := range []string{"2026-08-22", "2026-08-22T10", "2026-08-22T10:00:00Z"} {
		if _, err := stats.Range(raw, ""); err != nil {
			t.Errorf("Range(%q): %v", raw, err)
		}
	}
	if _, err := stats.Range("last tuesday", ""); !errors.Is(err, domain.ErrInvalidRange) {
		t.Errorf("error = %v, want ErrInvalidRange", err)
	}
	if _, err := stats.Range("", "not a time"); !errors.Is(err, domain.ErrInvalidRange) {
		t.Errorf("error = %v, want ErrInvalidRange", err)
	}

	// Neither end named is the commonest call there is: a dashboard opening.
	window, err := stats.Range("", "")
	if err != nil {
		t.Fatalf("Range with no ends: %v", err)
	}
	if window.Hours() != domain.DefaultRangeHours {
		t.Errorf("hours = %d, want the default %d", window.Hours(), domain.DefaultRangeHours)
	}
}

func TestTopIssuesAndBreakdownReachTheStore(t *testing.T) {
	// Both are pass-throughs today. Covered anyway, because "pass-through" is
	// a property that has to keep being true: the day one of them grows a
	// filter or a default, this is what says so.
	stats, store, _ := newStats(t)
	window, err := domain.NewRange(testNow, testNow, testNow)
	if err != nil {
		t.Fatalf("building the range: %v", err)
	}

	if _, err := stats.TopIssues(context.Background(), 1, window, 5); err != nil {
		t.Errorf("TopIssues: %v", err)
	}
	if _, err := stats.Breakdown(context.Background(), 1, domain.DimensionRelease, window, 5); err != nil {
		t.Errorf("Breakdown: %v", err)
	}

	store.deleteErr = errors.New("disk")
	if _, err := store.DeleteAggregatesBefore(context.Background(), 1, testNow, 10); err == nil {
		t.Error("a failing delete reported success")
	}
}

// bulkStats answers a bulk sparkline query with whatever the test set up,
// because what is under test here is the shaping — an entry per issue asked
// for, one count per hour of the window, in order — and not SQL.
type bulkStats struct {
	*fakeStats
	buckets map[int64][]ports.HourCount
	err     error
}

func (s *bulkStats) IssuesSeries(
	_ context.Context, _ int64, _ []int64, _ domain.Range,
) (map[int64][]ports.HourCount, error) {
	return s.buckets, s.err
}

func TestSparklinesHaveARowPerIssueAskedForAndAnHourPerBucket(t *testing.T) {
	window := domain.Window24h.Range(testNow)
	hours := window.Buckets()
	store := &bulkStats{
		fakeStats: newFakeStats(),
		buckets: map[int64][]ports.HourCount{
			7: {{Hour: hours[0], Count: 2}, {Hour: hours[len(hours)-1], Count: 5}},
		},
	}
	stats := NewStats(store, newFakeIssues(), fixedClock{now: testNow})

	// Issue 9 has nothing, and 404 does not exist. Both still get a row: a
	// client draws one sparkline per row it is showing, and a missing entry
	// would shift every chart onto the wrong row.
	span, sparklines, err := stats.IssuesSeries(
		context.Background(), 1, []int64{7, 9, 404}, domain.Window24h)
	if err != nil {
		t.Fatalf("reading the sparklines: %v", err)
	}
	if span.Hours() != 24 {
		t.Errorf("range covers %d hours, want 24", span.Hours())
	}
	if len(sparklines) != 3 {
		t.Fatalf("returned %d rows for three ids", len(sparklines))
	}

	for index, want := range map[int]struct {
		issueID int64
		total   int64
	}{0: {7, 7}, 1: {9, 0}, 2: {404, 0}} {
		got := sparklines[index]
		if got.IssueID != want.issueID {
			t.Errorf("row %d is issue %d, want %d — the order is what lines a "+
				"sparkline up with its row", index, got.IssueID, want.issueID)
		}
		if got.Total != want.total {
			t.Errorf("issue %d totalled %d, want %d", got.IssueID, got.Total, want.total)
		}
		if len(got.Counts) != len(hours) {
			t.Errorf("issue %d has %d counts for %d hours",
				got.IssueID, len(got.Counts), len(hours))
		}
	}

	// And the counts land on the hours they belong to, not merely add up.
	if first := sparklines[0].Counts[0]; first != 2 {
		t.Errorf("the first hour holds %d, want 2", first)
	}
	if last := sparklines[0].Counts[len(hours)-1]; last != 5 {
		t.Errorf("the last hour holds %d, want 5", last)
	}
}

// A bucket outside the window it was queried for can only mean the range and
// the query disagree. Dropped rather than appended, so the series stays
// exactly as long as the axis expects.
func TestSparklinesIgnoreABucketOutsideTheirWindow(t *testing.T) {
	store := &bulkStats{
		fakeStats: newFakeStats(),
		buckets: map[int64][]ports.HourCount{
			7: {{Hour: "1999-01-01T00", Count: 9}},
		},
	}
	stats := NewStats(store, newFakeIssues(), fixedClock{now: testNow})

	_, sparklines, err := stats.IssuesSeries(context.Background(), 1, []int64{7}, domain.Window24h)
	if err != nil {
		t.Fatalf("reading the sparklines: %v", err)
	}
	if sparklines[0].Total != 0 {
		t.Errorf("total = %d, want 0: an hour outside the axis is not on it",
			sparklines[0].Total)
	}
	if len(sparklines[0].Counts) != 24 {
		t.Errorf("%d counts, want the window's 24", len(sparklines[0].Counts))
	}
}
