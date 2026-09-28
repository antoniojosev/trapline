package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// The uptime job's own numbers.
const (
	// UptimeTickInterval is how often the job looks for monitors that are due.
	//
	// It is not how often a monitor is checked — that is the monitor's own
	// interval, and the floor on it is thirty seconds. This is the resolution
	// of the scheduler underneath, so a check due at :30 runs within five
	// seconds of :30 rather than whenever a much coarser tick happens to land.
	// What it costs is one indexed range scan every five seconds, and only on
	// an installation that has enabled a monitor, because otherwise the job
	// does not exist at all (ADR 005, ADR 014).
	UptimeTickInterval = 5 * time.Second
	// UptimeBatch bounds how many monitors one pass leases.
	UptimeBatch = 50
	// UptimeConcurrency is how many checks run at once.
	//
	// Eight, and the bound is the point rather than the number. Checks are
	// almost entirely waiting, so running them one at a time would make a pass
	// over fifty monitors take as long as the sum of their timeouts — but
	// running them all at once means an installation with two hundred
	// monitors opens two hundred sockets on a tick, which is a self-inflicted
	// outage on a small VPS. A semaphore keeps the cost flat regardless of how
	// many monitors somebody adds (ADR 016).
	UptimeConcurrency = 8

	// UptimeResultRetention is how long individual checks are kept. Ninety
	// days, which is the window the status page draws (ADR 017).
	UptimeResultRetention = 90 * 24 * time.Hour
	// UptimeDailyRetention is how long the roll-up is kept. Far longer than
	// the checks it summarises, because it is what answers "was this worse
	// last year" once they are gone — and it is four numbers a day.
	UptimeDailyRetention = 400 * 24 * time.Hour
	// uptimePruneInterval keeps the sweep off the five-second path.
	uptimePruneInterval = time.Hour
	// uptimePruneBatch bounds one sweep, so it never holds a long write
	// transaction on a live store — the same rule retention follows.
	uptimePruneBatch = 500
)

// Uptime is the monitoring subsystem seen from outside: the monitors, their
// history, and the job that keeps both up to date.
type Uptime struct {
	repo    ports.UptimeRepository
	checker ports.UptimeChecker
	guard   ports.URLGuard
	clock   ports.Clock
	log     *slog.Logger

	// monitorsChanged is called after a monitor is added, removed or switched,
	// and is how the scheduler learns that the job now has — or no longer has
	// — anything to do. A positional argument on the constructor rather than a
	// setter, for the reason ADR 014 gives: a hook that can be left unset is
	// one that will be, and the symptom is a subsystem that only starts after
	// a restart.
	monitorsChanged func()

	// lastPrune keeps the history sweep off the once-every-five-seconds path.
	// In memory because losing it costs one extra sweep after a restart.
	mu        sync.Mutex
	lastPrune time.Time
}

// NewUptime wires the use case.
func NewUptime(
	repo ports.UptimeRepository,
	checker ports.UptimeChecker,
	guard ports.URLGuard,
	clock ports.Clock,
	logger *slog.Logger,
	monitorsChanged func(),
) *Uptime {
	if logger == nil {
		logger = slog.Default()
	}
	if monitorsChanged == nil {
		monitorsChanged = func() {}
	}
	return &Uptime{
		repo: repo, checker: checker, guard: guard, clock: clock,
		log: logger, monitorsChanged: monitorsChanged,
	}
}

// AddMonitor validates a monitor and stores it.
//
// Two validations, in this order, and both are refusals rather than warnings.
// The domain decides whether the monitor makes sense — a name, a safe method,
// an interval above the floor. The guard decides whether this server is
// willing to visit the address behind the URL, and that question is asked here
// rather than at the first check on purpose: a monitor pointed at the metadata
// endpoint should fail while the person who typed it is looking at the screen,
// with a sentence saying why, not silently at 3am from a background job
// (ADR 016).
func (u *Uptime) AddMonitor(ctx context.Context, monitor *domain.UptimeMonitor) (domain.UptimeMonitor, error) {
	validated, err := domain.NewUptimeMonitor(monitor, u.clock.Now())
	if err != nil {
		return domain.UptimeMonitor{}, err
	}

	target, err := u.guard.ParseTarget(validated.URL)
	if err != nil {
		return domain.UptimeMonitor{}, err
	}
	if err := u.guard.Check(ctx, target, validated.AllowPrivate); err != nil {
		return domain.UptimeMonitor{}, err
	}

	saved, err := u.repo.CreateMonitor(ctx, &validated)
	if err != nil {
		return domain.UptimeMonitor{}, err
	}
	// The first monitor is what brings the job into existence, and it has to
	// happen while the person who added it is still watching (ADR 014).
	u.monitorsChanged()
	return saved, nil
}

// Monitors lists the monitors of one project, or all of them.
func (u *Uptime) Monitors(ctx context.Context, projectID *int64) ([]domain.UptimeMonitor, error) {
	return u.repo.ListMonitors(ctx, projectID)
}

// Monitor reads one monitor.
func (u *Uptime) Monitor(ctx context.Context, id int64) (domain.UptimeMonitor, error) {
	return u.repo.FindMonitor(ctx, id)
}

// RemoveMonitor deletes a monitor and its history.
func (u *Uptime) RemoveMonitor(ctx context.Context, id int64) error {
	if err := u.repo.DeleteMonitor(ctx, id); err != nil {
		return err
	}
	// And removing the last one gives the goroutine back.
	u.monitorsChanged()
	return nil
}

// SetEnabled switches a monitor on or off without losing its history.
func (u *Uptime) SetEnabled(ctx context.Context, id int64, enabled bool) (domain.UptimeMonitor, error) {
	monitor, err := u.repo.SetMonitorEnabled(ctx, id, enabled)
	if err != nil {
		return domain.UptimeMonitor{}, err
	}
	u.monitorsChanged()
	return monitor, nil
}

// Results reads a monitor's recent checks, newest first.
func (u *Uptime) Results(ctx context.Context, monitorID, limit int64) ([]domain.CheckResult, error) {
	if _, err := u.repo.FindMonitor(ctx, monitorID); err != nil {
		return nil, err
	}
	return u.repo.Results(ctx, monitorID, int(limit))
}

// Daily reads the roll-up behind the status page's long timeline.
func (u *Uptime) Daily(ctx context.Context, monitorID int64, days int) ([]domain.UptimeDay, error) {
	if _, err := u.repo.FindMonitor(ctx, monitorID); err != nil {
		return nil, err
	}
	if days <= 0 {
		days = 90
	}
	now := u.clock.Now().UTC()
	return u.repo.DailyUptime(ctx, monitorID, now.AddDate(0, 0, -(days-1)), now)
}

// HasEnabledMonitors reports whether anything is being checked. This is what
// the scheduler asks before the job exists (ADR 014).
func (u *Uptime) HasEnabledMonitors(ctx context.Context) (bool, error) {
	return u.repo.HasEnabledMonitors(ctx)
}

// UptimeSweep is what one pass of the job did.
type UptimeSweep struct {
	// Checked is how many monitors were checked.
	Checked int
	// Failed is how many of those checks did not meet their expectations.
	Failed int
	// Transitions is how many monitors changed status.
	Transitions int
	// Queued is how many notifications were written.
	Queued int
	// Pruned is how many history rows were dropped.
	Pruned int64
}

// Job presents the sweep as a scheduled job.
//
// One job for every monitor rather than one job each. A job per monitor would
// mean a goroutine and a timer per monitor, growing without bound with
// whatever somebody configures; this is one goroutine that leases what is
// overdue and checks it with a bounded fan-out, and its cost is the same with
// one monitor or two hundred (ADR 014, ADR 016).
func (u *Uptime) Job() UptimeJob { return UptimeJob{uptime: u} }

// UptimeJob is the sweep seen through the scheduler's contract.
type UptimeJob struct{ uptime *Uptime }

// Name identifies the job wherever it is reported.
func (j UptimeJob) Name() string { return "uptime" }

// Interval is how often the job looks for work.
func (j UptimeJob) Interval() time.Duration { return UptimeTickInterval }

// Run performs one pass.
func (j UptimeJob) Run(ctx context.Context) error {
	result, err := j.uptime.Sweep(ctx)
	if err != nil {
		return err
	}
	if result.Transitions > 0 {
		j.uptime.log.Info("uptime status changed",
			"transitions", result.Transitions,
			"checked", result.Checked,
			"notifications", result.Queued,
		)
	}
	return nil
}

// Sweep checks every monitor that is due, with a bounded fan-out.
func (u *Uptime) Sweep(ctx context.Context) (UptimeSweep, error) {
	now := u.clock.Now()
	due, err := u.repo.DueMonitors(ctx, now, UptimeBatch)
	if err != nil {
		return UptimeSweep{}, err
	}

	result := UptimeSweep{}
	if len(due) > 0 {
		result = u.check(ctx, due, now)
	}

	pruned, err := u.prune(ctx, now)
	result.Pruned = pruned
	if err != nil {
		return result, err
	}
	return result, nil
}

// check runs the leased monitors concurrently and records what they found.
//
// The semaphore is the whole design. Checks are waiting, not working, so
// running them one at a time would make a pass take the sum of the timeouts;
// running all of them at once would open one socket per monitor on every tick.
// Eight at a time keeps a pass short and the cost flat.
//
// Errors from recording are logged rather than returned. One monitor whose
// write failed must not abandon the other forty-nine — and the scheduler's
// contract is that a failed pass is followed by another one, which would
// re-check everything rather than the one row that went wrong.
func (u *Uptime) check(ctx context.Context, due []domain.UptimeMonitor, now time.Time) UptimeSweep {
	var (
		mu      sync.Mutex
		result  UptimeSweep
		wait    sync.WaitGroup
		permits = make(chan struct{}, UptimeConcurrency)
	)

	for index := range due {
		monitor := &due[index]
		select {
		case permits <- struct{}{}:
		case <-ctx.Done():
			// Shutting down. What has already been checked is recorded; the
			// rest is still leased and becomes due again on schedule.
			wait.Wait()
			return result
		}

		wait.Add(1)
		go func() {
			defer wait.Done()
			defer func() { <-permits }()

			outcome := u.checkOne(ctx, monitor, now)

			mu.Lock()
			defer mu.Unlock()
			result.Checked++
			if !outcome.ok {
				result.Failed++
			}
			if outcome.transitioned {
				result.Transitions++
			}
			result.Queued += outcome.queued
		}()
	}

	wait.Wait()
	return result
}

// oneCheck is what a single check contributed to the pass.
type oneCheck struct {
	ok           bool
	transitioned bool
	queued       int
}

func (u *Uptime) checkOne(ctx context.Context, monitor *domain.UptimeMonitor, now time.Time) oneCheck {
	outcome := u.checker.Check(ctx, monitor, now)
	after, transition := monitor.Apply(outcome)

	queued, err := u.repo.RecordResult(ctx, monitor, &after, outcome, transition)
	if err != nil {
		u.log.Error("could not record a check",
			"monitor", monitor.ID, "name", monitor.Name, "error", err)
		return oneCheck{ok: outcome.OK}
	}

	if transition != domain.TransitionNone {
		u.log.Info("monitor "+string(transition),
			"monitor", monitor.ID,
			"name", monitor.Name,
			"url", monitor.URL,
			"status_code", outcome.StatusCode,
			"reason", outcome.Error,
			"notifications", queued,
		)
	}
	return oneCheck{ok: outcome.OK, transitioned: transition != domain.TransitionNone, queued: queued}
}

// prune drops history past its window, at most once an hour.
func (u *Uptime) prune(ctx context.Context, now time.Time) (int64, error) {
	u.mu.Lock()
	if !u.lastPrune.IsZero() && now.Sub(u.lastPrune) < uptimePruneInterval {
		u.mu.Unlock()
		return 0, nil
	}
	u.lastPrune = now
	u.mu.Unlock()

	results, err := u.repo.PruneResults(ctx, now.Add(-UptimeResultRetention), uptimePruneBatch)
	if err != nil {
		return results, fmt.Errorf("pruning uptime history: %w", err)
	}
	daily, err := u.repo.PruneDaily(ctx, now.Add(-UptimeDailyRetention), uptimePruneBatch)
	if err != nil {
		return results + daily, fmt.Errorf("pruning uptime aggregates: %w", err)
	}
	return results + daily, nil
}
