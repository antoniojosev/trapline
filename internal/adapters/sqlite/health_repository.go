package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var _ ports.HealthRepository = (*HealthRepository)(nil)

// categorySession is the engine's category name, spelled here so this file
// does not import the engine for one string — the same reason the trace
// repository spells its own.
const categorySession = "session"

// defaultReleaseLimit bounds a release listing. Fifty is more releases than a
// team ships in a day, and past it the answer is an export rather than a page.
const defaultReleaseLimit = 50

// HealthRepository stores the session counters behind release health.
//
// It is the only writer of `session_hourly`, and everything it writes is an
// addition: the in-memory window (ADR 008) settles sessions into counters and
// hands over a flush's worth at a time, and each flush folds into whatever is
// already in the row. Nothing here ever sees a session id — by the time a
// number reaches this file, which sessions produced it has already been
// forgotten, which is exactly the property the ADR is protecting.
//
// Named for health rather than sessions because SessionRepository in this
// package already means panel logins.
type HealthRepository struct {
	db *DB
}

// NewHealthRepository wires the repository to an open database.
func NewHealthRepository(db *DB) *HealthRepository {
	return &HealthRepository{db: db}
}

// AddSessionCounts folds a flush's buckets into the table.
func (r *HealthRepository) AddSessionCounts(ctx context.Context, buckets []ports.SessionBucket) error {
	if len(buckets) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning the session flush: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The INSERT ... SELECT ... WHERE EXISTS is not decoration. A project
	// deleted between a session arriving and this flush running would trip the
	// foreign key, and a failing flush retries the same buckets a minute later
	// and fails again — forever, taking every other project's counters down
	// with it. Asking whether the project is still there turns that into a
	// bucket that quietly is not written, which is the correct outcome: the
	// project it described no longer exists.
	statement, err := tx.PrepareContext(ctx, `
		INSERT INTO session_hourly
			(project_id, release, environment, hour, started, errored, crashed, abnormal)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM projects WHERE id = ?)
		ON CONFLICT (project_id, release, environment, hour) DO UPDATE SET
			started  = started  + excluded.started,
			errored  = errored  + excluded.errored,
			crashed  = crashed  + excluded.crashed,
			abnormal = abnormal + excluded.abnormal`)
	if err != nil {
		return fmt.Errorf("preparing the session flush: %w", err)
	}
	defer func() { _ = statement.Close() }()

	for _, bucket := range buckets {
		if bucket.Counts.Empty() {
			continue
		}
		_, err := statement.ExecContext(ctx,
			bucket.Key.ProjectID, bucket.Key.Release, bucket.Key.Environment, bucket.Key.Hour,
			bucket.Counts.Started, bucket.Counts.Errored,
			bucket.Counts.Crashed, bucket.Counts.Abnormal,
			bucket.Key.ProjectID)
		if err != nil {
			return fmt.Errorf("writing a session bucket: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the session flush: %w", err)
	}
	return nil
}

// ReleaseSeries reads one release's hours, summed across environments.
func (r *HealthRepository) ReleaseSeries(
	ctx context.Context, projectID int64, release string, from, to string,
) ([]ports.SessionHour, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT hour,
		       SUM(started), SUM(errored), SUM(crashed), SUM(abnormal)
		FROM session_hourly
		WHERE project_id = ? AND release = ? AND hour >= ? AND hour <= ?
		GROUP BY hour
		ORDER BY hour`, projectID, release, from, to)
	if err != nil {
		return nil, fmt.Errorf("reading the release's sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var series []ports.SessionHour
	for rows.Next() {
		var hour ports.SessionHour
		if err := rows.Scan(&hour.Hour, &hour.Counts.Started, &hour.Counts.Errored,
			&hour.Counts.Crashed, &hour.Counts.Abnormal); err != nil {
			return nil, fmt.Errorf("scanning a session hour: %w", err)
		}
		series = append(series, hour)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the release's sessions: %w", err)
	}
	return series, nil
}

// ReleaseTotals reads every release of a project over a range, busiest first.
func (r *HealthRepository) ReleaseTotals(
	ctx context.Context, projectID int64, from, to string, limit int,
) ([]ports.ReleaseSessions, error) {
	if limit <= 0 {
		limit = defaultReleaseLimit
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT release,
		       SUM(started), SUM(errored), SUM(crashed), SUM(abnormal),
		       MIN(hour), MAX(hour)
		FROM session_hourly
		WHERE project_id = ? AND hour >= ? AND hour <= ?
		GROUP BY release
		ORDER BY SUM(started) DESC, release
		LIMIT ?`, projectID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("reading the project's releases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var totals []ports.ReleaseSessions
	for rows.Next() {
		var release ports.ReleaseSessions
		if err := rows.Scan(&release.Release,
			&release.Counts.Started, &release.Counts.Errored,
			&release.Counts.Crashed, &release.Counts.Abnormal,
			&release.FirstHour, &release.LastHour); err != nil {
			return nil, fmt.Errorf("scanning a release's sessions: %w", err)
		}
		totals = append(totals, release)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating the project's releases: %w", err)
	}
	return totals, nil
}

// HasSessions reports whether any project accepts sessions.
//
// It reads the configuration rather than the table, for the reason HasTracing
// does: a project switched on a minute ago has no rows yet, and a flush job
// that waited for the first row before existing would have nowhere to put the
// window that produced it.
func (r *HealthRepository) HasSessions(ctx context.Context) (bool, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT config FROM projects")
	if err != nil {
		return false, fmt.Errorf("reading project configuration: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return false, fmt.Errorf("scanning project configuration: %w", err)
		}
		if domain.DecodeProjectConfig(raw.String).CategoryEnabled(categorySession) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterating project configuration: %w", err)
	}
	return false, nil
}

// PruneSessionsBefore deletes one project's expired session buckets.
func (r *HealthRepository) PruneSessionsBefore(
	ctx context.Context, projectID int64, cutoff time.Time, limit int,
) (int64, error) {
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM session_hourly
		WHERE rowid IN (
			SELECT rowid FROM session_hourly WHERE project_id = ? AND hour < ? LIMIT ?
		)`, projectID, domain.HourBucket(cutoff), batchSize(limit))
	if err != nil {
		return 0, fmt.Errorf("deleting expired session buckets: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reading delete result: %w", err)
	}
	return deleted, nil
}
