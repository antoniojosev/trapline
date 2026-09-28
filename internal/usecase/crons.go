package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// The watcher's own numbers.
const (
	// CronWatchInterval is how often overdue monitors are looked for.
	//
	// Thirty seconds, and the reason is that this interval is the resolution
	// of every deadline in the subsystem: a monitor with a ten-second margin
	// cannot be told it is late sooner than the next sweep. It is not shorter
	// because the sweep is a write transaction against the one SQLite writer
	// this product has, and a monitoring feature that competes with ingest
	// for it would be paying for itself with the thing it is monitoring.
	CronWatchInterval = 30 * time.Second
	// CronWatchBatch bounds one pass, so a sweep can never hold a long write
	// transaction on a live store.
	CronWatchBatch = 200
	// CheckInKeep is how long a check-in stays in the history. It matches the
	// engine's default retention for the check_in category.
	CheckInKeep = 90 * 24 * time.Hour
	// checkInPruneInterval is how often that history is swept. Hourly, off
	// the thirty-second path, the same way the notifier keeps its log sweep
	// off its once-a-second path.
	checkInPruneInterval = time.Hour
	// checkInPruneBatch bounds one sweep.
	checkInPruneBatch = 500
)

// Crons is the cron-monitoring subsystem seen from outside: the monitors, the
// two ways a run reports itself, and the watcher that notices when one does
// not.
type Crons struct {
	repo  ports.CronRepository
	clock ports.Clock
	log   *slog.Logger
	// monitorsChanged is called after a monitor is added, removed or switched
	// on or off, and is how the scheduler learns that the watcher now has —
	// or no longer has — anything to do. A positional argument on the
	// constructor rather than a setter, for the reason ADR 014 gives: a hook
	// that can be left unset is one that will be, and the symptom is a
	// subsystem that only starts after a restart.
	monitorsChanged func()
	// lastPrune keeps the history sweep off the thirty-second path. In
	// memory because losing it costs one extra sweep after a restart.
	lastPrune time.Time
}

// NewCrons wires the use case.
func NewCrons(
	repo ports.CronRepository, clock ports.Clock, logger *slog.Logger, monitorsChanged func(),
) *Crons {
	if logger == nil {
		logger = slog.Default()
	}
	if monitorsChanged == nil {
		monitorsChanged = func() {}
	}
	return &Crons{repo: repo, clock: clock, log: logger, monitorsChanged: monitorsChanged}
}

// MonitorSpec is a monitor as somebody asks for it.
//
// The two schedule halves arrive as strings because that is what every client
// of this use case has: a CLI flag, a JSON field, an SDK's monitor_config.
// Parsing them here rather than at each transport is what keeps the three
// clients from each having their own idea of what "5 minutes" means (ADR 006).
type MonitorSpec struct {
	// Slug is the identity an SDK will send.
	Slug string
	// ScheduleType is crontab or interval. Empty means crontab, because that
	// is what somebody pasting a line out of their crontab has.
	ScheduleType string
	// Schedule is the expression: five crontab fields, or "<n> <unit>".
	Schedule string
	// Timezone is an IANA zone name. Empty means UTC.
	Timezone string
	// CheckinMargin and MaxRuntime are zero for the defaults.
	CheckinMargin time.Duration
	MaxRuntime    time.Duration
	// Enabled is whether it is watched.
	Enabled bool
}

// Add creates a monitor.
func (c *Crons) Add(ctx context.Context, projectID int64, spec MonitorSpec) (domain.CronMonitor, error) {
	schedule, err := parseSchedule(spec.ScheduleType, spec.Schedule)
	if err != nil {
		return domain.CronMonitor{}, err
	}
	monitor, err := domain.NewCronMonitor(projectID, spec.Slug, schedule, spec.Timezone,
		spec.CheckinMargin, spec.MaxRuntime, spec.Enabled, c.clock.Now())
	if err != nil {
		return domain.CronMonitor{}, err
	}
	stored, err := c.repo.CreateMonitor(ctx, &monitor)
	if err != nil {
		return domain.CronMonitor{}, err
	}
	// The first monitor is what brings the watcher into existence, and it has
	// to happen while the person who created it is still looking at the
	// screen — not at the next restart (ADR 014).
	c.monitorsChanged()
	return stored, nil
}

// MonitorChanges is what an update may touch. Every field is optional, and an
// absent one leaves what is stored alone.
type MonitorChanges struct {
	ScheduleType  *string
	Schedule      *string
	Timezone      *string
	CheckinMargin *time.Duration
	MaxRuntime    *time.Duration
	Enabled       *bool
}

// Update changes a monitor's configuration.
//
// The ping key is not among the fields: it is a credential that lives at the
// end of somebody's crontab line, and an edit to a schedule must not silently
// invalidate it.
func (c *Crons) Update(
	ctx context.Context, id int64, changes MonitorChanges,
) (domain.CronMonitor, error) {
	monitor, err := c.repo.FindMonitor(ctx, id)
	if err != nil {
		return domain.CronMonitor{}, err
	}

	scheduleChanged := false
	if changes.ScheduleType != nil || changes.Schedule != nil {
		kind := string(monitor.Schedule.Type)
		if changes.ScheduleType != nil {
			kind = *changes.ScheduleType
		}
		value := monitor.Schedule.String()
		if changes.Schedule != nil {
			value = *changes.Schedule
		}
		schedule, err := parseSchedule(kind, value)
		if err != nil {
			return domain.CronMonitor{}, err
		}
		monitor.Schedule, scheduleChanged = schedule, true
	}
	if changes.Timezone != nil {
		zone, err := domain.CleanTimezone(*changes.Timezone)
		if err != nil {
			return domain.CronMonitor{}, err
		}
		monitor.Timezone, scheduleChanged = zone, true
	}
	if changes.CheckinMargin != nil {
		monitor.CheckinMargin = *changes.CheckinMargin
	}
	if changes.MaxRuntime != nil {
		monitor.MaxRuntime = *changes.MaxRuntime
	}
	if changes.Enabled != nil {
		monitor.Enabled = *changes.Enabled
	}

	// Re-validated as a whole rather than field by field, so an update cannot
	// reach a state a creation could not.
	validated, err := domain.NewCronMonitor(monitor.ProjectID, monitor.Slug, monitor.Schedule,
		monitor.Timezone, monitor.CheckinMargin, monitor.MaxRuntime, monitor.Enabled, c.clock.Now())
	if err != nil {
		return domain.CronMonitor{}, err
	}
	monitor.CheckinMargin, monitor.MaxRuntime = validated.CheckinMargin, validated.MaxRuntime
	if scheduleChanged {
		// A new schedule means the old deadline describes a schedule nobody
		// is running any more, so it is recomputed from now. Leaving it would
		// report a monitor as missed against the timetable it had yesterday.
		monitor.NextExpectedAt = validated.NextExpectedAt
	}

	stored, err := c.repo.UpdateMonitor(ctx, &monitor)
	if err != nil {
		return domain.CronMonitor{}, err
	}
	c.monitorsChanged()
	return stored, nil
}

// Monitors lists a project's monitors.
func (c *Crons) Monitors(ctx context.Context, projectID int64) ([]domain.CronMonitor, error) {
	return c.repo.ListMonitors(ctx, projectID)
}

// Monitor reads one monitor.
func (c *Crons) Monitor(ctx context.Context, id int64) (domain.CronMonitor, error) {
	return c.repo.FindMonitor(ctx, id)
}

// Remove deletes a monitor and its history.
func (c *Crons) Remove(ctx context.Context, id int64) error {
	if err := c.repo.DeleteMonitor(ctx, id); err != nil {
		return err
	}
	// And removing the last one gives the goroutine back.
	c.monitorsChanged()
	return nil
}

// CheckIns reads one monitor's history, newest first.
func (c *Crons) CheckIns(ctx context.Context, monitorID, limit int64) ([]domain.CronCheckIn, error) {
	if _, err := c.repo.FindMonitor(ctx, monitorID); err != nil {
		return nil, err
	}
	return c.repo.ListCheckIns(ctx, monitorID, int(limit))
}

// HasEnabled reports whether anything is being watched. This is what the
// scheduler asks before the cron-watch job exists (ADR 014).
func (c *Crons) HasEnabled(ctx context.Context) (bool, error) {
	return c.repo.HasEnabledMonitors(ctx)
}

// Report is a check-in arriving through the envelope.
//
// declare is the monitor_config the SDK sent, nil when it sent none. When it
// is present the monitor is created if it does not exist and reconfigured if
// it does, which is what Sentry does and what makes instrumenting a cron job
// one decorator rather than a form somebody fills in first (ADR 016).
type Report struct {
	// Slug is which monitor this is about.
	Slug string
	// CheckInID is the SDK's correlation id, empty when it sent none.
	CheckInID string
	// Status is in_progress, ok or error.
	Status domain.CheckInStatus
	// Duration is what the SDK measured, zero when it said nothing.
	Duration time.Duration
	// Environment is which deployment reported it.
	Environment string
	// Declare is the schedule the SDK declared, nil when it declared none.
	Declare *MonitorSpec
}

// CheckInResult is what a reported run did.
type CheckInResult struct {
	// Monitor is the monitor as it stands afterwards.
	Monitor domain.CronMonitor
	// CheckIn is the row that was written.
	CheckIn domain.CronCheckIn
	// Created is whether this check-in brought the monitor into existence.
	Created bool
}

// Accept files a check-in that arrived through the envelope.
func (c *Crons) Accept(ctx context.Context, projectID int64, report Report) (CheckInResult, error) {
	slug, err := domain.CleanMonitorSlug(report.Slug)
	if err != nil {
		return CheckInResult{}, err
	}
	checkInID, err := domain.CleanCheckInID(report.CheckInID)
	if err != nil {
		return CheckInResult{}, err
	}

	monitor, created, err := c.resolve(ctx, projectID, slug, report.Declare)
	if err != nil {
		return CheckInResult{}, err
	}
	if !monitor.Enabled {
		return CheckInResult{}, fmt.Errorf("%w: %s", domain.ErrMonitorDisabled, monitor.Slug)
	}

	result, err := c.record(ctx, &monitor, checkInID, report.Status, report.Duration, report.Environment)
	if err != nil {
		return CheckInResult{}, err
	}
	result.Created = created
	return result, nil
}

// Ping is a check-in that arrived as a bare HTTP request to /ping/{key}.
//
// It has no body, no project and no correlation id, because the caller is a
// line at the end of a crontab entry rather than an SDK. The key is the whole
// of the authentication (ADR 016).
func (c *Crons) Ping(
	ctx context.Context, pingKey string, status domain.CheckInStatus,
) (CheckInResult, error) {
	monitor, err := c.repo.FindMonitorByPingKey(ctx, pingKey)
	if err != nil {
		return CheckInResult{}, err
	}
	if !monitor.Enabled {
		return CheckInResult{}, fmt.Errorf("%w: %s", domain.ErrMonitorDisabled, monitor.Slug)
	}
	return c.record(ctx, &monitor, "", status, 0, "")
}

// resolve finds the monitor a check-in is about, creating or reconfiguring it
// from a declaration.
func (c *Crons) resolve(
	ctx context.Context, projectID int64, slug string, declare *MonitorSpec,
) (domain.CronMonitor, bool, error) {
	monitor, err := c.repo.FindMonitorBySlug(ctx, projectID, slug)
	switch {
	case err == nil:
		if declare == nil {
			return monitor, false, nil
		}
		updated, err := c.applyDeclaration(ctx, &monitor, declare)
		return updated, false, err

	case errors.Is(err, domain.ErrMonitorNotFound):
		if declare == nil {
			// A check-in for a monitor nobody declared. Refused rather than
			// invented: without a schedule there is nothing to be late for,
			// so the monitor would exist and never report anything — which
			// looks like the feature working and is the opposite.
			return domain.CronMonitor{}, false, fmt.Errorf(
				"%w: %q, and the check-in carried no monitor_config to create it from",
				domain.ErrMonitorNotFound, slug)
		}
		spec := *declare
		spec.Slug, spec.Enabled = slug, true
		created, err := c.Add(ctx, projectID, spec)
		return created, true, err

	default:
		return domain.CronMonitor{}, false, err
	}
}

// applyDeclaration reconciles a stored monitor with what an SDK just declared.
//
// Only the fields the declaration actually carries, and only when they differ:
// an SDK repeating the same monitor_config on every run must not produce a
// write per check-in, and one that omits a field must not reset it to a
// default the operator deliberately changed.
func (c *Crons) applyDeclaration(
	ctx context.Context, monitor *domain.CronMonitor, declare *MonitorSpec,
) (domain.CronMonitor, error) {
	changes := MonitorChanges{}
	if declare.Schedule != "" {
		kind := declare.ScheduleType
		if kind == "" {
			kind = string(domain.ScheduleCrontab)
		}
		if kind != string(monitor.Schedule.Type) || declare.Schedule != monitor.Schedule.String() {
			changes.ScheduleType, changes.Schedule = &kind, &declare.Schedule
		}
	}
	if declare.Timezone != "" && declare.Timezone != monitor.Timezone {
		changes.Timezone = &declare.Timezone
	}
	if declare.CheckinMargin > 0 && declare.CheckinMargin != monitor.CheckinMargin {
		changes.CheckinMargin = &declare.CheckinMargin
	}
	if declare.MaxRuntime > 0 && declare.MaxRuntime != monitor.MaxRuntime {
		changes.MaxRuntime = &declare.MaxRuntime
	}

	if changes == (MonitorChanges{}) {
		return *monitor, nil
	}
	return c.Update(ctx, monitor.ID, changes)
}

// record applies one reported run.
func (c *Crons) record(
	ctx context.Context, monitor *domain.CronMonitor, checkInID string,
	status domain.CheckInStatus, duration time.Duration, environment string,
) (CheckInResult, error) {
	now := c.clock.Now()
	outcome, err := domain.ApplyCheckIn(monitor, status, now)
	if err != nil {
		return CheckInResult{}, err
	}

	write := ports.CheckInWrite{
		Monitor:     *monitor,
		CheckInID:   checkInID,
		Status:      status,
		At:          now,
		DurationMS:  duration.Milliseconds(),
		Environment: environment,
		Outcome:     outcome,
		Event:       monitorEvent(monitor, outcome, now),
	}
	checkIn, err := c.repo.RecordCheckIn(ctx, &write)
	if err != nil {
		return CheckInResult{}, err
	}

	after := *monitor
	after.Status = outcome.Status
	deadline := outcome.NextExpectedAt
	after.NextExpectedAt = &deadline
	checkedIn := now.UTC()
	after.LastCheckinAt = &checkedIn
	return CheckInResult{Monitor: after, CheckIn: checkIn}, nil
}

// CronSweepResult is what one pass of the watcher did.
type CronSweepResult struct {
	// Checked is how many overdue or open monitors were looked at.
	Checked int
	// Missed, TimedOut and Recovered count the transitions.
	Missed   int
	TimedOut int
	// Pruned is how many check-ins were dropped from the history.
	Pruned int64
}

// Sweep looks for monitors that should have been heard from and were not.
//
// This is the whole of the feature that a check-in cannot provide: everything
// else in this subsystem is driven by something arriving, and the one event
// worth paying for is the absence of one.
func (c *Crons) Sweep(ctx context.Context) (CronSweepResult, error) {
	now := c.clock.Now()
	due, err := c.repo.DueMonitors(ctx, now, CronWatchBatch)
	if err != nil {
		return CronSweepResult{}, err
	}

	result := CronSweepResult{Checked: len(due)}
	for index := range due {
		monitor := &due[index].Monitor
		outcome, changed, err := domain.SweepCron(monitor, due[index].OpenSince, now)
		if err != nil {
			// One monitor this build cannot evaluate — a timezone this
			// machine's tzdata does not have — must not stop the sweep for
			// every other one.
			c.log.Error("could not evaluate a cron monitor",
				"monitor", monitor.Slug, "monitor_id", monitor.ID, "error", err)
			continue
		}
		if !changed {
			continue
		}

		write := ports.SweepWrite{
			MonitorID: monitor.ID,
			Outcome:   outcome,
			Event:     monitorEvent(monitor, outcome, now),
		}
		if err := c.repo.ApplySweep(ctx, &write); err != nil {
			return result, err
		}
		switch outcome.Status {
		case domain.CronMissed:
			result.Missed++
		case domain.CronTimeout:
			result.TimedOut++
		}
	}

	pruned, err := c.prune(ctx, now)
	if err != nil {
		return result, err
	}
	result.Pruned = pruned
	return result, nil
}

func (c *Crons) prune(ctx context.Context, now time.Time) (int64, error) {
	if !c.lastPrune.IsZero() && now.Sub(c.lastPrune) < checkInPruneInterval {
		return 0, nil
	}
	c.lastPrune = now
	return c.repo.PruneCheckIns(ctx, now.Add(-CheckInKeep), checkInPruneBatch)
}

// monitorEvent renders a state change for the alert rules, or nil when the
// change is not one worth telling anybody about.
//
// The wording lives here rather than in the domain because it is a message to
// a person, and it lives here rather than in a transport because all three
// clients send the same one (ADR 004, ADR 006).
func monitorEvent(
	monitor *domain.CronMonitor, outcome domain.CronOutcome, now time.Time,
) *domain.AlertEvent {
	if outcome.Trigger == "" {
		return nil
	}

	var title string
	switch outcome.Trigger {
	case domain.TriggerCronMissed:
		title = monitor.Slug + " did not check in"
	case domain.TriggerCronTimeout:
		title = monitor.Slug + " started and never finished"
	case domain.TriggerCronFailed:
		title = monitor.Slug + " reported a failure"
	case domain.TriggerCronRecovered:
		title = monitor.Slug + " is checking in again"
	default:
		title = monitor.Slug + " changed state"
	}

	return &domain.AlertEvent{
		Kind:        outcome.Trigger,
		ProjectID:   monitor.ProjectID,
		MonitorKind: domain.MonitorKindCron,
		MonitorID:   monitor.ID,
		MonitorSlug: monitor.Slug,
		Title:       title,
		// The schedule is the culprit, in the sense the field means
		// everywhere else: it is where to look. "Every minute, in
		// America/Caracas" is what turns "did not check in" into something
		// somebody can check against a crontab.
		Culprit: monitor.Schedule.String() + " (" + monitor.Timezone + ")",
		Level:   domain.Level(levelFor(outcome.Trigger)),
		At:      now,
	}
}

// levelFor grades a monitor event. A recovery is information; everything else
// here is an error, because a job that did not run is a job that did not run.
func levelFor(kind domain.TriggerKind) string {
	if kind == domain.TriggerCronRecovered {
		return "info"
	}
	return "error"
}

// parseSchedule turns the two strings every client has into a schedule.
func parseSchedule(kind, value string) (domain.CronSpec, error) {
	if kind == "" {
		kind = string(domain.ScheduleCrontab)
	}
	return domain.ParseCronSpec(domain.ScheduleType(kind), value)
}

// Job returns the watcher as something the scheduler can run.
func (c *Crons) Job() CronWatchJob { return CronWatchJob{crons: c} }

// CronWatchJob notices monitors that should have reported and did not.
//
// Like the other jobs it names no scheduler type: the contract is structural,
// so the use cases do not depend on the thing that runs them (ADR 014).
type CronWatchJob struct{ crons *Crons }

// Name identifies the job wherever it is reported.
func (j CronWatchJob) Name() string { return "cron-watch" }

// Interval is how long the scheduler waits between passes.
func (j CronWatchJob) Interval() time.Duration { return CronWatchInterval }

// Run does one sweep.
func (j CronWatchJob) Run(ctx context.Context) error {
	result, err := j.crons.Sweep(ctx)
	if err != nil {
		return err
	}
	if result.Missed > 0 || result.TimedOut > 0 || result.Pruned > 0 {
		// Only when something happened. A line every thirty seconds saying
		// nothing did would bury the ones that say something did.
		j.crons.log.Info("cron monitors",
			"checked", result.Checked, "missed", result.Missed,
			"timed_out", result.TimedOut, "pruned", result.Pruned)
	}
	return nil
}
