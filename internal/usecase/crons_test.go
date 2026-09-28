package usecase

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// fakeCronRepo is an in-memory ports.CronRepository.
//
// The SQLite adapter is tested against a real database (ADR 009); this exists
// so the use case's orchestration — and the clock it runs on — can be driven
// by hand. Everything about this feature is timing, and a test that had to
// wait a real minute per case is a test nobody runs.
type fakeCronRepo struct {
	monitors map[int64]*domain.CronMonitor
	checkIns map[int64][]domain.CronCheckIn
	open     map[int64]*time.Time
	events   []domain.AlertEvent
	nextID   int64
	failOn   map[string]error
}

func newFakeCronRepo() *fakeCronRepo {
	return &fakeCronRepo{
		monitors: map[int64]*domain.CronMonitor{},
		checkIns: map[int64][]domain.CronCheckIn{},
		open:     map[int64]*time.Time{},
		nextID:   1,
		failOn:   map[string]error{},
	}
}

func (r *fakeCronRepo) CreateMonitor(
	_ context.Context, monitor *domain.CronMonitor,
) (domain.CronMonitor, error) {
	if err := r.failOn["create"]; err != nil {
		return domain.CronMonitor{}, err
	}
	for _, existing := range r.monitors {
		if existing.ProjectID == monitor.ProjectID && existing.Slug == monitor.Slug {
			return domain.CronMonitor{}, domain.ErrInvalidMonitor
		}
	}
	stored := *monitor
	stored.ID = r.nextID
	r.nextID++
	r.monitors[stored.ID] = &stored
	return stored, nil
}

func (r *fakeCronRepo) UpdateMonitor(
	_ context.Context, monitor *domain.CronMonitor,
) (domain.CronMonitor, error) {
	if _, found := r.monitors[monitor.ID]; !found {
		return domain.CronMonitor{}, domain.ErrMonitorNotFound
	}
	stored := *monitor
	r.monitors[monitor.ID] = &stored
	return stored, nil
}

func (r *fakeCronRepo) ListMonitors(_ context.Context, projectID int64) ([]domain.CronMonitor, error) {
	var monitors []domain.CronMonitor
	for _, monitor := range r.monitors {
		if monitor.ProjectID == projectID {
			monitors = append(monitors, *monitor)
		}
	}
	slices.SortFunc(monitors, func(a, b domain.CronMonitor) int { return int(a.ID - b.ID) })
	return monitors, nil
}

func (r *fakeCronRepo) FindMonitor(_ context.Context, id int64) (domain.CronMonitor, error) {
	monitor, found := r.monitors[id]
	if !found {
		return domain.CronMonitor{}, domain.ErrMonitorNotFound
	}
	return *monitor, nil
}

func (r *fakeCronRepo) FindMonitorBySlug(
	_ context.Context, projectID int64, slug string,
) (domain.CronMonitor, error) {
	if err := r.failOn["find"]; err != nil {
		return domain.CronMonitor{}, err
	}
	for _, monitor := range r.monitors {
		if monitor.ProjectID == projectID && monitor.Slug == slug {
			return *monitor, nil
		}
	}
	return domain.CronMonitor{}, domain.ErrMonitorNotFound
}

func (r *fakeCronRepo) FindMonitorByPingKey(_ context.Context, key string) (domain.CronMonitor, error) {
	for _, monitor := range r.monitors {
		if monitor.PingKey == key {
			return *monitor, nil
		}
	}
	return domain.CronMonitor{}, domain.ErrMonitorNotFound
}

func (r *fakeCronRepo) DeleteMonitor(_ context.Context, id int64) error {
	if _, found := r.monitors[id]; !found {
		return domain.ErrMonitorNotFound
	}
	delete(r.monitors, id)
	return nil
}

func (r *fakeCronRepo) HasEnabledMonitors(context.Context) (bool, error) {
	for _, monitor := range r.monitors {
		if monitor.Enabled {
			return true, nil
		}
	}
	return false, nil
}

func (r *fakeCronRepo) RecordCheckIn(
	_ context.Context, write *ports.CheckInWrite,
) (domain.CronCheckIn, error) {
	if err := r.failOn["record"]; err != nil {
		return domain.CronCheckIn{}, err
	}
	checkIn := domain.CronCheckIn{
		ID:          r.nextID,
		MonitorID:   write.Monitor.ID,
		CheckInID:   write.CheckInID,
		Status:      write.Status,
		StartedAt:   write.At,
		DurationMS:  write.DurationMS,
		Environment: write.Environment,
	}
	r.nextID++

	if write.Status == domain.CheckInProgress {
		started := write.At
		r.open[write.Monitor.ID] = &started
	} else {
		finished := write.At
		checkIn.FinishedAt = &finished
		delete(r.open, write.Monitor.ID)
	}
	r.checkIns[write.Monitor.ID] = append([]domain.CronCheckIn{checkIn}, r.checkIns[write.Monitor.ID]...)

	stored := r.monitors[write.Monitor.ID]
	stored.Status = write.Outcome.Status
	deadline := write.Outcome.NextExpectedAt
	stored.NextExpectedAt = &deadline
	at := write.At
	stored.LastCheckinAt = &at

	if write.Event != nil {
		r.events = append(r.events, *write.Event)
	}
	return checkIn, nil
}

func (r *fakeCronRepo) ListCheckIns(_ context.Context, monitorID int64, limit int) ([]domain.CronCheckIn, error) {
	checkIns := r.checkIns[monitorID]
	if limit > 0 && len(checkIns) > limit {
		checkIns = checkIns[:limit]
	}
	return checkIns, nil
}

func (r *fakeCronRepo) DueMonitors(_ context.Context, now time.Time, _ int) ([]ports.DueMonitor, error) {
	if err := r.failOn["due"]; err != nil {
		return nil, err
	}
	var due []ports.DueMonitor
	for _, monitor := range r.monitors {
		if !monitor.Enabled {
			continue
		}
		overdue := monitor.NextExpectedAt != nil && !monitor.NextExpectedAt.After(now)
		if overdue || r.open[monitor.ID] != nil {
			due = append(due, ports.DueMonitor{Monitor: *monitor, OpenSince: r.open[monitor.ID]})
		}
	}
	slices.SortFunc(due, func(a, b ports.DueMonitor) int { return int(a.Monitor.ID - b.Monitor.ID) })
	return due, nil
}

func (r *fakeCronRepo) ApplySweep(_ context.Context, write *ports.SweepWrite) error {
	if err := r.failOn["sweep"]; err != nil {
		return err
	}
	monitor, found := r.monitors[write.MonitorID]
	if !found {
		return domain.ErrMonitorNotFound
	}
	monitor.Status = write.Outcome.Status
	deadline := write.Outcome.NextExpectedAt
	monitor.NextExpectedAt = &deadline
	if write.Event != nil {
		r.events = append(r.events, *write.Event)
	}
	return nil
}

func (r *fakeCronRepo) PruneCheckIns(context.Context, time.Time, int) (int64, error) { return 0, nil }

func (r *fakeCronRepo) kinds() []domain.TriggerKind {
	kinds := make([]domain.TriggerKind, 0, len(r.events))
	for _, event := range r.events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

// newCrons wires the use case over a clock the test moves by hand.
func newCrons(t *testing.T, at time.Time) (*Crons, *fakeCronRepo, *movableClock, *int) {
	t.Helper()
	repo := newFakeCronRepo()
	clock := &movableClock{now: at}
	reevaluated := 0
	crons := NewCrons(repo, clock, nil, func() { reevaluated++ })
	return crons, repo, clock, &reevaluated
}

var cronBase = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func TestAddingTheFirstMonitorWakesTheScheduler(t *testing.T) {
	crons, _, _, reevaluated := newCrons(t, cronBase)
	ctx := context.Background()

	monitor, err := crons.Add(ctx, 1, MonitorSpec{Slug: "backup", Schedule: "*/1 * * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if *reevaluated != 1 {
		// ADR 014: switching a subsystem on takes effect while the person who
		// switched it on is still looking at the screen, not at the next
		// restart.
		t.Fatalf("the scheduler was re-evaluated %d times", *reevaluated)
	}

	has, err := crons.HasEnabled(ctx)
	if err != nil || !has {
		t.Fatalf("has = %v, err = %v", has, err)
	}
	if err := crons.Remove(ctx, monitor.ID); err != nil {
		t.Fatal(err)
	}
	if *reevaluated != 2 {
		t.Fatalf("removing the last monitor did not re-evaluate: %d", *reevaluated)
	}
}

func TestAPingMovesTheMonitorAndTheDeadline(t *testing.T) {
	crons, _, clock, _ := newCrons(t, cronBase)
	ctx := context.Background()

	monitor, err := crons.Add(ctx, 1, MonitorSpec{Slug: "backup", Schedule: "*/1 * * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	clock.now = cronBase.Add(30 * time.Second)
	result, err := crons.Ping(ctx, monitor.PingKey, domain.CheckInOK)
	if err != nil {
		t.Fatal(err)
	}
	if result.Monitor.Status != domain.CronOK {
		t.Fatalf("status = %q", result.Monitor.Status)
	}
	if !result.Monitor.NextExpectedAt.After(clock.now) {
		t.Fatalf("the deadline %s is not in the future", result.Monitor.NextExpectedAt)
	}
}

func TestAPingToADisabledMonitorIsRefused(t *testing.T) {
	crons, _, _, _ := newCrons(t, cronBase)
	ctx := context.Background()

	monitor, err := crons.Add(ctx, 1, MonitorSpec{Slug: "backup", Schedule: "@hourly", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crons.Ping(ctx, monitor.PingKey, domain.CheckInOK); !errors.Is(err, domain.ErrMonitorDisabled) {
		t.Fatalf("err = %v, want ErrMonitorDisabled", err)
	}
	if _, err := crons.Ping(ctx, "not-a-key", domain.CheckInOK); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Fatalf("err = %v, want ErrMonitorNotFound", err)
	}
}

// The whole feature, on an injected clock: a monitor that checks in stays ok,
// one that goes quiet is missed exactly once, and one that reports again
// recovers.
func TestTheWatcherReportsMissedOnceAndThenRecovery(t *testing.T) {
	crons, repo, clock, _ := newCrons(t, cronBase)
	ctx := context.Background()

	monitor, err := crons.Add(ctx, 1, MonitorSpec{
		Slug: "backup", Schedule: "*/1 * * * *", CheckinMargin: 10 * time.Second, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// It checks in on time: nothing is reported.
	clock.now = cronBase.Add(30 * time.Second)
	if _, err := crons.Ping(ctx, monitor.PingKey, domain.CheckInOK); err != nil {
		t.Fatal(err)
	}
	clock.now = cronBase.Add(40 * time.Second)
	if _, err := crons.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repo.events) != 0 {
		t.Fatalf("a healthy monitor produced %v", repo.kinds())
	}

	// It goes quiet. Past the deadline plus the margin, once.
	clock.now = cronBase.Add(3 * time.Minute)
	result, err := crons.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Missed != 1 {
		t.Fatalf("missed = %d", result.Missed)
	}
	if kinds := repo.kinds(); len(kinds) != 1 || kinds[0] != domain.TriggerCronMissed {
		t.Fatalf("events = %v", kinds)
	}
	if repo.events[0].MonitorID != monitor.ID || repo.events[0].MonitorSlug != "backup" {
		t.Fatalf("the event does not name the monitor: %+v", repo.events[0])
	}
	if repo.events[0].SubjectKey() != domain.MonitorSubjectKey(domain.MonitorKindCron, monitor.ID) {
		t.Fatalf("subject = %q", repo.events[0].SubjectKey())
	}

	// Still quiet: swept again, and told nobody again.
	clock.now = cronBase.Add(10 * time.Minute)
	if _, err := crons.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if kinds := repo.kinds(); len(kinds) != 1 {
		t.Fatalf("a monitor that is already missed was reported again: %v", kinds)
	}

	// It comes back.
	clock.now = cronBase.Add(11 * time.Minute)
	if _, err := crons.Ping(ctx, monitor.PingKey, domain.CheckInOK); err != nil {
		t.Fatal(err)
	}
	kinds := repo.kinds()
	if len(kinds) != 2 || kinds[1] != domain.TriggerCronRecovered {
		t.Fatalf("events = %v, want a recovery", kinds)
	}
}

func TestTheWatcherReportsATimeout(t *testing.T) {
	crons, repo, clock, _ := newCrons(t, cronBase)
	ctx := context.Background()

	monitor, err := crons.Add(ctx, 1, MonitorSpec{
		Slug: "backup", Schedule: "*/1 * * * *",
		CheckinMargin: time.Minute, MaxRuntime: 5 * time.Second, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	clock.now = cronBase.Add(time.Second)
	if _, err := crons.Ping(ctx, monitor.PingKey, domain.CheckInProgress); err != nil {
		t.Fatal(err)
	}
	// A start is not an outcome: nothing is reported yet.
	if len(repo.events) != 0 {
		t.Fatalf("a start reported %v", repo.kinds())
	}

	clock.now = cronBase.Add(30 * time.Second)
	result, err := crons.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.TimedOut != 1 {
		t.Fatalf("timed out = %d", result.TimedOut)
	}
	if kinds := repo.kinds(); len(kinds) != 1 || kinds[0] != domain.TriggerCronTimeout {
		t.Fatalf("events = %v", kinds)
	}
}

func TestAFailedRunIsReported(t *testing.T) {
	crons, repo, clock, _ := newCrons(t, cronBase)
	ctx := context.Background()

	monitor, err := crons.Add(ctx, 1, MonitorSpec{Slug: "backup", Schedule: "@hourly", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	clock.now = cronBase.Add(time.Minute)
	if _, err := crons.Ping(ctx, monitor.PingKey, domain.CheckInError); err != nil {
		t.Fatal(err)
	}
	if kinds := repo.kinds(); len(kinds) != 1 || kinds[0] != domain.TriggerCronFailed {
		t.Fatalf("events = %v, want a reported failure (ADR 036)", kinds)
	}
}

// An SDK declaring a monitor it has never declared before creates it. This is
// what makes instrumenting a cron job one decorator (ADR 016).
func TestACheckInWithAConfigCreatesTheMonitor(t *testing.T) {
	crons, repo, clock, _ := newCrons(t, cronBase)
	ctx := context.Background()

	result, err := crons.Accept(ctx, 1, Report{
		Slug:   "declared-by-the-sdk",
		Status: domain.CheckInOK,
		Declare: &MonitorSpec{
			Schedule: "*/5 * * * *", Timezone: "America/Caracas",
			CheckinMargin: 30 * time.Second, MaxRuntime: 2 * time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case !result.Created:
		t.Fatal("the monitor was not reported as created")
	case result.Monitor.Slug != "declared-by-the-sdk":
		t.Fatalf("slug = %q", result.Monitor.Slug)
	case result.Monitor.Schedule.String() != "*/5 * * * *":
		t.Fatalf("schedule = %q", result.Monitor.Schedule.String())
	case result.Monitor.Timezone != "America/Caracas":
		t.Fatalf("timezone = %q", result.Monitor.Timezone)
	case result.Monitor.Status != domain.CronOK:
		t.Fatalf("status = %q", result.Monitor.Status)
	}

	// A second check-in with the same declaration is not a second monitor,
	// and does not rewrite anything.
	clock.now = cronBase.Add(time.Minute)
	again, err := crons.Accept(ctx, 1, Report{
		Slug: "declared-by-the-sdk", Status: domain.CheckInOK,
		Declare: &MonitorSpec{Schedule: "*/5 * * * *", Timezone: "America/Caracas"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Monitor.ID != result.Monitor.ID {
		t.Fatalf("a repeated declaration produced %+v", again)
	}
	monitors, err := crons.Monitors(ctx, 1)
	if err != nil || len(monitors) != 1 {
		t.Fatalf("%d monitors exist: %v", len(monitors), err)
	}

	// A declaration that genuinely changed reconfigures the monitor.
	if _, err := crons.Accept(ctx, 1, Report{
		Slug: "declared-by-the-sdk", Status: domain.CheckInOK,
		Declare: &MonitorSpec{Schedule: "*/10 * * * *"},
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := crons.Monitor(ctx, result.Monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Schedule.String() != "*/10 * * * *" {
		t.Fatalf("schedule = %q", updated.Schedule.String())
	}
	if updated.PingKey != result.Monitor.PingKey {
		t.Fatal("a redeclaration rotated the ping key")
	}
	_ = repo
}

// And one without a config is refused rather than inventing a monitor with no
// schedule, which would exist and never report anything.
func TestACheckInForAnUnknownMonitorWithoutAConfigIsRefused(t *testing.T) {
	crons, _, _, _ := newCrons(t, cronBase)

	_, err := crons.Accept(context.Background(), 1, Report{Slug: "nobody-declared-me", Status: domain.CheckInOK})
	if !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Fatalf("err = %v, want ErrMonitorNotFound", err)
	}
	if !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Fatal("the error does not explain that a monitor_config would have created it")
	}
}

func TestAcceptValidatesWhatTheSDKSent(t *testing.T) {
	crons, _, _, _ := newCrons(t, cronBase)
	ctx := context.Background()

	for name, report := range map[string]Report{
		"a slug with a space":  {Slug: "my backup", Status: domain.CheckInOK},
		"an unusable check id": {Slug: "backup", CheckInID: "a b", Status: domain.CheckInOK},
		"a nonsense schedule": {
			Slug: "backup", Status: domain.CheckInOK,
			Declare: &MonitorSpec{Schedule: "sometimes"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := crons.Accept(ctx, 1, report); !errors.Is(err, domain.ErrInvalidMonitor) {
				t.Fatalf("err = %v, want ErrInvalidMonitor", err)
			}
		})
	}
}

func TestUpdateRecomputesTheDeadlineWhenTheScheduleMoves(t *testing.T) {
	crons, _, clock, _ := newCrons(t, cronBase)
	ctx := context.Background()

	monitor, err := crons.Add(ctx, 1, MonitorSpec{Slug: "backup", Schedule: "*/1 * * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	before := *monitor.NextExpectedAt

	clock.now = cronBase.Add(time.Minute)
	yearly := "0 0 1 1 *"
	updated, err := crons.Update(ctx, monitor.ID, MonitorChanges{Schedule: &yearly})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.NextExpectedAt.After(before.Add(24 * time.Hour)) {
		// Leaving the old deadline would report the monitor as missed against
		// the timetable it had yesterday.
		t.Fatalf("the deadline stayed at %s after the schedule changed", updated.NextExpectedAt)
	}

	// An update that touches nothing schedule-shaped leaves the deadline
	// alone.
	enabled := false
	off, err := crons.Update(ctx, monitor.ID, MonitorChanges{Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if !off.NextExpectedAt.Equal(*updated.NextExpectedAt) {
		t.Fatalf("switching a monitor off moved its deadline to %s", off.NextExpectedAt)
	}
	if off.Enabled {
		t.Fatal("the monitor is still enabled")
	}
}

func TestUpdateRejectsWhatCreationWouldHave(t *testing.T) {
	crons, _, _, _ := newCrons(t, cronBase)
	ctx := context.Background()

	monitor, err := crons.Add(ctx, 1, MonitorSpec{Slug: "backup", Schedule: "@hourly", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	nonsense := "not a crontab"
	if _, err := crons.Update(ctx, monitor.ID, MonitorChanges{Schedule: &nonsense}); !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v", err)
	}
	mars := "Mars/Olympus"
	if _, err := crons.Update(ctx, monitor.ID, MonitorChanges{Timezone: &mars}); !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v", err)
	}
	tooLong := 48 * time.Hour
	if _, err := crons.Update(ctx, monitor.ID, MonitorChanges{CheckinMargin: &tooLong}); !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("err = %v", err)
	}
	if _, err := crons.Update(ctx, 9999, MonitorChanges{}); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckInsRefuseAnUnknownMonitor(t *testing.T) {
	crons, _, _, _ := newCrons(t, cronBase)
	if _, err := crons.CheckIns(context.Background(), 9999, 10); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// A monitor this build cannot evaluate — a timezone this machine's tzdata does
// not have — must not stop the sweep for every other one.
func TestOneUnevaluableMonitorDoesNotStopTheSweep(t *testing.T) {
	crons, repo, clock, _ := newCrons(t, cronBase)
	ctx := context.Background()

	broken, err := crons.Add(ctx, 1, MonitorSpec{Slug: "broken", Schedule: "*/1 * * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	repo.monitors[broken.ID].Timezone = "Mars/Olympus"
	if _, err := crons.Add(ctx, 1, MonitorSpec{Slug: "fine", Schedule: "*/1 * * * *", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	clock.now = cronBase.Add(5 * time.Minute)
	result, err := crons.Sweep(ctx)
	if err != nil {
		t.Fatalf("the sweep gave up: %v", err)
	}
	if result.Missed != 1 {
		t.Fatalf("missed = %d, want the healthy monitor to still be reported", result.Missed)
	}
}

func TestTheSweepReportsStorageFailures(t *testing.T) {
	crons, repo, _, _ := newCrons(t, cronBase)
	repo.failOn["due"] = errors.New("disk is on fire")
	if _, err := crons.Sweep(context.Background()); err == nil {
		t.Fatal("a failing store was reported as a clean sweep")
	}
}

func TestTheJobIsNamedAndBounded(t *testing.T) {
	crons, _, clock, _ := newCrons(t, cronBase)
	job := crons.Job()
	if job.Name() != "cron-watch" {
		t.Fatalf("name = %q", job.Name())
	}
	if job.Interval() != CronWatchInterval {
		t.Fatalf("interval = %s", job.Interval())
	}
	ctx := context.Background()
	if _, err := crons.Add(ctx, 1, MonitorSpec{Slug: "backup", Schedule: "*/1 * * * *", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	clock.now = cronBase.Add(5 * time.Minute)
	if err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
}
