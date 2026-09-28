package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Compile-time proof that this adapter satisfies the port.
var _ ports.CronRepository = (*CronRepository)(nil)

// DefaultCheckInPageSize is how many check-ins a history returns when the
// caller does not say.
const DefaultCheckInPageSize = 50

// MaxCheckInPageSize bounds what a caller may ask for.
const MaxCheckInPageSize = 500

// monitorColumns is the monitor row, in one place so the four readers of it
// cannot drift.
const monitorColumns = `id, project_id, slug, ping_key, schedule_type, schedule, timezone,
	checkin_margin_s, max_runtime_s, status, last_checkin_at, next_expected_at, enabled, created_at`

// CronRepository stores cron monitors and their check-ins.
type CronRepository struct {
	db *DB
	// alerts is who turns "this monitor is missed" into rows in the outbox,
	// inside this repository's own transaction. Optional: an assembly without
	// alerting watches monitors exactly as it did before alerting existed.
	alerts *AlertRepository
	// origin is the installation's public address, needed to put a link to
	// the monitor in the notification.
	origin domain.Origin
}

// NewCronRepository wires the repository to an open database.
func NewCronRepository(db *DB) *CronRepository {
	return &CronRepository{db: db}
}

// WithAlerts makes state changes offer themselves to the alert rules.
//
// The same seam IssueRepository has, for the same reason: the notification and
// the fact it is about are written by one transaction, so there is no window
// in which a monitor is missed and nobody has been queued to hear about it
// (ADR 015).
//
// It differs from the ingest path in one deliberate way. There, a failure to
// queue a notification is logged and the event is stored anyway — throwing
// away an event to preserve an alert would be the wrong trade, because the
// events are the product. Here the whole transaction fails: the watcher sweeps
// again in thirty seconds and the status it would have moved has not moved, so
// the decision is simply retaken. A monitor whose status silently advanced
// past the one notification anybody wanted is the failure this feature exists
// to avoid.
func (r *CronRepository) WithAlerts(alerts *AlertRepository, origin domain.Origin) *CronRepository {
	r.alerts, r.origin = alerts, origin
	return r
}

// CreateMonitor stores a new monitor.
func (r *CronRepository) CreateMonitor(
	ctx context.Context, monitor *domain.CronMonitor,
) (domain.CronMonitor, error) {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO cron_monitors
			(project_id, slug, ping_key, schedule_type, schedule, timezone,
			 checkin_margin_s, max_runtime_s, status, last_checkin_at, next_expected_at,
			 enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		monitor.ProjectID, monitor.Slug, monitor.PingKey,
		string(monitor.Schedule.Type), monitor.Schedule.String(), monitor.Timezone,
		int64(monitor.CheckinMargin.Seconds()), int64(monitor.MaxRuntime.Seconds()),
		string(monitor.Status), nullableTime(monitor.LastCheckinAt), nullableTime(monitor.NextExpectedAt),
		boolToInt(monitor.Enabled), formatTime(monitor.CreatedAt),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.CronMonitor{}, fmt.Errorf("%w: project %d already has a monitor called %q",
				domain.ErrInvalidMonitor, monitor.ProjectID, monitor.Slug)
		}
		return domain.CronMonitor{}, fmt.Errorf("creating a monitor: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.CronMonitor{}, fmt.Errorf("reading the new monitor's id: %w", err)
	}
	stored := *monitor
	stored.ID = id
	return stored, nil
}

// UpdateMonitor rewrites a monitor's configuration and its derived state.
//
// The ping key is deliberately absent from the statement: rotating a
// credential and editing a schedule are different operations, and doing both
// at once would silently break every crontab line carrying the old key.
func (r *CronRepository) UpdateMonitor(
	ctx context.Context, monitor *domain.CronMonitor,
) (domain.CronMonitor, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE cron_monitors SET
			schedule_type = ?, schedule = ?, timezone = ?,
			checkin_margin_s = ?, max_runtime_s = ?,
			status = ?, last_checkin_at = ?, next_expected_at = ?, enabled = ?
		WHERE id = ?`,
		string(monitor.Schedule.Type), monitor.Schedule.String(), monitor.Timezone,
		int64(monitor.CheckinMargin.Seconds()), int64(monitor.MaxRuntime.Seconds()),
		string(monitor.Status), nullableTime(monitor.LastCheckinAt), nullableTime(monitor.NextExpectedAt),
		boolToInt(monitor.Enabled), monitor.ID,
	)
	if err != nil {
		return domain.CronMonitor{}, fmt.Errorf("updating monitor %d: %w", monitor.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return domain.CronMonitor{}, fmt.Errorf("updating monitor %d: %w", monitor.ID, err)
	}
	if affected == 0 {
		return domain.CronMonitor{}, domain.ErrMonitorNotFound
	}
	return *monitor, nil
}

// ListMonitors returns a project's monitors, oldest first.
func (r *CronRepository) ListMonitors(
	ctx context.Context, projectID int64,
) ([]domain.CronMonitor, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+monitorColumns+" FROM cron_monitors WHERE project_id = ? ORDER BY id", projectID)
	if err != nil {
		return nil, fmt.Errorf("listing monitors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	monitors := make([]domain.CronMonitor, 0, 8)
	for rows.Next() {
		monitor, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		monitors = append(monitors, monitor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing monitors: %w", err)
	}
	return monitors, nil
}

// FindMonitor returns one monitor.
func (r *CronRepository) FindMonitor(ctx context.Context, id int64) (domain.CronMonitor, error) {
	return r.findMonitor(ctx, r.db, "id = ?", id)
}

// FindMonitorBySlug returns a project's monitor by the identity an SDK sends.
func (r *CronRepository) FindMonitorBySlug(
	ctx context.Context, projectID int64, slug string,
) (domain.CronMonitor, error) {
	return r.findMonitor(ctx, r.db, "project_id = ? AND slug = ?", projectID, slug)
}

// FindMonitorByPingKey returns the monitor a ping URL names.
func (r *CronRepository) FindMonitorByPingKey(
	ctx context.Context, pingKey string,
) (domain.CronMonitor, error) {
	return r.findMonitor(ctx, r.db, "ping_key = ?", pingKey)
}

func (r *CronRepository) findMonitor(
	ctx context.Context, q queryer, where string, args ...any,
) (domain.CronMonitor, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+monitorColumns+" FROM cron_monitors WHERE "+where, args...)
	monitor, err := scanMonitor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CronMonitor{}, domain.ErrMonitorNotFound
	}
	if err != nil {
		return domain.CronMonitor{}, err
	}
	return monitor, nil
}

// DeleteMonitor removes a monitor and, by cascade, its check-ins.
func (r *CronRepository) DeleteMonitor(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, "DELETE FROM cron_monitors WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("deleting monitor %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("deleting monitor %d: %w", id, err)
	}
	if affected == 0 {
		return domain.ErrMonitorNotFound
	}
	return nil
}

// HasEnabledMonitors reports whether anything is being watched.
//
// LIMIT 1 rather than a count: the answer is asked at boot and on every
// configuration change, and "is there at least one" does not need to know how
// many there are (ADR 014).
func (r *CronRepository) HasEnabledMonitors(ctx context.Context) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx,
		"SELECT 1 FROM cron_monitors WHERE enabled = 1 LIMIT 1").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("asking whether any monitor is enabled: %w", err)
	}
	return true, nil
}

// RecordCheckIn writes a reported run and everything it changes.
func (r *CronRepository) RecordCheckIn(
	ctx context.Context, write *ports.CheckInWrite,
) (domain.CronCheckIn, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.CronCheckIn{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	checkIn, err := writeCheckIn(ctx, tx, write)
	if err != nil {
		return domain.CronCheckIn{}, err
	}
	if err := applyOutcome(ctx, tx, write.Monitor.ID, write.Outcome, &write.At); err != nil {
		return domain.CronCheckIn{}, err
	}
	if err := r.enqueue(ctx, tx, write.Monitor.ProjectID, write.Monitor.ID, write.Event); err != nil {
		return domain.CronCheckIn{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.CronCheckIn{}, fmt.Errorf("committing a check-in: %w", err)
	}
	return checkIn, nil
}

// writeCheckIn either closes the run this one finishes or opens a new row.
//
// The match is by the SDK's correlation id when there is one, and by "the
// oldest run still open on this monitor" when there is not — which is the only
// thing a ping can mean, because a curl at the end of a crontab line has
// nowhere to carry an id.
func writeCheckIn(
	ctx context.Context, tx *sql.Tx, write *ports.CheckInWrite,
) (domain.CronCheckIn, error) {
	if write.Status == domain.CheckInProgress {
		return insertCheckIn(ctx, tx, write, write.At, nil)
	}

	open, err := findOpenCheckIn(ctx, tx, write.Monitor.ID, write.CheckInID)
	if err != nil {
		return domain.CronCheckIn{}, err
	}
	if open == nil {
		// A finish with nothing open. Normal, and the commonest shape there
		// is: a plain `curl .../ping/<key>` at the end of a crontab line
		// never announced a start. The run is recorded as instantaneous
		// rather than invented, because the only honest duration here is the
		// one the SDK reported, if any.
		return insertCheckIn(ctx, tx, write, write.At, &write.At)
	}

	duration := write.DurationMS
	if duration <= 0 {
		duration = write.At.Sub(open.StartedAt).Milliseconds()
		if duration < 0 {
			duration = 0
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE cron_checkins SET status = ?, finished_at = ?, duration_ms = ?, environment = ?
		WHERE id = ?`,
		string(write.Status), formatTime(write.At), duration,
		firstNonEmptyString(write.Environment, open.Environment), open.ID,
	); err != nil {
		return domain.CronCheckIn{}, fmt.Errorf("closing a check-in: %w", err)
	}

	finished := write.At.UTC()
	closed := *open
	closed.Status = write.Status
	closed.FinishedAt = &finished
	closed.DurationMS = duration
	closed.Environment = firstNonEmptyString(write.Environment, open.Environment)
	return closed, nil
}

func insertCheckIn(
	ctx context.Context, tx *sql.Tx, write *ports.CheckInWrite, startedAt time.Time, finishedAt *time.Time,
) (domain.CronCheckIn, error) {
	duration := write.DurationMS
	if duration < 0 {
		duration = 0
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO cron_checkins (monitor_id, checkin_id, status, started_at, finished_at, duration_ms, environment)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		write.Monitor.ID, write.CheckInID, string(write.Status),
		formatTime(startedAt), nullableTime(finishedAt), duration, write.Environment,
	)
	if err != nil {
		return domain.CronCheckIn{}, fmt.Errorf("recording a check-in: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.CronCheckIn{}, fmt.Errorf("reading the new check-in's id: %w", err)
	}
	return domain.CronCheckIn{
		ID:          id,
		MonitorID:   write.Monitor.ID,
		CheckInID:   write.CheckInID,
		Status:      write.Status,
		StartedAt:   startedAt.UTC(),
		FinishedAt:  finishedAt,
		DurationMS:  duration,
		Environment: write.Environment,
	}, nil
}

// findOpenCheckIn returns the run a finish belongs to, or nil.
func findOpenCheckIn(
	ctx context.Context, q queryer, monitorID int64, checkInID string,
) (*domain.CronCheckIn, error) {
	query := `SELECT id, monitor_id, checkin_id, status, started_at, finished_at, duration_ms, environment
		FROM cron_checkins WHERE monitor_id = ? AND finished_at IS NULL`
	args := []any{monitorID}
	if checkInID != "" {
		query += " AND checkin_id = ?"
		args = append(args, checkInID)
	}
	query += " ORDER BY started_at LIMIT 1"

	checkIn, err := scanCheckIn(q.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &checkIn, nil
}

// ListCheckIns returns one monitor's runs, newest first.
func (r *CronRepository) ListCheckIns(
	ctx context.Context, monitorID int64, limit int,
) ([]domain.CronCheckIn, error) {
	switch {
	case limit <= 0:
		limit = DefaultCheckInPageSize
	case limit > MaxCheckInPageSize:
		limit = MaxCheckInPageSize
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, monitor_id, checkin_id, status, started_at, finished_at, duration_ms, environment
		FROM cron_checkins WHERE monitor_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`,
		monitorID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing check-ins: %w", err)
	}
	defer func() { _ = rows.Close() }()

	checkIns := make([]domain.CronCheckIn, 0, limit)
	for rows.Next() {
		checkIn, err := scanCheckIn(rows)
		if err != nil {
			return nil, err
		}
		checkIns = append(checkIns, checkIn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing check-ins: %w", err)
	}
	return checkIns, nil
}

// DueMonitors returns what the watcher has to look at.
//
// The margin is deliberately not in this query. It is a per-monitor column and
// the timestamps are text, so applying it here would mean date arithmetic in
// SQL over strings; what it would save is a handful of rows that are overdue
// but still inside their margin. The domain applies it instead, where it is a
// comparison of two instants with a table of tests behind it.
func (r *CronRepository) DueMonitors(
	ctx context.Context, now time.Time, limit int,
) ([]ports.DueMonitor, error) {
	if limit <= 0 {
		limit = DefaultCheckInPageSize
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+prefixed(monitorColumns, "m")+`,
			(SELECT MIN(c.started_at) FROM cron_checkins c
			  WHERE c.monitor_id = m.id AND c.finished_at IS NULL) AS open_since
		FROM cron_monitors m
		WHERE m.enabled = 1
		  AND ( (m.next_expected_at IS NOT NULL AND m.next_expected_at <= ?)
		     OR EXISTS (SELECT 1 FROM cron_checkins c
		                 WHERE c.monitor_id = m.id AND c.finished_at IS NULL) )
		ORDER BY m.next_expected_at, m.id
		LIMIT ?`, formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("reading the monitors that are due: %w", err)
	}
	defer func() { _ = rows.Close() }()

	due := make([]ports.DueMonitor, 0, limit)
	for rows.Next() {
		monitor, openSince, err := scanDueMonitor(rows)
		if err != nil {
			return nil, err
		}
		due = append(due, ports.DueMonitor{Monitor: monitor, OpenSince: openSince})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the monitors that are due: %w", err)
	}
	return due, nil
}

// ApplySweep writes what the watcher decided.
func (r *CronRepository) ApplySweep(ctx context.Context, write *ports.SweepWrite) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := applyOutcome(ctx, tx, write.MonitorID, write.Outcome, nil); err != nil {
		return err
	}
	projectID := int64(0)
	if write.Event != nil {
		projectID = write.Event.ProjectID
	}
	if err := r.enqueue(ctx, tx, projectID, write.MonitorID, write.Event); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing a monitor's new status: %w", err)
	}
	return nil
}

// applyOutcome moves a monitor's status and deadline.
//
// checkedInAt is nil for a sweep: the passage of time is not something being
// heard from, and moving last_checkin_at because a monitor was declared missed
// would make "when did this last actually run" answer with the moment somebody
// noticed it had not.
func applyOutcome(
	ctx context.Context, tx *sql.Tx, monitorID int64, outcome domain.CronOutcome, checkedInAt *time.Time,
) error {
	query := "UPDATE cron_monitors SET status = ?, next_expected_at = ?"
	args := []any{string(outcome.Status), formatTime(outcome.NextExpectedAt)}
	if checkedInAt != nil {
		query += ", last_checkin_at = ?"
		args = append(args, formatTime(*checkedInAt))
	}
	query += " WHERE id = ?"
	args = append(args, monitorID)

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("updating monitor %d: %w", monitorID, err)
	}
	return nil
}

// enqueue offers a monitor event to the alert rules, in this transaction.
//
// An error fails the whole write on purpose — see WithAlerts.
func (r *CronRepository) enqueue(
	ctx context.Context, tx *sql.Tx, projectID, monitorID int64, event *domain.AlertEvent,
) error {
	if r.alerts == nil || event == nil {
		return nil
	}
	if _, err := r.alerts.enqueueTx(ctx, tx, event, r.origin.MonitorURL(projectID, domain.MonitorKindCron, monitorID)); err != nil {
		return fmt.Errorf("queueing a monitor notification: %w", err)
	}
	return nil
}

// PruneCheckIns deletes check-ins older than cutoff.
func (r *CronRepository) PruneCheckIns(
	ctx context.Context, cutoff time.Time, limit int,
) (int64, error) {
	if limit <= 0 {
		limit = MaxCheckInPageSize
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM cron_checkins WHERE id IN (
			SELECT id FROM cron_checkins WHERE started_at < ? ORDER BY started_at LIMIT ?
		)`, formatTime(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("pruning check-ins: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pruning check-ins: %w", err)
	}
	return deleted, nil
}

func scanMonitor(row scanner) (domain.CronMonitor, error) {
	monitor, _, err := readMonitor(row, false)
	return monitor, err
}

func scanDueMonitor(row scanner) (domain.CronMonitor, *time.Time, error) {
	return readMonitor(row, true)
}

// readMonitor reads a monitor row, optionally with the one column that is not
// part of the entity: when the oldest unfinished run on it started. The
// column rides on the same query so one sweep is one round trip however many
// monitors are overdue.
func readMonitor(row scanner, withOpenSince bool) (domain.CronMonitor, *time.Time, error) {
	var (
		monitor      domain.CronMonitor
		scheduleType string
		schedule     string
		marginS      int64
		runtimeS     int64
		status       string
		lastCheckin  sql.NullString
		nextExpected sql.NullString
		enabled      int64
		createdAt    string
		openSince    sql.NullString
	)

	targets := []any{
		&monitor.ID, &monitor.ProjectID, &monitor.Slug, &monitor.PingKey,
		&scheduleType, &schedule, &monitor.Timezone,
		&marginS, &runtimeS, &status, &lastCheckin, &nextExpected, &enabled, &createdAt,
	}
	if withOpenSince {
		targets = append(targets, &openSince)
	}
	if err := row.Scan(targets...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.CronMonitor{}, nil, err //nolint:wrapcheck // the caller maps this to a domain error.
		}
		return domain.CronMonitor{}, nil, fmt.Errorf("reading a monitor: %w", err)
	}

	parsed, err := domain.ParseCronSpec(domain.ScheduleType(scheduleType), schedule)
	if err != nil {
		return domain.CronMonitor{}, nil, fmt.Errorf("monitor %d: %w", monitor.ID, err)
	}
	monitor.Schedule = parsed
	monitor.CheckinMargin = time.Duration(marginS) * time.Second
	monitor.MaxRuntime = time.Duration(runtimeS) * time.Second
	monitor.Status = domain.CronStatus(status)
	monitor.Enabled = enabled != 0

	if monitor.LastCheckinAt, err = parseNullableTime(lastCheckin); err != nil {
		return domain.CronMonitor{}, nil, err
	}
	if monitor.NextExpectedAt, err = parseNullableTime(nextExpected); err != nil {
		return domain.CronMonitor{}, nil, err
	}
	if monitor.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.CronMonitor{}, nil, err
	}

	var open *time.Time
	if withOpenSince {
		if open, err = parseNullableTime(openSince); err != nil {
			return domain.CronMonitor{}, nil, err
		}
	}
	return monitor, open, nil
}

func scanCheckIn(row scanner) (domain.CronCheckIn, error) {
	var (
		checkIn    domain.CronCheckIn
		status     string
		startedAt  string
		finishedAt sql.NullString
	)
	if err := row.Scan(&checkIn.ID, &checkIn.MonitorID, &checkIn.CheckInID, &status,
		&startedAt, &finishedAt, &checkIn.DurationMS, &checkIn.Environment); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.CronCheckIn{}, err //nolint:wrapcheck // the caller maps this to a domain error.
		}
		return domain.CronCheckIn{}, fmt.Errorf("reading a check-in: %w", err)
	}
	checkIn.Status = domain.CheckInStatus(status)

	var err error
	if checkIn.StartedAt, err = parseTime(startedAt); err != nil {
		return domain.CronCheckIn{}, err
	}
	if checkIn.FinishedAt, err = parseNullableTime(finishedAt); err != nil {
		return domain.CronCheckIn{}, err
	}
	return checkIn, nil
}

// prefixed qualifies a column list with a table alias, so the one definition
// of the monitor row serves both the plain reads and the join the sweep does.
func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for index, part := range parts {
		parts[index] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
