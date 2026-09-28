package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The scheduler decides whether something runs at all, so its tests are about
// absence as much as presence: the most important assertion in this file is
// that a subsystem with nothing to do leaves no goroutine behind.

// fakeJob records what it was asked to do.
type fakeJob struct {
	name     string
	interval time.Duration

	mu       sync.Mutex
	runs     int
	err      error
	panicNow bool
	ran      chan struct{}
}

func newFakeJob(name string, interval time.Duration) *fakeJob {
	return &fakeJob{name: name, interval: interval, ran: make(chan struct{}, 32)}
}

func (j *fakeJob) Name() string            { return j.name }
func (j *fakeJob) Interval() time.Duration { return j.interval }

func (j *fakeJob) Run(context.Context) error {
	j.mu.Lock()
	j.runs++
	err := j.err
	shouldPanic := j.panicNow
	j.mu.Unlock()

	select {
	case j.ran <- struct{}{}:
	default:
	}
	if shouldPanic {
		panic("a job exploded")
	}
	return err
}

func (j *fakeJob) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.runs
}

func (j *fakeJob) failWith(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.err = err
}

// waitChan is the injected replacement for time.After: it records the delays
// asked for and only releases a tick when the test says so, so no test in this
// file ever sleeps waiting for real time to pass.
type waitChan struct {
	mu     sync.Mutex
	delays []time.Duration
	ticks  chan time.Time
}

func newWaitChan() *waitChan { return &waitChan{ticks: make(chan time.Time)} }

func (w *waitChan) After(d time.Duration) <-chan time.Time {
	w.mu.Lock()
	w.delays = append(w.delays, d)
	w.mu.Unlock()
	return w.ticks
}

// tick releases one wait, failing the test if nothing is waiting.
func (w *waitChan) tick(t *testing.T) {
	t.Helper()
	select {
	case w.ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was waiting for a tick")
	}
}

func (w *waitChan) asked() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.delays...)
}

// newTestScheduler builds one that neither sleeps nor logs.
func newTestScheduler(t *testing.T, waits *waitChan) *Scheduler {
	t.Helper()
	return New(Options{
		After:  waits.After,
		Random: func() float64 { return 0.5 }, // no jitter, so delays are exact
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// waitFor polls until cond holds, so a test never guesses how long a goroutine
// needs to get to its next line.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

func TestASubsystemWithNoWorkGetsNoGoroutine(t *testing.T) {
	// The whole point of the package. Not "a goroutine that checks a flag and
	// sleeps" — none at all (ADR 005).
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("notifier", time.Hour)

	scheduler.Register(job, func(context.Context) (bool, error) { return false, nil })
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	if got := scheduler.Jobs(); len(got) != 0 {
		t.Errorf("Jobs() = %v, want nothing running", got)
	}
	if job.count() != 0 {
		t.Errorf("the job ran %d times without any work to do", job.count())
	}
	if delays := waits.asked(); len(delays) != 0 {
		t.Errorf("something is waiting on a timer: %v", delays)
	}
}

func TestASubsystemWithWorkRunsImmediately(t *testing.T) {
	// Work first, wait after: a server that was down for a week comes back
	// with a week of expired events, and sitting out a full interval before
	// noticing would be an odd first act.
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("notifier", time.Hour)

	scheduler.Register(job, func(context.Context) (bool, error) { return true, nil })
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	<-job.ran
	waitFor(t, "the first run to be recorded", func() bool {
		jobs := scheduler.Jobs()
		return len(jobs) == 1 && jobs[0].Runs == 1
	})

	status := scheduler.Jobs()[0]
	switch {
	case status.Name != "notifier":
		t.Errorf("Name = %q", status.Name)
	case status.Interval != time.Hour:
		t.Errorf("Interval = %s", status.Interval)
	case status.LastRun.IsZero():
		t.Error("LastRun is unset after a run")
	case !status.NextRun.After(status.LastRun):
		t.Errorf("NextRun %s is not after LastRun %s", status.NextRun, status.LastRun)
	case status.LastError != "":
		t.Errorf("LastError = %q on a clean run", status.LastError)
	}

	waits.tick(t)
	waitFor(t, "the second run", func() bool { return job.count() == 2 })
}

func TestRegisteredJobsRunAlwaysWhenWantsIsNil(t *testing.T) {
	// Retention's exception: nobody opts into having their disk not fill up.
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("retention", time.Hour)

	scheduler.Register(job, nil)
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	<-job.ran
	if got := scheduler.Jobs(); len(got) != 1 {
		t.Fatalf("Jobs() = %v, want retention running", got)
	}
}

func TestAConfigurationChangeStartsAJobWithoutARestart(t *testing.T) {
	// The reason the answer is re-asked at all: switching a subsystem on has
	// to take effect while the operator is still looking at the screen where
	// they switched it, not at the next restart.
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("notifier", time.Hour)

	var hasWork atomic.Bool
	scheduler.Register(job, func(context.Context) (bool, error) { return hasWork.Load(), nil })
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	if len(scheduler.Jobs()) != 0 {
		t.Fatal("the job started before there was anything to do")
	}

	hasWork.Store(true)
	scheduler.Reevaluate()

	<-job.ran
	waitFor(t, "the job to appear as running", func() bool { return len(scheduler.Jobs()) == 1 })
}

func TestASubsystemThatRunsOutOfWorkGivesItsGoroutineBack(t *testing.T) {
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("notifier", time.Hour)

	var hasWork atomic.Bool
	hasWork.Store(true)
	scheduler.Register(job, func(context.Context) (bool, error) { return hasWork.Load(), nil })
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	<-job.ran
	waitFor(t, "the job to be waiting", func() bool { return len(waits.asked()) == 1 })

	hasWork.Store(false)
	scheduler.Reevaluate()

	if got := scheduler.Jobs(); len(got) != 0 {
		t.Errorf("Jobs() = %v, want nothing running", got)
	}
	// And it stays gone: a tick released now must not produce another run.
	select {
	case waits.ticks <- time.Now():
	case <-time.After(200 * time.Millisecond):
	}
	if job.count() != 1 {
		t.Errorf("the job ran %d times after being switched off", job.count())
	}
}

func TestAFailingRunDoesNotStopTheSchedule(t *testing.T) {
	// A sweep can fail because a disk is full. Giving up quietly is how the
	// disk stays full with nobody having been told.
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("retention", time.Hour)
	wanted := errors.New("disk on fire")
	job.failWith(wanted)

	scheduler.Register(job, nil)
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	<-job.ran
	waitFor(t, "the failure to be recorded", func() bool {
		jobs := scheduler.Jobs()
		return len(jobs) == 1 && jobs[0].Failures == 1
	})
	if got := scheduler.Jobs()[0].LastError; got != wanted.Error() {
		t.Errorf("LastError = %q, want %q", got, wanted.Error())
	}

	// The next tick still happens, and a run that succeeds clears the error
	// rather than leaving a recovered job looking broken forever.
	job.failWith(nil)
	waits.tick(t)
	waitFor(t, "a clean run", func() bool {
		jobs := scheduler.Jobs()
		return len(jobs) == 1 && jobs[0].Runs == 2 && jobs[0].LastError == ""
	})
	if got := scheduler.Jobs()[0].Failures; got != 1 {
		t.Errorf("Failures = %d, want the historical count kept", got)
	}
}

func TestAPanickingJobIsAnErrorAndNotAnOutage(t *testing.T) {
	// An error tracker that dies because its own housekeeping hit a nil
	// pointer is a poor argument for itself.
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("retention", time.Hour)
	job.mu.Lock()
	job.panicNow = true
	job.mu.Unlock()

	scheduler.Register(job, nil)
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	<-job.ran
	waitFor(t, "the panic to be recorded as a failure", func() bool {
		jobs := scheduler.Jobs()
		return len(jobs) == 1 && jobs[0].Failures == 1
	})
	if got := scheduler.Jobs()[0].LastError; got == "" {
		t.Error("a panic left no error behind")
	}
}

func TestAFailedWantsLeavesTheJobAsItIs(t *testing.T) {
	// Reading "the database blinked" as "this subsystem has no work" would
	// switch something off for a reason that has nothing to do with it.
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("notifier", time.Hour)

	var fail atomic.Bool
	scheduler.Register(job, func(context.Context) (bool, error) {
		if fail.Load() {
			return false, errors.New("database is busy")
		}
		return true, nil
	})
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	<-job.ran
	fail.Store(true)
	scheduler.Reevaluate()

	if got := scheduler.Jobs(); len(got) != 1 {
		t.Errorf("Jobs() = %v, want the running job left alone", got)
	}
}

func TestJitterSpreadsTheDelayWithinTenPercent(t *testing.T) {
	// Without it every job registered at boot ticks in lockstep forever, and
	// they contend for the one SQLite writer this product has.
	scheduler := New(Options{Random: func() float64 { return 0 }})
	if got := scheduler.delay(time.Hour); got != 54*time.Minute {
		t.Errorf("earliest delay = %s, want 54m (an hour less 10%%)", got)
	}

	scheduler = New(Options{Random: func() float64 { return 1 }})
	if got := scheduler.delay(time.Hour); got != 66*time.Minute {
		t.Errorf("latest delay = %s, want 66m (an hour plus 10%%)", got)
	}

	scheduler = New(Options{Random: func() float64 { return 0.5 }})
	if got := scheduler.delay(time.Hour); got != time.Hour {
		t.Errorf("middle delay = %s, want the interval untouched", got)
	}
}

func TestJitterVariesWithTheDefaultRandomSource(t *testing.T) {
	// The seam is injected, so this is the one test that proves the shipped
	// scheduler actually jitters rather than being handed a fixed number.
	scheduler := New(Options{})
	seen := map[time.Duration]bool{}
	for range 50 {
		delay := scheduler.delay(time.Hour)
		if delay < 54*time.Minute || delay > 66*time.Minute {
			t.Fatalf("delay %s is outside ±10%% of an hour", delay)
		}
		seen[delay] = true
	}
	if len(seen) < 2 {
		t.Error("every delay was identical, so nothing is jittering")
	}
}

func TestJitterNeverProducesANonPositiveDelay(t *testing.T) {
	// A zero or negative wait is a busy loop. The guard is cheap and the
	// failure it prevents is a pegged core on someone else's server.
	scheduler := New(Options{Random: func() float64 { return 0 }})
	if got := scheduler.delay(time.Nanosecond); got <= 0 {
		t.Errorf("delay = %s, want something positive", got)
	}
}

func TestStartRefusesAWiringMistake(t *testing.T) {
	// These are conditions no runtime handling improves: a job ticking every
	// zero seconds is a busy loop, and two jobs with one name make the status
	// endpoint a lie. Better to refuse to boot.
	tests := []struct {
		name string
		jobs []Job
	}{
		{"no name", []Job{newFakeJob("", time.Hour)}},
		{"duplicate names", []Job{newFakeJob("a", time.Hour), newFakeJob("a", time.Hour)}},
		{"zero interval", []Job{newFakeJob("a", 0)}},
		{"negative interval", []Job{newFakeJob("a", -time.Second)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheduler := newTestScheduler(t, newWaitChan())
			for _, job := range test.jobs {
				scheduler.Register(job, nil)
			}
			if err := scheduler.Start(t.Context()); err == nil {
				scheduler.Stop()
				t.Fatal("Start accepted it")
			}
			if got := scheduler.Jobs(); len(got) != 0 {
				t.Errorf("Jobs() = %v, want nothing started", got)
			}
		})
	}
}

func TestStartIsIdempotent(t *testing.T) {
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("retention", time.Hour)
	scheduler.Register(job, nil)

	ctx := t.Context()
	if err := scheduler.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := scheduler.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer scheduler.Stop()

	<-job.ran
	waitFor(t, "the job to be waiting", func() bool { return len(waits.asked()) == 1 })
	if got := scheduler.Jobs(); len(got) != 1 {
		t.Errorf("Jobs() = %v, want one goroutine and not two", got)
	}
}

func TestRegisteringAfterStartConsidersTheJobImmediately(t *testing.T) {
	// Otherwise a subsystem that installs itself at runtime would sit idle
	// until somebody else happened to change a setting.
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	job := newFakeJob("late", time.Hour)
	scheduler.Register(job, nil)

	<-job.ran
	waitFor(t, "the late job to appear", func() bool { return len(scheduler.Jobs()) == 1 })
}

func TestNothingRunsBeforeStart(t *testing.T) {
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("retention", time.Hour)
	scheduler.Register(job, nil)

	scheduler.Reevaluate() // must be a no-op, not a way in through the back
	scheduler.Stop()       // and stopping something never started is not a panic

	if job.count() != 0 {
		t.Errorf("the job ran %d times before Start", job.count())
	}
	if got := scheduler.Jobs(); len(got) != 0 {
		t.Errorf("Jobs() = %v, want nothing", got)
	}
}

func TestStopWaitsForTheRunInFlight(t *testing.T) {
	// Shutdown is where a half-finished delete batch gets to finish rather
	// than being abandoned with the process.
	waits := newWaitChan()
	release := make(chan struct{})
	finished := make(chan struct{})
	scheduler := newTestScheduler(t, waits)

	scheduler.Register(blockingJob{
		interval: time.Hour,
		run: func() {
			<-release
			close(finished)
		},
	}, nil)
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		scheduler.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while a run was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	<-finished
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned")
	}
}

func TestStoppingEndsTheGoroutines(t *testing.T) {
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("retention", time.Hour)
	scheduler.Register(job, nil)

	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-job.ran
	waitFor(t, "the job to be waiting", func() bool { return len(waits.asked()) == 1 })

	scheduler.Stop()
	if got := scheduler.Jobs(); len(got) != 0 {
		t.Errorf("Jobs() = %v, want nothing after Stop", got)
	}
	// Stop is safe twice: shutdown paths run through defers.
	scheduler.Stop()
}

func TestACancelledContextStartsNothing(t *testing.T) {
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	job := newFakeJob("retention", time.Hour)
	scheduler.Register(job, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := scheduler.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	if job.count() != 0 {
		t.Errorf("the job ran %d times under a dead context", job.count())
	}
}

func TestJobsAreReportedByName(t *testing.T) {
	waits := newWaitChan()
	scheduler := newTestScheduler(t, waits)
	for _, name := range []string{"retention", "notifier", "uptime"} {
		scheduler.Register(newFakeJob(name, time.Hour), nil)
	}
	if err := scheduler.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer scheduler.Stop()

	waitFor(t, "all three to start", func() bool { return len(scheduler.Jobs()) == 3 })
	jobs := scheduler.Jobs()
	if jobs[0].Name != "notifier" || jobs[1].Name != "retention" || jobs[2].Name != "uptime" {
		t.Errorf("Jobs() = %v, want them sorted by name", jobs)
	}
}

// blockingJob runs whatever it is given, once per pass.
type blockingJob struct {
	interval time.Duration
	run      func()
}

func (j blockingJob) Name() string            { return "blocking" }
func (j blockingJob) Interval() time.Duration { return j.interval }
func (j blockingJob) Run(context.Context) error {
	j.run()
	return nil
}
