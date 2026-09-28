package domain

import (
	"fmt"
	"strings"
	"time"
)

// HourLayout is how an hourly bucket is written down: UTC, to the hour, in a
// form that sorts lexicographically in the same order it sorts
// chronologically (ADR 010).
//
// That property is the whole reason it is text and not an integer. A range
// query over buckets is then an ordinary index range scan on a TEXT column,
// and a row a human reads says what it means instead of being a number they
// have to convert.
const HourLayout = "2006-01-02T15"

// MaxRangeHours bounds how many buckets one query may ask for.
//
// It matches the aggregates keep-window, so the ceiling is "everything that
// exists" rather than an arbitrary cut: asking for more can only return
// empty buckets, and a response with a hundred thousand of those is a client
// that has to paginate a chart.
const MaxRangeHours = 400 * 24

// MaxSparklineIssues bounds how many issues one bulk series request may name.
//
// It matches the longest page the issue listing serves, because that is what
// the bulk request exists for — one row, one sparkline. Past it, the caller
// wants a report, and a report is what the breakdown endpoints are.
const MaxSparklineIssues = 100

// DefaultRangeHours is the window a stats query covers when the caller names
// neither end. A day, because "what is happening now" is what the dashboard
// is opened for.
const DefaultRangeHours = 24

// HourBucket is the bucket an instant belongs to.
func HourBucket(at time.Time) string {
	return at.UTC().Truncate(time.Hour).Format(HourLayout)
}

// ParseHourBucket reads a bucket back as the instant it starts at.
func ParseHourBucket(bucket string) (time.Time, error) {
	parsed, err := time.ParseInLocation(HourLayout, bucket, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %q is not an hour bucket", ErrInvalidRange, bucket)
	}
	return parsed, nil
}

// Range is a closed interval of whole hours, in UTC.
//
// Closed rather than half-open because both ends name a bucket that is
// included, and every consumer of this — a chart axis, a CLI table, a SQL
// BETWEEN — is easier to get right when "to" means the last bucket shown
// rather than the first one hidden.
type Range struct {
	From time.Time
	To   time.Time
}

// NewRange builds a range from two instants, truncating both to their hour.
//
// A zero From means "DefaultRangeHours before To"; a zero To means "now".
// Both being zero is the commonest call there is — a dashboard opening on the
// last day — so it is spelled as passing nothing rather than as a second
// constructor.
func NewRange(from, to, now time.Time) (Range, error) {
	if to.IsZero() {
		to = now
	}
	to = to.UTC().Truncate(time.Hour)
	if from.IsZero() {
		from = to.Add(-(DefaultRangeHours - 1) * time.Hour)
	}
	from = from.UTC().Truncate(time.Hour)

	if to.Before(from) {
		return Range{}, fmt.Errorf("%w: to (%s) is before from (%s)",
			ErrInvalidRange, HourBucket(to), HourBucket(from))
	}
	if hours := int(to.Sub(from)/time.Hour) + 1; hours > MaxRangeHours {
		return Range{}, fmt.Errorf("%w: %d hours requested, the most that exists is %d",
			ErrInvalidRange, hours, MaxRangeHours)
	}
	return Range{From: from, To: to}, nil
}

// Hours is how many buckets the range covers, both ends included.
func (r Range) Hours() int {
	if r.To.Before(r.From) {
		return 0
	}
	return int(r.To.Sub(r.From)/time.Hour) + 1
}

// Buckets lists every bucket in the range, including the ones with nothing in
// them.
//
// The empty ones are the point. A chart drawn from only the hours that had
// events draws a quiet night as a straight line between two spikes, which is
// the opposite of what happened.
func (r Range) Buckets() []string {
	count := r.Hours()
	if count <= 0 {
		return nil
	}
	buckets := make([]string, 0, count)
	for at := r.From; !at.After(r.To); at = at.Add(time.Hour) {
		buckets = append(buckets, HourBucket(at))
	}
	return buckets
}

// FirstBucket and LastBucket are the range's ends as stored.
func (r Range) FirstBucket() string { return HourBucket(r.From) }

// LastBucket is the final bucket the range includes.
func (r Range) LastBucket() string { return HourBucket(r.To) }

// Window is a named span a client can ask for without doing arithmetic:
// "24h", "14d". The set is small and closed on purpose — an open-ended
// duration parser invites "1000d", which is a range query nobody meant.
type Window string

// The windows an issue's own chart offers.
const (
	// Window24h is the incident view: one day, one bar per hour.
	Window24h Window = "24h"
	// Window14d is the trend view: a fortnight, which is long enough to show
	// whether a fix held.
	Window14d Window = "14d"
)

// ParseWindow resolves a named window, defaulting to a day when unnamed.
func ParseWindow(raw string) (Window, error) {
	switch Window(strings.TrimSpace(raw)) {
	case "", Window24h:
		return Window24h, nil
	case Window14d:
		return Window14d, nil
	default:
		return "", fmt.Errorf("%w: unknown range %q, expected 24h or 14d", ErrInvalidRange, raw)
	}
}

// Duration is how far back a window reaches.
func (w Window) Duration() time.Duration {
	if w == Window14d {
		return 14 * 24 * time.Hour
	}
	return 24 * time.Hour
}

// Range turns a window into the interval of buckets it covers.
func (w Window) Range(now time.Time) Range {
	to := now.UTC().Truncate(time.Hour)
	return Range{From: to.Add(-w.Duration() + time.Hour), To: to}
}

// Dimension is a facet a project's events can be broken down by.
//
// Two of them, and they are the two that answer "which deploy did this" and
// "is it only staging" — the questions asked in the first minute of an
// incident. They are stored as their own aggregate rather than as tags
// because they are the only tags every event carries, so the cardinality is
// bounded and the rollup is worth writing on the hot path (ADR 010).
type Dimension string

// The dimensions a breakdown can use.
const (
	// DimensionRelease groups by the release an event reported.
	DimensionRelease Dimension = "release"
	// DimensionEnvironment groups by the environment an event reported.
	DimensionEnvironment Dimension = "environment"
)

// Dimensions lists every known dimension, so a client can render the choice
// without hard-coding a list that goes stale.
func Dimensions() []Dimension { return []Dimension{DimensionRelease, DimensionEnvironment} }

// ParseDimension validates a requested breakdown.
func ParseDimension(raw string) (Dimension, error) {
	for _, known := range Dimensions() {
		if Dimension(raw) == known {
			return known, nil
		}
	}
	names := make([]string, 0, len(Dimensions()))
	for _, known := range Dimensions() {
		names = append(names, string(known))
	}
	return "", fmt.Errorf("%w: unknown dimension %q, expected one of %s",
		ErrInvalidRange, raw, strings.Join(names, ", "))
}
