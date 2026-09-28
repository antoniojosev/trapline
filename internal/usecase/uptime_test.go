package usecase

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var uptimeNow = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// fakeUptimeRepo is the store, in memory. It keeps every write so a test can
// assert on the transition that was recorded rather than on the one the use
// case says it made.
type fakeUptimeRepo struct {
	mu       sync.Mutex
	monitors map[int64]domain.UptimeMonitor
	nextID   int64

	recorded    []recordedCheck
	prunedAfter time.Time
	pruneCalls  int
	failRecord  error
	failDue     error
}

type recordedCheck struct {
	monitorID  int64
	result     domain.CheckResult
	transition domain.UptimeTransition
	status     domain.UptimeStatus
}

func newFakeUptimeRepo() *fakeUptimeRepo {
	return &fakeUptimeRepo{monitors: map[int64]domain.UptimeMonitor{}}
}

func (f *fakeUptimeRepo) CreateMonitor(
	_ context.Context, monitor *domain.UptimeMonitor,
) (domain.UptimeMonitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	stored := *monitor
	stored.ID = f.nextID
	f.monitors[stored.ID] = stored
	return stored, nil
}

func (f *fakeUptimeRepo) ListMonitors(
	_ context.Context, projectID *int64,
) ([]domain.UptimeMonitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var monitors []domain.UptimeMonitor
	for _, monitor := range f.monitors {
		if projectID == nil || monitor.ProjectID == *projectID {
			monitors = append(monitors, monitor)
		}
	}
	return monitors, nil
}

func (f *fakeUptimeRepo) FindMonitor(_ context.Context, id int64) (domain.UptimeMonitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	monitor, ok := f.monitors[id]
	if !ok {
		return domain.UptimeMonitor{}, domain.ErrMonitorNotFound
	}
	return monitor, nil
}

func (f *fakeUptimeRepo) DeleteMonitor(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.monitors[id]; !ok {
		return domain.ErrMonitorNotFound
	}
	delete(f.monitors, id)
	return nil
}

func (f *fakeUptimeRepo) SetMonitorEnabled(
	_ context.Context, id int64, enabled bool,
) (domain.UptimeMonitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	monitor, ok := f.monitors[id]
	if !ok {
		return domain.UptimeMonitor{}, domain.ErrMonitorNotFound
	}
	monitor.Enabled = enabled
	f.monitors[id] = monitor
	return monitor, nil
}

func (f *fakeUptimeRepo) HasEnabledMonitors(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, monitor := range f.monitors {
		if monitor.Enabled {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeUptimeRepo) DueMonitors(
	_ context.Context, now time.Time, _ int,
) ([]domain.UptimeMonitor, error) {
	if f.failDue != nil {
		return nil, f.failDue
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var due []domain.UptimeMonitor
	for id, monitor := range f.monitors {
		if monitor.Enabled && !monitor.NextCheckAt.After(now) {
			due = append(due, monitor)
			// The lease, as the real store does it.
			monitor.NextCheckAt = now.Add(monitor.Interval())
			f.monitors[id] = monitor
		}
	}
	return due, nil
}

func (f *fakeUptimeRepo) RecordResult(
	_ context.Context, before, after *domain.UptimeMonitor,
	result domain.CheckResult, transition domain.UptimeTransition,
) (int, error) {
	if f.failRecord != nil {
		return 0, f.failRecord
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.monitors[before.ID] = *after
	f.recorded = append(f.recorded, recordedCheck{
		monitorID: before.ID, result: result, transition: transition, status: after.Status,
	})
	if transition == domain.TransitionNone {
		return 0, nil
	}
	return 1, nil
}

func (f *fakeUptimeRepo) Results(_ context.Context, _ int64, _ int) ([]domain.CheckResult, error) {
	return nil, nil
}

func (f *fakeUptimeRepo) DailyUptime(
	_ context.Context, _ int64, from, _ time.Time,
) ([]domain.UptimeDay, error) {
	return []domain.UptimeDay{{Day: from, Checks: 1}}, nil
}

func (f *fakeUptimeRepo) PruneResults(_ context.Context, cutoff time.Time, _ int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneCalls++
	f.prunedAfter = cutoff
	return 3, nil
}

func (f *fakeUptimeRepo) PruneDaily(_ context.Context, _ time.Time, _ int) (int64, error) {
	return 1, nil
}

func (f *fakeUptimeRepo) transitions() []domain.UptimeTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kinds []domain.UptimeTransition
	for _, check := range f.recorded {
		if check.transition != domain.TransitionNone {
			kinds = append(kinds, check.transition)
		}
	}
	return kinds
}

// scriptedChecker answers from a list, so a test drives a monitor through an
// outage without a server to switch off.
type scriptedChecker struct {
	mu      sync.Mutex
	replies []bool
	index   int
	// inFlight and peak measure the fan-out, which is the only way to assert
	// the semaphore exists.
	inFlight atomic.Int64
	peak     atomic.Int64
	delay    time.Duration
}

func (s *scriptedChecker) Check(
	_ context.Context, monitor *domain.UptimeMonitor, now time.Time,
) domain.CheckResult {
	current := s.inFlight.Add(1)
	for {
		peak := s.peak.Load()
		if current <= peak || s.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	defer s.inFlight.Add(-1)

	s.mu.Lock()
	ok := true
	if s.index < len(s.replies) {
		ok = s.replies[s.index]
		s.index++
	} else if len(s.replies) > 0 {
		ok = s.replies[len(s.replies)-1]
	}
	s.mu.Unlock()

	result := domain.CheckResult{At: now, OK: ok, StatusCode: 200, LatencyMS: 7}
	if !ok {
		result.StatusCode, result.Error = 503, "status 503 is outside the expected 200–299"
	}
	_ = monitor
	return result
}

// permissiveGuard allows everything, so a test of the state machine is not a
// test of DNS. The guard's own rules have their own exhaustive suite
// (internal/ssrfguard).
type permissiveGuard struct {
	err   error
	seen  []string
	calls int
}

func (g *permissiveGuard) ParseTarget(raw string) (*url.URL, error) {
	return url.Parse(raw) //nolint:wrapcheck // a fake, and the caller only checks for nil.
}

func (g *permissiveGuard) Check(_ context.Context, target *url.URL, allowPrivate bool) error {
	g.calls++
	g.seen = append(g.seen, target.String())
	_ = allowPrivate
	return g.err
}

func newUptimeUseCase(t *testing.T, checker ports.UptimeChecker) (*Uptime, *fakeUptimeRepo, *permissiveGuard) {
	t.Helper()
	repo := newFakeUptimeRepo()
	guard := &permissiveGuard{}
	return NewUptime(repo, checker, guard, fixedClock{now: uptimeNow}, nil, nil), repo, guard
}

func addMonitor(t *testing.T, uptime *Uptime) domain.UptimeMonitor {
	t.Helper()
	monitor, err := uptime.AddMonitor(context.Background(), &domain.UptimeMonitor{
		ProjectID: 1, Name: "api", URL: "https://api.example.com/health",
		IntervalSeconds: 30, Enabled: true,
	})
	if err != nil {
		t.Fatalf("AddMonitor: %v", err)
	}
	return monitor
}

// TestAddMonitorAsksTheGuard is the whole reason a refused target is a 422 the
// author reads rather than a check that quietly fails at 3am.
func TestAddMonitorAsksTheGuard(t *testing.T) {
	uptime, _, guard := newUptimeUseCase(t, &scriptedChecker{})
	addMonitor(t, uptime)

	if guard.calls != 1 {
		t.Fatalf("the guard was consulted %d times, want 1", guard.calls)
	}
	if guard.seen[0] != "https://api.example.com/health" {
		t.Errorf("the guard was asked about %q", guard.seen[0])
	}
}

func TestAddMonitorRefusesWhatTheGuardRefuses(t *testing.T) {
	uptime, repo, guard := newUptimeUseCase(t, &scriptedChecker{})
	guard.err = errors.New("169.254.169.254 is a link-local address")

	_, err := uptime.AddMonitor(context.Background(), &domain.UptimeMonitor{
		ProjectID: 1, Name: "metadata", URL: "http://169.254.169.254/latest/", Enabled: true,
	})
	if err == nil {
		t.Fatal("a refused target was stored")
	}
	if monitors, _ := repo.ListMonitors(context.Background(), nil); len(monitors) != 0 {
		t.Errorf("%d monitors were stored despite the refusal", len(monitors))
	}
}

func TestAddMonitorRefusesWhatTheDomainRefuses(t *testing.T) {
	uptime, _, guard := newUptimeUseCase(t, &scriptedChecker{})

	_, err := uptime.AddMonitor(context.Background(), &domain.UptimeMonitor{
		ProjectID: 1, Name: "too eager", URL: "https://api.example.com/", IntervalSeconds: 5,
	})
	if !errors.Is(err, domain.ErrInvalidMonitor) {
		t.Fatalf("an interval below the floor produced %v", err)
	}
	// And it never reached the network layer: an invalid monitor is not worth
	// a DNS lookup.
	if guard.calls != 0 {
		t.Errorf("the guard was consulted for a monitor the domain had already refused")
	}
}

// TestSweepDrivesTheStateMachine is the gate's scenario, without Docker: up,
// one failure that says nothing, a second that is the outage, then a recovery.
func TestSweepDrivesTheStateMachine(t *testing.T) {
	checker := &scriptedChecker{replies: []bool{true, false, false, true}}
	uptime, repo, _ := newUptimeUseCase(t, checker)
	monitor := addMonitor(t, uptime)

	for pass := range 4 {
		// Time moves by one interval per pass, so each pass finds the monitor
		// due exactly once.
		uptime.clock = fixedClock{now: uptimeNow.Add(time.Duration(pass) * monitor.Interval())}
		if _, err := uptime.Sweep(context.Background()); err != nil {
			t.Fatalf("Sweep %d: %v", pass, err)
		}
	}

	got := repo.transitions()
	want := []domain.UptimeTransition{domain.TransitionDown, domain.TransitionRecovered}
	if len(got) != len(want) {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("transitions = %v, want %v", got, want)
		}
	}

	// Four checks recorded, not two: the history keeps the failures that were
	// not yet an outage, which is what makes "it has been flapping all week"
	// answerable.
	if len(repo.recorded) != 4 {
		t.Errorf("recorded %d checks, want 4", len(repo.recorded))
	}
}

// TestSweepQueuesOnlyOnTransitions: a monitor that keeps working, or keeps
// failing, must not queue anything. Alerting on every check is the same as not
// alerting.
func TestSweepQueuesOnlyOnTransitions(t *testing.T) {
	checker := &scriptedChecker{replies: []bool{false, false, false, false, false}}
	uptime, _, _ := newUptimeUseCase(t, checker)
	monitor := addMonitor(t, uptime)

	total := 0
	for pass := range 5 {
		uptime.clock = fixedClock{now: uptimeNow.Add(time.Duration(pass) * monitor.Interval())}
		sweep, err := uptime.Sweep(context.Background())
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		total += sweep.Queued
	}
	if total != 1 {
		t.Errorf("five consecutive failures queued %d notifications, want 1", total)
	}
}

// TestSweepBoundsTheFanOut is the semaphore. Without it, an installation with
// two hundred monitors opens two hundred sockets on a tick.
func TestSweepBoundsTheFanOut(t *testing.T) {
	checker := &scriptedChecker{delay: 5 * time.Millisecond}
	uptime, _, _ := newUptimeUseCase(t, checker)

	for index := range 30 {
		if _, err := uptime.AddMonitor(context.Background(), &domain.UptimeMonitor{
			ProjectID: 1, Name: "api", URL: "https://api.example.com/health",
			IntervalSeconds: 30 + index, Enabled: true,
		}); err != nil {
			t.Fatalf("AddMonitor: %v", err)
		}
	}

	sweep, err := uptime.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if sweep.Checked != 30 {
		t.Fatalf("checked %d monitors, want 30", sweep.Checked)
	}
	if peak := checker.peak.Load(); peak > UptimeConcurrency {
		t.Errorf("%d checks ran at once, and the bound is %d", peak, UptimeConcurrency)
	}
	if peak := checker.peak.Load(); peak < 2 {
		t.Errorf("checks ran one at a time (peak %d); a pass would take the sum of the timeouts", peak)
	}
}

// TestSweepSkipsWhatIsNotDue: the lease is what stops the same monitor being
// checked twice in one interval.
func TestSweepSkipsWhatIsNotDue(t *testing.T) {
	uptime, repo, _ := newUptimeUseCase(t, &scriptedChecker{})
	addMonitor(t, uptime)

	for range 3 {
		if _, err := uptime.Sweep(context.Background()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}
	if len(repo.recorded) != 1 {
		t.Errorf("three passes at the same instant checked %d times, want 1", len(repo.recorded))
	}
}

// TestSweepSurvivesAWriteFailure: one monitor whose write failed must not
// abandon the rest of the pass.
func TestSweepSurvivesAWriteFailure(t *testing.T) {
	uptime, repo, _ := newUptimeUseCase(t, &scriptedChecker{})
	addMonitor(t, uptime)
	repo.failRecord = errors.New("disk is full")

	sweep, err := uptime.Sweep(context.Background())
	if err != nil {
		t.Fatalf("a failed write ended the whole pass: %v", err)
	}
	if sweep.Checked != 1 {
		t.Errorf("checked = %d", sweep.Checked)
	}
	if sweep.Transitions != 0 {
		t.Errorf("a check that was not recorded reported a transition")
	}
}

// TestSweepReportsALeaseFailure: failing to read what is due is a failure of
// the pass, and the scheduler is the thing that logs and counts it.
func TestSweepReportsALeaseFailure(t *testing.T) {
	uptime, repo, _ := newUptimeUseCase(t, &scriptedChecker{})
	repo.failDue = errors.New("database is locked")

	if _, err := uptime.Sweep(context.Background()); err == nil {
		t.Fatal("a store that could not be read reported a successful pass")
	}
}

// TestPruneRunsAtMostOnceAnHour keeps the sweep off the five-second path.
func TestPruneRunsAtMostOnceAnHour(t *testing.T) {
	uptime, repo, _ := newUptimeUseCase(t, &scriptedChecker{})

	for range 5 {
		if _, err := uptime.Sweep(context.Background()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}
	if repo.pruneCalls != 1 {
		t.Fatalf("five passes swept the history %d times, want 1", repo.pruneCalls)
	}
	if want := uptimeNow.Add(-UptimeResultRetention); !repo.prunedAfter.Equal(want) {
		t.Errorf("pruned before %s, want %s", repo.prunedAfter, want)
	}

	uptime.clock = fixedClock{now: uptimeNow.Add(2 * time.Hour)}
	if _, err := uptime.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if repo.pruneCalls != 2 {
		t.Errorf("the sweep never ran again after an hour: %d calls", repo.pruneCalls)
	}
}

// TestTheJobDoesNotExistWithoutMonitors is ADR 014 made checkable at this
// layer: the scheduler asks this before the goroutine is created at all.
func TestTheJobDoesNotExistWithoutMonitors(t *testing.T) {
	uptime, _, _ := newUptimeUseCase(t, &scriptedChecker{})

	has, err := uptime.HasEnabledMonitors(context.Background())
	if err != nil {
		t.Fatalf("HasEnabledMonitors: %v", err)
	}
	if has {
		t.Fatal("an installation with no monitor reports work to do")
	}

	monitor := addMonitor(t, uptime)
	if has, _ = uptime.HasEnabledMonitors(context.Background()); !has {
		t.Fatal("a monitor was added and the job still has nothing to do")
	}

	if _, err := uptime.SetEnabled(context.Background(), monitor.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if has, _ = uptime.HasEnabledMonitors(context.Background()); has {
		t.Fatal("the last monitor was switched off and the job still exists")
	}
}

// TestChangesWakeTheScheduler: the first monitor has to bring the job into
// existence while the person who added it is still looking, not at the next
// restart (ADR 014).
func TestChangesWakeTheScheduler(t *testing.T) {
	repo := newFakeUptimeRepo()
	woken := 0
	uptime := NewUptime(repo, &scriptedChecker{}, &permissiveGuard{},
		fixedClock{now: uptimeNow}, nil, func() { woken++ })

	monitor := addMonitor(t, uptime)
	if woken != 1 {
		t.Fatalf("adding a monitor woke the scheduler %d times", woken)
	}
	if _, err := uptime.SetEnabled(context.Background(), monitor.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if woken != 2 {
		t.Fatalf("switching a monitor off woke the scheduler %d times", woken)
	}
	if err := uptime.RemoveMonitor(context.Background(), monitor.ID); err != nil {
		t.Fatalf("RemoveMonitor: %v", err)
	}
	if woken != 3 {
		t.Fatalf("removing a monitor woke the scheduler %d times", woken)
	}
}

func TestReadsRefuseAMonitorThatDoesNotExist(t *testing.T) {
	uptime, _, _ := newUptimeUseCase(t, &scriptedChecker{})

	if _, err := uptime.Results(context.Background(), 7, 10); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Errorf("Results on nothing = %v", err)
	}
	if _, err := uptime.Daily(context.Background(), 7, 90); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Errorf("Daily on nothing = %v", err)
	}
	if _, err := uptime.Monitor(context.Background(), 7); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Errorf("Monitor on nothing = %v", err)
	}
	if err := uptime.RemoveMonitor(context.Background(), 7); !errors.Is(err, domain.ErrMonitorNotFound) {
		t.Errorf("RemoveMonitor on nothing = %v", err)
	}
}

func TestDailyDefaultsToNinetyDays(t *testing.T) {
	uptime, _, _ := newUptimeUseCase(t, &scriptedChecker{})
	monitor := addMonitor(t, uptime)

	days, err := uptime.Daily(context.Background(), monitor.ID, 0)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("Daily returned %d rows", len(days))
	}
	// The fake echoes the `from` it was given, so this asserts the window.
	if want := uptimeNow.AddDate(0, 0, -89); !days[0].Day.Equal(want) {
		t.Errorf("the window starts at %s, want %s", days[0].Day, want)
	}
}

// TestTheJobIsOneJob: not one per monitor. The name and the interval are what
// the scheduler and /system/jobs report.
func TestTheJobIsOneJob(t *testing.T) {
	uptime, _, _ := newUptimeUseCase(t, &scriptedChecker{})
	job := uptime.Job()

	if job.Name() != "uptime" {
		t.Errorf("the job is called %q", job.Name())
	}
	if job.Interval() != UptimeTickInterval {
		t.Errorf("the job ticks every %s", job.Interval())
	}
	addMonitor(t, uptime)
	if err := job.Run(context.Background()); err != nil {
		t.Errorf("Run: %v", err)
	}
}
