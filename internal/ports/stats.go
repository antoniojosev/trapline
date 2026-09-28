package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// LevelBucket is one hour of one project, for one severity.
//
// Rows rather than a nested shape, because that is what the store returns and
// the use case is where the shaping belongs. An adapter that already knew the
// response layout would have to be changed every time the dashboard did.
type LevelBucket struct {
	Hour  string
	Level domain.Level
	Count int64
}

// HourCount is one hour of one issue.
type HourCount struct {
	Hour  string `json:"hour"`
	Count int64  `json:"count"`
}

// IssueCount is an issue and how many events it took in a range — which is a
// different number from Issue.Times, and the difference is the point: Times is
// the whole life of the issue, this is the window being looked at.
type IssueCount struct {
	Issue domain.Issue
	Count int64
}

// DimensionCount is one value of a breakdown dimension and its total.
type DimensionCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// StatsRepository reads the hourly aggregates written during ingest.
//
// Separate from IssueRepository even though both live in the same file on
// disk: nothing here is on the write path, and the two are used by different
// callers for different reasons. Keeping them apart is also what lets the
// retention sweep depend on the aggregates without acquiring the ability to
// record an event.
//
// Every method here reads buckets and nothing else. No query in this
// interface may touch events.payload, and none may derive a count with a
// GROUP BY over events: that is the rule the storage design rests on (ADR
// 001) and the reason these tables exist at all (ADR 010).
type StatsRepository interface {
	// ProjectSeries is a project's events per hour, split by level, for the
	// hours in the range that have any. Absent hours are absent, not zero —
	// filling the gaps is the use case's job, because only it knows the range
	// the caller asked for.
	ProjectSeries(ctx context.Context, projectID int64, window domain.Range) ([]LevelBucket, error)
	// TopIssues is the issues with the most events in the range, loudest
	// first.
	TopIssues(ctx context.Context, projectID int64, window domain.Range, limit int) ([]IssueCount, error)
	// Breakdown totals a project's events by release or environment.
	Breakdown(
		ctx context.Context, projectID int64, dimension domain.Dimension,
		window domain.Range, limit int,
	) ([]DimensionCount, error)
	// IssueSeries is one issue's events per hour. The project id is part of
	// the lookup rather than checked afterwards, for the same reason it is in
	// FindByID: a query that can return another project's numbers is one
	// refactor away from being an authorisation bug.
	IssueSeries(ctx context.Context, projectID, issueID int64, window domain.Range) ([]HourCount, error)
	// IssuesSeries is the same thing for a page of issues at once, keyed by
	// issue id. It exists because the alternative is what a list of twenty-five
	// rows with a sparkline each would otherwise do: twenty-five round trips
	// for twenty-five index scans that the same scan already covered.
	// An id belonging to another project simply has no rows, so this cannot
	// be used to read across a project boundary.
	IssuesSeries(
		ctx context.Context, projectID int64, issueIDs []int64, window domain.Range,
	) (map[int64][]HourCount, error)
	// DeleteAggregatesBefore removes buckets older than cutoff, at most limit
	// rows per call so a sweep never holds a long write transaction on a live
	// store. Aggregates have their own keep-window, far longer than the
	// events they summarise (ADR 010).
	DeleteAggregatesBefore(ctx context.Context, projectID int64, cutoff time.Time, limit int) (int64, error)
}
