package ports

import (
	"context"
	"net/url"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// UptimeRepository stores monitors, their check history and the daily roll-up.
//
// One port for the three because they are one transaction. A check produces a
// result row, a new monitor state and — when the state changed — the
// notifications about it, and those have to be committed together or a restart
// in the middle either alerts about an outage that did not happen or stays
// quiet about one that did (ADR 015).
type UptimeRepository interface {
	// CreateMonitor stores a validated monitor.
	//
	// By pointer, here and below, for the reason domain.UptimeMonitor's own
	// methods give: it is a little over two hundred bytes and these are the
	// calls the timer makes over every monitor in the installation. Nothing
	// behind this port mutates what it is handed.
	CreateMonitor(ctx context.Context, monitor *domain.UptimeMonitor) (domain.UptimeMonitor, error)
	// ListMonitors returns the monitors of one project, or every monitor when
	// projectID is nil, oldest first.
	ListMonitors(ctx context.Context, projectID *int64) ([]domain.UptimeMonitor, error)
	// FindMonitor returns one monitor, or domain.ErrMonitorNotFound.
	FindMonitor(ctx context.Context, id int64) (domain.UptimeMonitor, error)
	// DeleteMonitor removes a monitor, its results and its aggregates.
	DeleteMonitor(ctx context.Context, id int64) error
	// SetMonitorEnabled switches a monitor on or off without deleting its
	// history.
	SetMonitorEnabled(ctx context.Context, id int64, enabled bool) (domain.UptimeMonitor, error)

	// HasEnabledMonitors reports whether anything is being checked. This is
	// the question the scheduler asks before the uptime job exists at all
	// (ADR 014).
	HasEnabledMonitors(ctx context.Context) (bool, error)
	// DueMonitors leases the enabled monitors whose next check has come,
	// pushing their next check into the future so a second pass cannot take
	// them. Leasing rather than reading, for the same reason the outbox
	// leases: a pass that dies mid-check must not hold anything, and the row
	// simply becomes due again on schedule.
	DueMonitors(ctx context.Context, now time.Time, limit int) ([]domain.UptimeMonitor, error)

	// RecordResult writes one check: the result row, the day's aggregate, the
	// monitor's new state, and the notifications a transition produced — in
	// one transaction. It returns how many notifications were queued.
	RecordResult(
		ctx context.Context,
		before *domain.UptimeMonitor,
		after *domain.UptimeMonitor,
		result domain.CheckResult,
		transition domain.UptimeTransition,
	) (int, error)

	// Results reads a monitor's recent checks, newest first.
	Results(ctx context.Context, monitorID int64, limit int) ([]domain.CheckResult, error)
	// DailyUptime reads the roll-up for a range of days, oldest first, with
	// absent days absent rather than zero — the caller knows how to draw a
	// day nothing ran on, and the store must not invent checks.
	DailyUptime(ctx context.Context, monitorID int64, from, to time.Time) ([]domain.UptimeDay, error)

	// PruneResults deletes checks older than cutoff, at most limit per call.
	// The history is a log, and a log with no end is a disk that fills; the
	// daily aggregate is what survives it (ADR 001).
	PruneResults(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// PruneDaily deletes aggregate rows older than cutoff.
	PruneDaily(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// URLGuard decides whether this server may fetch a URL somebody else chose.
//
// Declared here, on the consumer side, rather than the use case importing the
// package that implements it. That is the usual rule (ports/doc.go) and it
// earns its keep twice over here: the use case depends on "something checks
// this", the gate can substitute a guard that allows everything, and no test
// of the uptime logic has to arrange DNS.
type URLGuard interface {
	// ParseTarget reads a raw URL and rejects the shapes that are not check
	// targets at all — a scheme that is not http, credentials in the URL.
	ParseTarget(raw string) (*url.URL, error)
	// Check resolves the host and refuses if any address behind it is one
	// this server does not visit. allowPrivate is the monitor's own opt-in;
	// the installation-wide one belongs to the guard.
	Check(ctx context.Context, target *url.URL, allowPrivate bool) error
}

// UptimeChecker performs one HTTP check.
//
// A port because reaching the network is an adapter's job, and because it is
// the seam that lets the whole state machine — thresholds, transitions,
// notifications — be tested without a server to point at.
type UptimeChecker interface {
	// Check fetches the monitor's URL once and reports what happened. It
	// returns an error only when the check could not be attempted at all; a
	// target that is down is a CheckResult with OK false, which is the normal
	// case and not a failure of this call.
	Check(ctx context.Context, monitor *domain.UptimeMonitor, now time.Time) domain.CheckResult
}
