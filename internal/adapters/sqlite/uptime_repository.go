package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// UptimeRepository stores monitors, their checks and the daily roll-up.
type UptimeRepository struct {
	db *DB

	// alerts is how a status change becomes a notification inside the same
	// transaction that recorded it. Optional: a repository assembled without
	// it records checks and notifies nobody, which is the truthful behaviour
	// for an installation with no alerting (ADR 035's rule, applied here).
	alerts *AlertRepository
	origin domain.Origin
}

// NewUptimeRepository wires the repository to an open database.
func NewUptimeRepository(db *DB) *UptimeRepository {
	return &UptimeRepository{db: db}
}

// WithAlerts makes a status change offer itself to the alert rules.
//
// The second collaboration between two adapters in this repository, and it is
// here for the same reason as the first (issue_repository.go): the
// notification and the state change it is about are written by one
// transaction and committed together. Routing it through a use case would
// mean two transactions and a window in which a monitor is down and nothing
// has been queued about it — and that window is exactly where a restart lands
// during an outage, which is the only moment this subsystem is judged on
// (ADR 015).
func (r *UptimeRepository) WithAlerts(alerts *AlertRepository, origin domain.Origin) *UptimeRepository {
	r.alerts, r.origin = alerts, origin
	return r
}

const uptimeMonitorColumns = `id, project_id, name, url, method, interval_s, timeout_s,
	expected_status_min, expected_status_max, expected_body_substring, follow_redirects,
	allow_private, public, enabled, status, consecutive_failures, last_checked_at,
	next_check_at, last_status_change_at, created_at`

// CreateMonitor stores a validated monitor.
func (r *UptimeRepository) CreateMonitor(
	ctx context.Context, monitor *domain.UptimeMonitor,
) (domain.UptimeMonitor, error) {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO uptime_monitors
			(project_id, name, url, method, interval_s, timeout_s,
			 expected_status_min, expected_status_max, expected_body_substring,
			 follow_redirects, allow_private, public, enabled, status,
			 consecutive_failures, last_checked_at, next_check_at,
			 last_status_change_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, ?, NULL, ?)`,
		monitor.ProjectID, monitor.Name, monitor.URL, monitor.Method,
		monitor.IntervalSeconds, monitor.TimeoutSeconds,
		monitor.ExpectedStatusMin, monitor.ExpectedStatusMax, monitor.ExpectedBodySubstring,
		boolToInt(monitor.FollowRedirects), boolToInt(monitor.AllowPrivate),
		boolToInt(monitor.Public), boolToInt(monitor.Enabled), string(monitor.Status),
		formatTime(monitor.NextCheckAt), formatTime(monitor.CreatedAt),
	)
	if err != nil {
		return domain.UptimeMonitor{}, fmt.Errorf("creating a monitor: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.UptimeMonitor{}, fmt.Errorf("reading the monitor id: %w", err)
	}
	monitor.ID = id
	return *monitor, nil
}

// ListMonitors returns the monitors of one project, or all of them.
func (r *UptimeRepository) ListMonitors(
	ctx context.Context, projectID *int64,
) ([]domain.UptimeMonitor, error) {
	query := "SELECT " + uptimeMonitorColumns + " FROM uptime_monitors"
	var args []any
	if projectID != nil {
		query += " WHERE project_id = ?"
		args = append(args, *projectID)
	}
	query += " ORDER BY id"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing monitors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	monitors := []domain.UptimeMonitor{}
	for rows.Next() {
		monitor, err := scanUptimeMonitor(rows)
		if err != nil {
			return nil, err
		}
		monitors = append(monitors, monitor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading monitors: %w", err)
	}
	return monitors, nil
}

// FindMonitor returns one monitor.
func (r *UptimeRepository) FindMonitor(ctx context.Context, id int64) (domain.UptimeMonitor, error) {
	return findMonitor(ctx, r.db, id)
}

func findMonitor(ctx context.Context, q queryer, id int64) (domain.UptimeMonitor, error) {
	monitor, err := scanUptimeMonitor(q.QueryRowContext(ctx,
		"SELECT "+uptimeMonitorColumns+" FROM uptime_monitors WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.UptimeMonitor{}, domain.ErrMonitorNotFound
	}
	return monitor, err
}

func scanUptimeMonitor(row scanner) (domain.UptimeMonitor, error) {
	var (
		monitor                                        domain.UptimeMonitor
		follow, allowPrivate, public, enabled          int64
		status, nextCheck, createdAt                   string
		lastChecked, lastStatusChange                  sql.NullString
		expectedStatusMin, expectedStatusMax, interval int64
		timeout                                        int64
		failures                                       int64
	)
	if err := row.Scan(
		&monitor.ID, &monitor.ProjectID, &monitor.Name, &monitor.URL, &monitor.Method,
		&interval, &timeout, &expectedStatusMin, &expectedStatusMax,
		&monitor.ExpectedBodySubstring, &follow, &allowPrivate, &public, &enabled,
		&status, &failures, &lastChecked, &nextCheck, &lastStatusChange, &createdAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.UptimeMonitor{}, err //nolint:wrapcheck // recognised by the caller.
		}
		return domain.UptimeMonitor{}, fmt.Errorf("reading a monitor: %w", err)
	}

	monitor.IntervalSeconds = int(interval)
	monitor.TimeoutSeconds = int(timeout)
	monitor.ExpectedStatusMin = int(expectedStatusMin)
	monitor.ExpectedStatusMax = int(expectedStatusMax)
	monitor.FollowRedirects = follow != 0
	monitor.AllowPrivate = allowPrivate != 0
	monitor.Public = public != 0
	monitor.Enabled = enabled != 0
	monitor.ConsecutiveFailures = int(failures)
	monitor.Status = domain.UptimeStatus(status)

	var err error
	if monitor.NextCheckAt, err = parseTime(nextCheck); err != nil {
		return domain.UptimeMonitor{}, err
	}
	if monitor.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.UptimeMonitor{}, err
	}
	if monitor.LastCheckedAt, err = parseNullableTime(lastChecked); err != nil {
		return domain.UptimeMonitor{}, err
	}
	if monitor.LastStatusChangeAt, err = parseNullableTime(lastStatusChange); err != nil {
		return domain.UptimeMonitor{}, err
	}
	return monitor, nil
}

// DeleteMonitor removes a monitor. Its results and aggregates go with it
// through ON DELETE CASCADE.
func (r *UptimeRepository) DeleteMonitor(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, "DELETE FROM uptime_monitors WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("deleting monitor %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting deleted monitors: %w", err)
	}
	if affected == 0 {
		return domain.ErrMonitorNotFound
	}
	return nil
}

// SetMonitorEnabled switches a monitor on or off, keeping its history.
//
// Switching on also makes it due immediately: a monitor somebody has just
// re-enabled and that then sits idle for an interval looks broken, and the
// stored next_check_at may be days in the past or — for one switched off
// mid-outage — describe a schedule nobody is on any more.
func (r *UptimeRepository) SetMonitorEnabled(
	ctx context.Context, id int64, enabled bool,
) (domain.UptimeMonitor, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.UptimeMonitor{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	monitor, err := findMonitor(ctx, tx, id)
	if err != nil {
		return domain.UptimeMonitor{}, err
	}
	next := monitor.NextCheckAt
	if enabled && !monitor.Enabled {
		next = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE uptime_monitors SET enabled = ?, next_check_at = ? WHERE id = ?",
		boolToInt(enabled), formatTime(next), id); err != nil {
		return domain.UptimeMonitor{}, fmt.Errorf("switching monitor %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return domain.UptimeMonitor{}, fmt.Errorf("committing the switch: %w", err)
	}

	monitor.Enabled = enabled
	monitor.NextCheckAt = next
	return monitor, nil
}

// HasEnabledMonitors reports whether anything is being checked.
//
// This is what the scheduler asks before the uptime job exists at all: with no
// monitor enabled there is nothing to check, so the job is not a goroutine
// that wakes up and goes back to sleep, it is absent (ADR 005, ADR 014).
func (r *UptimeRepository) HasEnabledMonitors(ctx context.Context) (bool, error) {
	var exists int
	err := r.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM uptime_monitors WHERE enabled = 1)").Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("looking for enabled monitors: %w", err)
	}
	return exists == 1, nil
}

// DueMonitors leases the monitors whose next check has come.
//
// The lease is next_check_at itself, pushed one interval forward as the rows
// are read. That means a pass that dies mid-check holds nothing — the monitor
// simply becomes due again on schedule — and no "in flight" column is needed,
// which would be a column a crash could leave set forever. It is the same
// mechanism the outbox uses, for the same reason.
func (r *UptimeRepository) DueMonitors(
	ctx context.Context, now time.Time, limit int,
) ([]domain.UptimeMonitor, error) {
	if limit <= 0 {
		limit = 50
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT `+uptimeMonitorColumns+`
		  FROM uptime_monitors
		 WHERE enabled = 1 AND next_check_at <= ?
		 ORDER BY next_check_at, id
		 LIMIT ?`, formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("reading due monitors: %w", err)
	}

	var due []domain.UptimeMonitor
	for rows.Next() {
		monitor, err := scanUptimeMonitor(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		due = append(due, monitor)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("reading due monitors: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing the due monitors query: %w", err)
	}

	for index := range due {
		monitor := &due[index]
		if _, err := tx.ExecContext(ctx,
			"UPDATE uptime_monitors SET next_check_at = ? WHERE id = ?",
			formatTime(now.Add(monitor.Interval())), monitor.ID); err != nil {
			return nil, fmt.Errorf("leasing monitor %d: %w", monitor.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing the lease: %w", err)
	}
	return due, nil
}

// RecordResult writes one check and everything that follows from it.
//
// One transaction: the result row, the day's aggregate, the monitor's new
// state and the notifications the transition produced. The alternative —
// write the result, then notify — has a window in which a service is recorded
// as down and nothing has been queued about it, and a process that dies in
// that window stays quiet about an outage forever (ADR 015).
func (r *UptimeRepository) RecordResult(
	ctx context.Context,
	before, after *domain.UptimeMonitor,
	result domain.CheckResult,
	transition domain.UptimeTransition,
) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO uptime_results (monitor_id, checked_at, ok, status_code, latency_ms, error)
		VALUES (?, ?, ?, ?, ?, ?)`,
		before.ID, formatTime(result.At), boolToInt(result.OK),
		result.StatusCode, result.LatencyMS, truncateError(result.Error),
	); err != nil {
		return 0, fmt.Errorf("recording a check: %w", err)
	}

	// The aggregate is written by the same statement that wrote the result,
	// not by a nightly roll-up job. A job would be a second thing to run, a
	// second thing to fail, and a gap in the chart for whatever it missed —
	// and this way the ninety-day view is correct the moment the check
	// happens (ADR 010's argument, applied to a much smaller table).
	failure := 0
	if !result.OK {
		failure = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO uptime_daily (monitor_id, day, checks, failures, latency_sum)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT (monitor_id, day) DO UPDATE SET
			checks      = checks + 1,
			failures    = failures + excluded.failures,
			latency_sum = latency_sum + excluded.latency_sum`,
		before.ID, domain.UptimeDayKey(result.At), failure, result.LatencyMS,
	); err != nil {
		return 0, fmt.Errorf("rolling up the day: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE uptime_monitors
		   SET status = ?, consecutive_failures = ?, last_checked_at = ?,
		       next_check_at = ?, last_status_change_at = ?
		 WHERE id = ?`,
		string(after.Status), after.ConsecutiveFailures,
		nullableTime(after.LastCheckedAt), formatTime(after.NextCheckAt),
		nullableTime(after.LastStatusChangeAt), before.ID,
	); err != nil {
		return 0, fmt.Errorf("updating monitor %d: %w", before.ID, err)
	}

	queued := 0
	if r.alerts != nil {
		if event, ok := after.AlertEventFor(transition, result); ok {
			queued, err = r.alerts.enqueueTx(ctx, tx, &event,
				r.origin.MonitorURL(after.ProjectID, domain.MonitorKindUptime, after.ID))
			if err != nil {
				return 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing a check: %w", err)
	}
	return queued, nil
}

// Results reads a monitor's recent checks, newest first.
func (r *UptimeRepository) Results(
	ctx context.Context, monitorID int64, limit int,
) ([]domain.CheckResult, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT checked_at, ok, status_code, latency_ms, error
		  FROM uptime_results
		 WHERE monitor_id = ?
		 ORDER BY checked_at DESC, id DESC
		 LIMIT ?`, monitorID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	results := []domain.CheckResult{}
	for rows.Next() {
		var (
			result           domain.CheckResult
			checkedAt        string
			ok               int64
			status, latency  int64
			failureExplained string
		)
		if err := rows.Scan(&checkedAt, &ok, &status, &latency, &failureExplained); err != nil {
			return nil, fmt.Errorf("reading a result: %w", err)
		}
		result.OK = ok != 0
		result.StatusCode = int(status)
		result.LatencyMS = int(latency)
		result.Error = failureExplained
		if result.At, err = parseTime(checkedAt); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading results: %w", err)
	}
	return results, nil
}

// DailyUptime reads the roll-up for a range of days, oldest first.
//
// Absent days are absent rather than filled with zeros. A day nothing ran on
// is a fact about this server, not about the target, and the caller — a status
// page — is the one that knows how to draw it (domain.UptimeDay.Uptime says
// what the number means).
func (r *UptimeRepository) DailyUptime(
	ctx context.Context, monitorID int64, from, to time.Time,
) ([]domain.UptimeDay, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT day, checks, failures, latency_sum
		  FROM uptime_daily
		 WHERE monitor_id = ? AND day >= ? AND day <= ?
		 ORDER BY day`,
		monitorID, domain.UptimeDayKey(from), domain.UptimeDayKey(to))
	if err != nil {
		return nil, fmt.Errorf("reading the daily roll-up: %w", err)
	}
	defer func() { _ = rows.Close() }()

	days := []domain.UptimeDay{}
	for rows.Next() {
		var (
			day  domain.UptimeDay
			date string
		)
		if err := rows.Scan(&date, &day.Checks, &day.Failures, &day.LatencySum); err != nil {
			return nil, fmt.Errorf("reading a day: %w", err)
		}
		parsed, err := time.Parse("2006-01-02", date)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a day: %w", ErrSchema, date, err)
		}
		day.Day = parsed.UTC()
		days = append(days, day)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the daily roll-up: %w", err)
	}
	return days, nil
}

// PruneResults deletes checks older than cutoff, a bounded batch at a time.
func (r *UptimeRepository) PruneResults(
	ctx context.Context, cutoff time.Time, limit int,
) (int64, error) {
	return r.prune(ctx, `
		DELETE FROM uptime_results
		 WHERE id IN (SELECT id FROM uptime_results WHERE checked_at < ? LIMIT ?)`,
		formatTime(cutoff), limit, "results")
}

// PruneDaily deletes aggregate rows older than cutoff.
func (r *UptimeRepository) PruneDaily(
	ctx context.Context, cutoff time.Time, limit int,
) (int64, error) {
	return r.prune(ctx, `
		DELETE FROM uptime_daily
		 WHERE rowid IN (SELECT rowid FROM uptime_daily WHERE day < ? LIMIT ?)`,
		domain.UptimeDayKey(cutoff), limit, "daily aggregates")
}

// prune runs one bounded delete. Bounded because this store has a single
// writer shared with ingestion, and one long delete transaction stalls the
// endpoint the product exists to keep answering.
func (r *UptimeRepository) prune(
	ctx context.Context, query string, cutoff any, limit int, what string,
) (int64, error) {
	if limit <= 0 {
		limit = 500
	}
	result, err := r.db.ExecContext(ctx, query, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("pruning uptime %s: %w", what, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting pruned uptime %s: %w", what, err)
	}
	return deleted, nil
}
