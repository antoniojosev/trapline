package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var _ ports.StatsRepository = (*StatsRepository)(nil)

// DefaultTopIssues is how many issues a "loudest in this range" list returns
// when the caller does not say.
const DefaultTopIssues = 10

// MaxTopIssues bounds what a caller may ask for. The list exists to be read at
// a glance; past a certain length it is the issue list with extra steps.
const MaxTopIssues = 100

// StatsRepository reads the hourly aggregates.
//
// Every query here is a range scan of a bucket table's primary key or of one
// index built for it. None of them touches events, and none of them touches a
// payload — that is the point of the tables existing (ADR 001, ADR 010).
type StatsRepository struct {
	db *DB
}

// NewStatsRepository wires the repository to an open database.
func NewStatsRepository(db *DB) *StatsRepository {
	return &StatsRepository{db: db}
}

// ProjectSeries returns a project's hourly counts, split by level.
func (r *StatsRepository) ProjectSeries(
	ctx context.Context, projectID int64, window domain.Range,
) ([]ports.LevelBucket, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT hour, level, count
		FROM project_hourly
		WHERE project_id = ? AND hour >= ? AND hour <= ?
		ORDER BY hour, level`,
		projectID, window.FirstBucket(), window.LastBucket())
	if err != nil {
		return nil, fmt.Errorf("reading the hourly series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var series []ports.LevelBucket
	for rows.Next() {
		var (
			bucket ports.LevelBucket
			level  string
		)
		if err := rows.Scan(&bucket.Hour, &level, &bucket.Count); err != nil {
			return nil, fmt.Errorf("scanning an hourly bucket: %w", err)
		}
		bucket.Level = domain.Level(level)
		series = append(series, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the hourly series: %w", err)
	}
	return series, nil
}

// TopIssues returns the issues with the most events in the range.
//
// The join reaches into issues for the columns a list needs to render, but the
// ranking comes entirely from the buckets: the sum is over issue_hourly, which
// is what makes "loudest in the last six hours" a different question from
// "loudest ever" and lets both be answered without reading an event.
func (r *StatsRepository) TopIssues(
	ctx context.Context, projectID int64, window domain.Range, limit int,
) ([]ports.IssueCount, error) {
	if limit <= 0 {
		limit = DefaultTopIssues
	}
	if limit > MaxTopIssues {
		limit = MaxTopIssues
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT i.id, i.project_id, i.fingerprint, i.grouping_version, i.title, i.culprit,
		       i.level, i.status, i.first_seen, i.last_seen, i.times, i.last_release,
		       SUM(h.count) AS ranked
		FROM issue_hourly h
		JOIN issues i ON i.id = h.issue_id
		WHERE h.project_id = ? AND h.hour >= ? AND h.hour <= ?
		GROUP BY h.issue_id
		ORDER BY ranked DESC, i.last_seen DESC, i.id DESC
		LIMIT ?`,
		projectID, window.FirstBucket(), window.LastBucket(), limit)
	if err != nil {
		return nil, fmt.Errorf("reading the top issues: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var top []ports.IssueCount
	for rows.Next() {
		var counted ports.IssueCount
		// The issue columns are scanned by the same function the listing uses,
		// with the range total appended. Two copies of that column list is how
		// one of them silently stops matching the other.
		issue, err := scanIssue(withExtra{row: rows, extra: []any{&counted.Count}})
		if err != nil {
			return nil, fmt.Errorf("scanning a top issue: %w", err)
		}
		counted.Issue = issue
		top = append(top, counted)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the top issues: %w", err)
	}
	return top, nil
}

// Breakdown totals a project's events by release or environment.
func (r *StatsRepository) Breakdown(
	ctx context.Context, projectID int64, dimension domain.Dimension,
	window domain.Range, limit int,
) ([]ports.DimensionCount, error) {
	if limit <= 0 {
		limit = DefaultTopIssues
	}
	if limit > MaxTopIssues {
		limit = MaxTopIssues
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT value, SUM(count) AS ranked
		FROM project_hourly_dims
		WHERE project_id = ? AND dim = ? AND hour >= ? AND hour <= ?
		GROUP BY value
		ORDER BY ranked DESC, value
		LIMIT ?`,
		projectID, string(dimension), window.FirstBucket(), window.LastBucket(), limit)
	if err != nil {
		return nil, fmt.Errorf("reading the %s breakdown: %w", dimension, err)
	}
	defer func() { _ = rows.Close() }()

	var breakdown []ports.DimensionCount
	for rows.Next() {
		var counted ports.DimensionCount
		if err := rows.Scan(&counted.Value, &counted.Count); err != nil {
			return nil, fmt.Errorf("scanning a %s bucket: %w", dimension, err)
		}
		breakdown = append(breakdown, counted)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the %s breakdown: %w", dimension, err)
	}
	return breakdown, nil
}

// IssueSeries returns one issue's hourly counts.
func (r *StatsRepository) IssueSeries(
	ctx context.Context, projectID, issueID int64, window domain.Range,
) ([]ports.HourCount, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT hour, count
		FROM issue_hourly
		WHERE issue_id = ? AND project_id = ? AND hour >= ? AND hour <= ?
		ORDER BY hour`,
		issueID, projectID, window.FirstBucket(), window.LastBucket())
	if err != nil {
		return nil, fmt.Errorf("reading the issue's series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var series []ports.HourCount
	for rows.Next() {
		var bucket ports.HourCount
		if err := rows.Scan(&bucket.Hour, &bucket.Count); err != nil {
			return nil, fmt.Errorf("scanning an issue bucket: %w", err)
		}
		series = append(series, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the issue's series: %w", err)
	}
	return series, nil
}

// IssuesSeries returns the hourly counts of several issues at once.
//
// One statement with an IN list rather than a loop of IssueSeries calls: the
// index is (issue_id, project_id, hour) either way, but a page of twenty-five
// rows becomes one round trip instead of twenty-five, and the caller renders
// the whole page from a single answer rather than watching it fill in.
func (r *StatsRepository) IssuesSeries(
	ctx context.Context, projectID int64, issueIDs []int64, window domain.Range,
) (map[int64][]ports.HourCount, error) {
	if len(issueIDs) == 0 {
		return map[int64][]ports.HourCount{}, nil
	}
	if len(issueIDs) > domain.MaxSparklineIssues {
		// Bounded here as well as at the transport, for the same reason
		// TopIssues is: a repository that trusts its caller to have checked
		// is one caller away from an unbounded IN clause.
		issueIDs = issueIDs[:domain.MaxSparklineIssues]
	}

	// Placeholders rather than an interpolated list: these are ints from a
	// query string, and the moment one of them is pasted into SQL the fact
	// that they parsed as ints today is the only thing standing between this
	// and an injection.
	placeholders := make([]byte, 0, len(issueIDs)*2)
	arguments := make([]any, 0, len(issueIDs)+3)
	arguments = append(arguments, projectID, window.FirstBucket(), window.LastBucket())
	for index, issueID := range issueIDs {
		if index > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		arguments = append(arguments, issueID)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT issue_id, hour, count
		FROM issue_hourly
		WHERE project_id = ? AND hour >= ? AND hour <= ?
		  AND issue_id IN (`+string(placeholders)+`)
		ORDER BY issue_id, hour`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("reading the issues' series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	series := make(map[int64][]ports.HourCount, len(issueIDs))
	for rows.Next() {
		var (
			issueID int64
			bucket  ports.HourCount
		)
		if err := rows.Scan(&issueID, &bucket.Hour, &bucket.Count); err != nil {
			return nil, fmt.Errorf("scanning a bulk issue bucket: %w", err)
		}
		series[issueID] = append(series[issueID], bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the issues' series: %w", err)
	}
	return series, nil
}

// aggregateTables are every table a retention sweep of the aggregates has to
// clear. Named once so a fourth rollup cannot be added and then quietly kept
// forever because nobody remembered this list.
var aggregateTables = [...]string{"issue_hourly", "project_hourly", "project_hourly_dims"}

// DeleteAggregatesBefore removes buckets older than cutoff, in bounded
// batches.
//
// Bounded for the same reason the event sweep is: the store has one writer,
// shared with ingestion, and a sweep that holds a long transaction stalls the
// endpoint the product exists to keep answering.
//
// The cutoff is rounded down to its bucket, so an hour that is only partly
// expired survives whole. Deleting it would erase the events from the newer
// part of the hour along with the older, and a chart that loses its most
// recent bar at every sweep is worse than one that keeps an extra hour.
func (r *StatsRepository) DeleteAggregatesBefore(
	ctx context.Context, projectID int64, cutoff time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	bucket := domain.HourBucket(cutoff)

	var deleted int64
	for _, table := range aggregateTables {
		// The table name is one of three constants chosen here, never
		// anything a caller supplied; every value stays a bound parameter.
		result, err := r.db.ExecContext(ctx, `
			DELETE FROM `+table+`
			WHERE rowid IN (
				SELECT rowid FROM `+table+`
				WHERE project_id = ? AND hour < ?
				LIMIT ?
			)`, projectID, bucket, limit)
		if err != nil {
			return deleted, fmt.Errorf("deleting expired %s buckets: %w", table, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("reading the %s delete result: %w", table, err)
		}
		deleted += affected
	}
	return deleted, nil
}

// withExtra scans a row into the destinations a shared scan function expects,
// followed by the extra columns this query added.
//
// It exists so a query that selects the issue columns plus an aggregate can
// reuse scanIssue instead of restating twelve column bindings that would then
// have to be kept in step with it by hand.
type withExtra struct {
	row   scanner
	extra []any
}

func (w withExtra) Scan(dest ...any) error {
	//nolint:wrapcheck // pass-through to the underlying row: the caller wraps.
	return w.row.Scan(append(dest, w.extra...)...)
}
