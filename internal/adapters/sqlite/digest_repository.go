package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var _ ports.DigestRepository = (*DigestRepository)(nil)

// DigestRepository answers "what appeared" and "what came back" over a range.
//
// Both are the same query with a different date column and a different
// predicate, which is why they share everything below the two constants. The
// counts come from issue_hourly and the membership from an indexed column on
// issues; no query here reads an event, so a digest can still be written
// about a week whose payloads retention has already removed (ADR 001,
// ADR 010).
type DigestRepository struct {
	db *DB
}

// NewDigestRepository wires the repository to an open database.
func NewDigestRepository(db *DB) *DigestRepository {
	return &DigestRepository{db: db}
}

// MaxDigestIssues bounds a shortlist. The digest prints five; the ceiling is
// for a caller previewing one who asked for more.
const MaxDigestIssues = 50

// The two transitions a weekly report is about, as the column that records
// when each happened.
//
// first_seen is on every issue and never null. regressed_at is null for the
// overwhelming majority — most issues have never come back — and the index
// behind it is partial for exactly that reason (migration 0017).
const (
	firstSeenColumn   = "i.first_seen"
	regressedAtColumn = "i.regressed_at"
)

// NewIssues returns the issues first seen inside the window.
func (r *DigestRepository) NewIssues(
	ctx context.Context, projectID int64, window domain.Range, limit int,
) (ports.IssueCounts, error) {
	return r.transitions(ctx, projectID, window, limit, firstSeenColumn, "new issues")
}

// RegressedIssues returns the issues that reopened inside the window.
func (r *DigestRepository) RegressedIssues(
	ctx context.Context, projectID int64, window domain.Range, limit int,
) (ports.IssueCounts, error) {
	return r.transitions(ctx, projectID, window, limit, regressedAtColumn, "regressions")
}

// transitions is both questions.
//
// The window arrives as whole hours, because that is the resolution the
// buckets have and the resolution a digest is scheduled at. Its ends are
// converted to instants for the comparison against the timestamp column: the
// range is closed at both ends in buckets, so the last instant it includes is
// the final second of the last bucket, and using the bucket's start would
// silently drop an issue's first hour.
func (r *DigestRepository) transitions(
	ctx context.Context, projectID int64, window domain.Range, limit int, column, what string,
) (ports.IssueCounts, error) {
	if limit <= 0 {
		limit = DefaultTopIssues
	}
	if limit > MaxDigestIssues {
		limit = MaxDigestIssues
	}

	from := formatTime(window.From)
	// The instant the last included bucket ends, exclusive: the start of the
	// hour after it.
	until := formatTime(window.To.Add(time.Hour))

	var counts ports.IssueCounts
	// The total first, and as its own statement. It is a count over the
	// indexed column alone, so it neither joins nor groups — which is what
	// makes "47 new issues" cheap to say next to a list of five.
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM issues i
		WHERE i.project_id = ? AND `+column+` >= ? AND `+column+` < ?`,
		projectID, from, until).Scan(&counts.Total); err != nil {
		return ports.IssueCounts{}, fmt.Errorf("counting %s: %w", what, err)
	}
	if counts.Total == 0 {
		return counts, nil
	}

	// LEFT JOIN, not JOIN. An issue can be first seen inside the window and
	// have no bucket rows left for it — the aggregates have their own
	// retention (ADR 010) and a very old week is legitimately countless — and
	// an inner join would drop it from a list whose whole subject is that it
	// appeared. It shows with a count of zero, which is what is known.
	rows, err := r.db.QueryContext(ctx, `
		SELECT i.id, i.project_id, i.fingerprint, i.grouping_version, i.title, i.culprit,
		       i.level, i.status, i.first_seen, i.last_seen, i.times, i.last_release,
		       COALESCE(SUM(h.count), 0) AS ranked
		FROM issues i
		LEFT JOIN issue_hourly h
		       ON h.issue_id = i.id AND h.hour >= ? AND h.hour <= ?
		WHERE i.project_id = ? AND `+column+` >= ? AND `+column+` < ?
		GROUP BY i.id
		ORDER BY ranked DESC, i.last_seen DESC, i.id DESC
		LIMIT ?`,
		window.FirstBucket(), window.LastBucket(), projectID, from, until, limit)
	if err != nil {
		return ports.IssueCounts{}, fmt.Errorf("reading %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var counted ports.IssueCount
		// The same scanner the listing uses, with the range total appended.
		// Two copies of that column list is how one of them silently stops
		// matching the other.
		issue, err := scanIssue(withExtra{row: rows, extra: []any{&counted.Count}})
		if err != nil {
			return ports.IssueCounts{}, fmt.Errorf("scanning one of the %s: %w", what, err)
		}
		counted.Issue = issue
		counts.Issues = append(counts.Issues, counted)
	}
	if err := rows.Err(); err != nil {
		return ports.IssueCounts{}, fmt.Errorf("iterating %s: %w", what, err)
	}
	return counts, nil
}
