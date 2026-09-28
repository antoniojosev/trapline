// Package scheduler runs the product's background jobs.
//
// It exists to make ADR 005 enforceable rather than aspirational. A subsystem
// that is switched off gets no goroutine — not one that wakes up, reads a flag
// and goes back to sleep, but none at all. So a job is not started, it is
// *offered*: registered together with the question "does this subsystem have
// any work?", and only the ones that answer yes are ever run.
//
// The question is asked when the server boots and again whenever configuration
// changes, so switching a subsystem on takes effect without a restart and
// switching it off gives the goroutine back.
//
// Only the standard library, and no import from the rest of the repo: a job is
// anything with a name, an interval and a Run, which is why the use cases can
// satisfy this contract without knowing this package exists.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
)

// jitterFraction spreads ticks by up to ±10 %.
//
// Without it every job registered at boot ticks in lockstep for the life of
// the process, and they contend for the one thing this product has exactly one
// of: the SQLite writer. The spread is per tick rather than a one-off offset,
// so two jobs that happen to land together do not stay together.
const jitterFraction = 0.10

// Job is a piece of work that runs on an interval.
//
// Deliberately three methods with no lifecycle: a job cannot be started,
// stopped, paused or reconfigured, so there is no state in it for the
// scheduler and the scheduler to disagree about. Everything else is decided
// here.
type Job interface {
	// Name identifies the job to an operator, and must be unique.
	Name() string
	// Interval is how long to wait between runs. It is read once, when the
	// job starts.
	Interval() time.Duration
	// Run does one pass. Returning an error is normal and expected — a sweep
	// can fail because a disk is full — and never stops the schedule.
	Run(ctx context.Context) error
}

// Wants reports whether a job's subsystem currently has anything to do.
//
// A nil Wants means "always", which is the exception and not the rule: it is
// for the one subsystem nobody opts into (retention). Everything else answers
// this by asking storage — are there any channels, any monitors — so that
// installing the product does not cost anything the installation is not using.
type Wants func(ctx context.Context) (bool, error)

// Options are the seams a test needs. Every field is optional.
type Options struct {
	// Now is the clock. Defaults to time.Now in UTC.
	Now func() time.Time
	// After is how the scheduler waits between runs. Defaults to time.After;
	// a test replaces it to drive time by hand instead of sleeping.
	After func(time.Duration) <-chan time.Time
	// Random returns a value in [0,1) and decides the jitter. Injected so a
	// test can prove the jitter exists rather than hope for it.
	Random func() float64
	// Logger defaults to slog's.
	Logger *slog.Logger
}

// Status is what one running job looks like from outside.
//
// Plain Go types, no JSON tags: this package is the source of the numbers, and
// how they are shown is the business of whichever adapter is showing them
// (ADR 006).
type Status struct {
	Name     string
	Interval time.Duration
	// StartedAt is when the job's goroutine began, which is not when the
	// process did if the subsystem was switched on later.
	StartedAt time.Time
	// LastRun is the zero time until the job has finished a pass.
	LastRun time.Time
	// NextRun is when the current wait expires, jitter included.
	NextRun time.Time
	// LastError is the message from the most recent failed pass, and is
	// cleared by a pass that succeeds. A stale error would make a job that
	// recovered look broken forever.
	LastError string
	Runs      int64
	Failures  int64
}

// Scheduler owns the background goroutines.
type Scheduler struct {
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time
	random func() float64
	log    *slog.Logger

	mu         sync.Mutex
	candidates []*candidate
	// ctx is what Start was given. Held rather than passed around because a
	// re-evaluation triggered by a configuration write has to start a job
	// under the server's lifetime, not under the lifetime of the HTTP request
	// that happened to change the configuration.
	ctx     context.Context
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

type candidate struct {
	job   Job
	wants Wants

	running bool
	// generation tells one run of this job from the next, so a goroutine that
	// is still finishing its pass after being switched off cannot clear the
	// state of the one that replaced it.
	generation int64
	cancel     context.CancelFunc
	status     Status
}

// New builds a scheduler. Nothing runs until Start.
func New(options Options) *Scheduler {
	s := &Scheduler{
		now:    options.Now,
		after:  options.After,
		random: options.Random,
		log:    options.Logger,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.after == nil {
		s.after = time.After
	}
	if s.random == nil {
		// Not crypto/rand: this decides when a sweep runs, not a secret.
		s.random = rand.Float64 //nolint:gosec // jitter, not a credential
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Register offers a job. It does not start it: whether it ever runs is decided
// by wants, at Start and at every re-evaluation after it.
func (s *Scheduler) Register(job Job, wants Wants) {
	s.mu.Lock()
	s.candidates = append(s.candidates, &candidate{job: job, wants: wants})
	started := s.started
	ctx := s.ctx
	added := s.candidates[len(s.candidates)-1]
	s.mu.Unlock()

	// Registering after the server is up is legitimate — a subsystem that
	// installs itself at runtime — and it should not have to wait for
	// somebody else's configuration change to be considered.
	if started {
		s.evaluate(ctx, added)
	}
}

// Start evaluates every candidate and launches the ones with work.
//
// It returns an error only for a wiring mistake — two jobs with one name, an
// interval that is not positive — because those are conditions no amount of
// runtime handling improves: a job that ticks every zero seconds is a busy
// loop, and two jobs with one name make the status endpoint a lie. The server
// refuses to boot instead.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	seen := make(map[string]bool, len(s.candidates))
	for _, c := range s.candidates {
		name := c.job.Name()
		switch {
		case name == "":
			s.mu.Unlock()
			return errors.New("scheduler: a job was registered without a name")
		case seen[name]:
			s.mu.Unlock()
			return fmt.Errorf("scheduler: two jobs are called %q", name)
		case c.job.Interval() <= 0:
			s.mu.Unlock()
			return fmt.Errorf("scheduler: job %q has a non-positive interval (%s)", name, c.job.Interval())
		}
		seen[name] = true
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.ctx, s.cancel, s.started = runCtx, cancel, true
	candidates := make([]*candidate, len(s.candidates))
	copy(candidates, s.candidates)
	s.mu.Unlock()

	for _, c := range candidates {
		s.evaluate(runCtx, c)
	}
	return nil
}

// Reevaluate asks every candidate again whether its subsystem has work, and
// starts or stops goroutines accordingly.
//
// This is what a configuration write calls. It is deliberately cheap and
// non-blocking for the caller: switching a subsystem off cancels its context
// and returns, rather than waiting for a sweep that may be mid-batch, because
// the caller is an HTTP request and nobody's PUT should hang on somebody
// else's disk.
func (s *Scheduler) Reevaluate() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	ctx := s.ctx
	candidates := make([]*candidate, len(s.candidates))
	copy(candidates, s.candidates)
	s.mu.Unlock()

	for _, c := range candidates {
		s.evaluate(ctx, c)
	}
}

// evaluate starts or stops one candidate.
func (s *Scheduler) evaluate(ctx context.Context, c *candidate) {
	if ctx.Err() != nil {
		return
	}

	wanted := true
	if c.wants != nil {
		answer, err := c.wants(ctx)
		if err != nil {
			// Asking whether there is work failed. Whatever is running keeps
			// running: reading the failure as "no work" would switch a
			// subsystem off because the database blinked, which is a far
			// worse outcome than one extra idle tick.
			s.log.Error("could not decide whether a job has work",
				"job", c.job.Name(), "error", err)
			return
		}
		wanted = answer
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case wanted && !c.running:
		jobCtx, cancel := context.WithCancel(ctx)
		c.generation++
		c.running, c.cancel = true, cancel
		c.status = Status{
			Name:      c.job.Name(),
			Interval:  c.job.Interval(),
			StartedAt: s.now(),
		}
		s.wg.Add(1)
		go s.run(jobCtx, c, c.generation)
		s.log.Info("job started", "job", c.job.Name(), "interval", c.job.Interval().String())

	case !wanted && c.running:
		c.cancel()
		c.running = false
		s.log.Info("job stopped: its subsystem has nothing to do", "job", c.job.Name())
	}
}

// run is one job's goroutine: work, then wait, until the context ends.
//
// It works before it waits, on purpose. A server that has been down for a week
// comes back with a week of expired events, and making it sit out a full
// interval before noticing would be an odd first act.
func (s *Scheduler) run(ctx context.Context, c *candidate, generation int64) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Only if nothing has replaced this goroutine. A job switched off and
		// straight back on has a newer generation already running, and this
		// one must not clear its state on the way out.
		if c.generation == generation {
			c.running = false
		}
	}()

	for {
		err := s.runOnce(ctx, c)
		if ctx.Err() != nil {
			return
		}

		delay := s.delay(c.job.Interval())
		s.record(c, generation, err, s.now().Add(delay))

		select {
		case <-ctx.Done():
			return
		case <-s.after(delay):
		}
	}
}

// runOnce calls the job and converts a panic into an error.
//
// A job that panics would otherwise take the whole process with it, including
// the HTTP server that is the only reason the process exists. An error tracker
// that dies because its own housekeeping hit a nil pointer is a poor argument
// for itself.
func (s *Scheduler) runOnce(ctx context.Context, c *candidate) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("job %q panicked: %v", c.job.Name(), recovered)
		}
	}()
	return c.job.Run(ctx)
}

// record files the outcome of a pass and when the next one is due.
func (s *Scheduler) record(c *candidate, generation int64, err error, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.generation != generation {
		return
	}

	c.status.Runs++
	c.status.LastRun = s.now()
	c.status.NextRun = next
	if err != nil {
		c.status.Failures++
		c.status.LastError = err.Error()
	} else {
		// Cleared, so a job that recovered stops looking broken.
		c.status.LastError = ""
	}

	if err != nil {
		// Logged here rather than left to the job, so every job reports a
		// failure the same way and a new one cannot forget to. A failed pass
		// never stops the schedule: the next one may well succeed, and giving
		// up quietly is how a disk fills with nobody having been told.
		s.log.Error("job failed", "job", c.job.Name(), "error", err,
			"failures", c.status.Failures, "next_run", next)
	}
}

// delay is one interval with jitter applied.
func (s *Scheduler) delay(interval time.Duration) time.Duration {
	// random() in [0,1) mapped to [-1,1), scaled by the fraction.
	offset := (s.random()*2 - 1) * jitterFraction
	delay := time.Duration(float64(interval) * (1 + offset))
	if delay <= 0 {
		return interval
	}
	return delay
}

// Jobs reports the running jobs, by name.
//
// Only the running ones. A subsystem with nothing to do has no job here, not a
// job listed as inactive, because "it does not exist" is precisely the claim
// ADR 005 makes and a row saying otherwise would soften it.
func (s *Scheduler) Jobs() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	statuses := make([]Status, 0, len(s.candidates))
	for _, c := range s.candidates {
		if c.running {
			statuses = append(statuses, c.status)
		}
	}
	slices.SortFunc(statuses, func(a, b Status) int { return strings.Compare(a.Name, b.Name) })
	return statuses
}

// Stop cancels every job and waits for the goroutines to end.
//
// It waits, unlike Reevaluate: this runs at shutdown, where the point is that a
// half-finished delete batch gets to finish rather than being abandoned with
// the process.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	cancel := s.cancel
	s.mu.Unlock()

	cancel()
	s.wg.Wait()
}
