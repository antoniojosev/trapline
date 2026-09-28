package usecase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Stats is the dashboard side of the product: how much is breaking, when, in
// which release and where.
//
// Every answer comes from the hourly buckets written during ingest (ADR 010),
// which is what lets these questions be asked over months without any of them
// reading an event.
type Stats struct {
	stats  ports.StatsRepository
	issues ports.IssueRepository
	clock  ports.Clock
}

// NewStats wires the use case.
func NewStats(stats ports.StatsRepository, issues ports.IssueRepository, clock ports.Clock) *Stats {
	return &Stats{stats: stats, issues: issues, clock: clock}
}

// HourlyPoint is one hour of a project's series.
//
// ByLevel carries every level that occurred in that hour, and Count is their
// sum rather than a number the client has to add up — the total is what a
// chart's tooltip shows first and what the axis is scaled by.
type HourlyPoint struct {
	Hour    string           `json:"hour"`
	Count   int64            `json:"count"`
	ByLevel map[string]int64 `json:"by_level"`
}

// Series is a project's events per hour over a range.
type Series struct {
	Range  domain.Range
	Points []HourlyPoint
	Total  int64
}

// ProjectSeries returns one point per hour in the range, empty hours
// included.
//
// The empty hours are filled here rather than in the store, because only this
// layer knows what was asked for: a store that invented rows would be
// inventing them for a range it was told about anyway, and a client that had
// to fill them would each do it slightly differently (ADR 006 — four clients,
// one answer).
func (s *Stats) ProjectSeries(ctx context.Context, projectID int64, window domain.Range) (Series, error) {
	buckets, err := s.stats.ProjectSeries(ctx, projectID, window)
	if err != nil {
		return Series{}, err
	}

	index := make(map[string]int, window.Hours())
	points := make([]HourlyPoint, 0, window.Hours())
	for _, hour := range window.Buckets() {
		index[hour] = len(points)
		points = append(points, HourlyPoint{Hour: hour, ByLevel: map[string]int64{}})
	}

	series := Series{Range: window, Points: points}
	for _, bucket := range buckets {
		position, inRange := index[bucket.Hour]
		if !inRange {
			// A bucket outside the range it was queried for can only mean the
			// range and the query disagree. Skipped rather than appended, so
			// the series stays exactly as long as the axis expects.
			continue
		}
		point := &series.Points[position]
		point.Count += bucket.Count
		point.ByLevel[string(bucket.Level)] += bucket.Count
		series.Total += bucket.Count
	}
	return series, nil
}

// TopIssues returns the issues with the most events in the range.
func (s *Stats) TopIssues(
	ctx context.Context, projectID int64, window domain.Range, limit int,
) ([]ports.IssueCount, error) {
	return s.stats.TopIssues(ctx, projectID, window, limit)
}

// Breakdown totals a project's events by release or environment.
func (s *Stats) Breakdown(
	ctx context.Context, projectID int64, dimension domain.Dimension, window domain.Range, limit int,
) ([]ports.DimensionCount, error) {
	return s.stats.Breakdown(ctx, projectID, dimension, window, limit)
}

// IssueSeries is one issue's own chart over a named window.
type IssueSeries struct {
	Issue  domain.Issue
	Window domain.Window
	Range  domain.Range
	Points []ports.HourCount
	Total  int64
}

// IssueSeries returns one issue's events per hour.
//
// The issue is read first so an id that belongs to another project — or to
// nobody — answers "not found" rather than a chart of zeroes. An empty chart
// is what a quiet issue looks like, and a typo must not be able to impersonate
// one.
func (s *Stats) IssueSeries(
	ctx context.Context, projectID, issueID int64, window domain.Window,
) (IssueSeries, error) {
	issue, err := s.issues.FindByID(ctx, projectID, issueID)
	if err != nil {
		return IssueSeries{}, err
	}

	span := window.Range(s.clock.Now())
	buckets, err := s.stats.IssueSeries(ctx, projectID, issueID, span)
	if err != nil {
		return IssueSeries{}, err
	}

	counts := make(map[string]int64, len(buckets))
	for _, bucket := range buckets {
		counts[bucket.Hour] += bucket.Count
	}

	result := IssueSeries{Issue: issue, Window: window, Range: span}
	result.Points = make([]ports.HourCount, 0, span.Hours())
	for _, hour := range span.Buckets() {
		count := counts[hour]
		result.Total += count
		result.Points = append(result.Points, ports.HourCount{Hour: hour, Count: count})
	}
	return result, nil
}

// IssueSparkline is one issue's counts over a window, in the shape a chart
// consumes: one number per hour, empty hours included, in order.
//
// Counts rather than {hour, count} pairs, because every hour in the window is
// present and in order — repeating the hour on each of them would triple the
// size of a response whose whole purpose is to be small enough to send
// twenty-five of at once. The window's bounds are on the response once.
type IssueSparkline struct {
	IssueID int64   `json:"issue_id"`
	Total   int64   `json:"total"`
	Counts  []int64 `json:"counts"`
}

// IssuesSeries returns a sparkline for each of several issues.
//
// The issues are not checked for existence first, unlike IssueSeries. The
// difference is what the caller is doing: IssueSeries answers a question about
// one issue somebody navigated to, where a wrong id has to say so, and this
// draws a decoration beside rows that were just read from the same project.
// An id that no longer exists gets an empty sparkline, which is what it is.
func (s *Stats) IssuesSeries(
	ctx context.Context, projectID int64, issueIDs []int64, window domain.Window,
) (domain.Range, []IssueSparkline, error) {
	span := window.Range(s.clock.Now())
	buckets, err := s.stats.IssuesSeries(ctx, projectID, issueIDs, span)
	if err != nil {
		return domain.Range{}, nil, err
	}

	hours := span.Buckets()
	positions := make(map[string]int, len(hours))
	for index, hour := range hours {
		positions[hour] = index
	}

	sparklines := make([]IssueSparkline, 0, len(issueIDs))
	for _, issueID := range issueIDs {
		sparkline := IssueSparkline{IssueID: issueID, Counts: make([]int64, len(hours))}
		for _, bucket := range buckets[issueID] {
			position, inRange := positions[bucket.Hour]
			if !inRange {
				continue
			}
			sparkline.Counts[position] += bucket.Count
			sparkline.Total += bucket.Count
		}
		sparklines = append(sparklines, sparkline)
	}
	return span, sparklines, nil
}

// Range resolves the range a caller asked for, filling in the ends it left
// out. It lives here so the four clients cannot each invent their own default
// window.
func (s *Stats) Range(from, to string) (domain.Range, error) {
	parsedFrom, err := parseRangeEnd(from)
	if err != nil {
		return domain.Range{}, err
	}
	parsedTo, err := parseRangeEnd(to)
	if err != nil {
		return domain.Range{}, err
	}
	return domain.NewRange(parsedFrom, parsedTo, s.clock.Now())
}

// parseRangeEnd reads one end of a range from what a caller typed.
//
// Three spellings, in the order somebody is likely to reach for them: a full
// RFC 3339 instant, which is what a program sends; an hour, which is the
// resolution the aggregates actually have; and a bare date, which is what a
// person types. An empty string is not an error — it means "you choose", and
// the range constructor does.
func parseRangeEnd(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, domain.HourLayout, time.DateOnly} {
		if parsed, err := time.ParseInLocation(layout, trimmed, time.UTC); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"%w: %q is not a time; use 2006-01-02, 2006-01-02T15 or an RFC 3339 instant",
		domain.ErrInvalidRange, trimmed)
}
