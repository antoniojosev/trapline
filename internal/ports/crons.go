package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// CheckInWrite is one reported run and everything it changes, offered to
// storage as a single unit.
//
// It is one struct and one call rather than "insert a check-in, then update
// the monitor, then queue a notification", because those three have to commit
// together. A crash between the second and the third leaves a monitor that
// says it recovered and an operator who was told it was broken and never told
// otherwise — the exact failure the outbox exists to prevent (ADR 015).
type CheckInWrite struct {
	// Monitor is the monitor as it was read, before the outcome is applied.
	Monitor domain.CronMonitor
	// CheckInID is the SDK's correlation id, empty for a ping.
	CheckInID string
	// Status is what the run reported.
	Status domain.CheckInStatus
	// At is when it was reported.
	At time.Time
	// DurationMS is what the SDK measured, zero when it said nothing and the
	// storage layer should derive it from the open row it is closing.
	DurationMS int64
	// Environment is which deployment reported it.
	Environment string
	// Outcome is what the domain decided this does to the monitor.
	Outcome domain.CronOutcome
	// Event is what to offer the alert rules, nil when the outcome is not
	// worth telling anybody about. It is built by the use case, because the
	// wording of a notification is not a storage decision.
	Event *domain.AlertEvent
}

// SweepWrite is what the watcher decided about one monitor, in the same
// single-transaction shape and for the same reason.
type SweepWrite struct {
	// MonitorID is which monitor this is about.
	MonitorID int64
	// Outcome is the new status and the new deadline.
	Outcome domain.CronOutcome
	// Event is what to offer the alert rules, nil when nothing changed worth
	// reporting.
	Event *domain.AlertEvent
}

// DueMonitor is one monitor the watcher has to look at, together with the one
// fact it cannot answer from the monitor row alone.
type DueMonitor struct {
	// Monitor is the monitor.
	Monitor domain.CronMonitor
	// OpenSince is when the oldest unfinished run announced itself, nil when
	// there is none. It is read in the same query as the monitor, so one
	// sweep is one round trip however many monitors are overdue.
	OpenSince *time.Time
}

// CronRepository stores cron monitors, their check-ins and the state the
// watcher derives from both.
//
// One port rather than two because the two are one transaction: a check-in
// arrives, it writes a row, it moves the monitor's status and deadline, and
// it may queue a notification — and those four either all happened or none of
// them did (ADR 015, ADR 016).
type CronRepository interface {
	// CreateMonitor stores a new monitor.
	CreateMonitor(ctx context.Context, monitor *domain.CronMonitor) (domain.CronMonitor, error)
	// UpdateMonitor rewrites a monitor's configuration — schedule, zone,
	// margin, runtime, enabled — and its deadline. It never rewrites the ping
	// key: rotating a credential is a different operation from editing a
	// schedule, and doing both at once would silently break every crontab
	// line that carries the old one.
	UpdateMonitor(ctx context.Context, monitor *domain.CronMonitor) (domain.CronMonitor, error)
	// ListMonitors returns a project's monitors, oldest first.
	ListMonitors(ctx context.Context, projectID int64) ([]domain.CronMonitor, error)
	// FindMonitor returns one monitor, or domain.ErrMonitorNotFound.
	FindMonitor(ctx context.Context, id int64) (domain.CronMonitor, error)
	// FindMonitorBySlug returns a project's monitor by the identity an SDK
	// sends, or domain.ErrMonitorNotFound.
	FindMonitorBySlug(ctx context.Context, projectID int64, slug string) (domain.CronMonitor, error)
	// FindMonitorByPingKey returns the monitor a ping URL names.
	//
	// It carries no project, deliberately: the key is the credential and the
	// URL is meant to be short enough to paste at the end of a crontab line
	// (ADR 016).
	FindMonitorByPingKey(ctx context.Context, pingKey string) (domain.CronMonitor, error)
	// DeleteMonitor removes a monitor and its check-ins.
	DeleteMonitor(ctx context.Context, id int64) error
	// HasEnabledMonitors reports whether anything is being watched. This is
	// the question the scheduler asks before the cron-watch job exists at all
	// (ADR 014).
	HasEnabledMonitors(ctx context.Context) (bool, error)

	// RecordCheckIn writes a reported run and everything it changes.
	RecordCheckIn(ctx context.Context, write *CheckInWrite) (domain.CronCheckIn, error)
	// ListCheckIns returns one monitor's runs, newest first.
	ListCheckIns(ctx context.Context, monitorID int64, limit int) ([]domain.CronCheckIn, error)

	// DueMonitors returns the enabled monitors whose deadline has passed or
	// which have a run still open, oldest deadline first.
	DueMonitors(ctx context.Context, now time.Time, limit int) ([]DueMonitor, error)
	// ApplySweep writes what the watcher decided.
	ApplySweep(ctx context.Context, write *SweepWrite) error

	// PruneCheckIns deletes check-ins older than cutoff, at most limit per
	// call. A history with no end is a disk that fills.
	PruneCheckIns(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}
