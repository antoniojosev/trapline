package usecase

import (
	"context"
	"log/slog"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Retention deletes events that are past their keep-window.
//
// It is one of the few things here that runs on a timer, and that is a
// deliberate exception to the rule that a subsystem gets no goroutine unless
// it is switched on (ADR 005). Retention is not optional: without it a
// single-file store grows until the disk is full, and the product's promise is
// a server nobody has to tend. The rule protects users from paying for
// features they did not ask for; nobody asks for unbounded growth.
type Retention struct {
	projects   ports.ProjectRepository
	config     ports.ProjectConfigStore
	issues     ports.IssueRepository
	aggregates ports.StatsRepository
	clock      ports.Clock
	// traces is the transaction side: the minute buckets, the hour buckets
	// and the sampled waterfalls. Optional, because an assembly without
	// tracing has no such tables to sweep and must not grow a dependency on
	// one — the same rule every other optional collaborator here follows.
	traces ports.TraceRepository
	// artifacts is the uploaded source maps and the staging area the chunked
	// upload uses. Optional like traces, and named structurally rather than
	// by type so retention does not grow a dependency on the use case that
	// owns the windows — it asks for a sweep and is told how many rows went.
	artifacts artifactSweeper

	// sessions holds the release-health counters. Optional in the same way
	// traces are, and swept on the aggregates' clock rather than the events'
	// one: four counters per release-hour are what still answers "was the last
	// release worse" long after the events that produced them are gone, which
	// is the same argument ADR 010 makes for the error buckets.
	sessions ports.HealthRepository

	// Interval between sweeps. An hour rather than a minute because retention
	// is measured in days: sweeping more often would burn writes on a store
	// whose single writer is shared with ingestion, to delete rows that are
	// not going to become any more expired in the meantime.
	Interval time.Duration
	// BatchSize bounds one delete statement. A sweep on a live store must
	// never hold one long write transaction, because that stalls ingestion —
	// and an error tracker that stops accepting events while tidying up has
	// failed at the only moment that matters.
	BatchSize int
	// Pause between batches, so a large sweep leaves gaps for the writer that
	// is actually serving users.
	Pause time.Duration
}

// NewRetention wires the use case with defaults.
func NewRetention(
	projects ports.ProjectRepository,
	config ports.ProjectConfigStore,
	issues ports.IssueRepository,
	aggregates ports.StatsRepository,
	clock ports.Clock,
) *Retention {
	return &Retention{
		projects:   projects,
		config:     config,
		issues:     issues,
		aggregates: aggregates,
		clock:      clock,
		Interval:   time.Hour,
		BatchSize:  1000,
		Pause:      50 * time.Millisecond,
	}
}

// WithSessions adds the release-health counters to the sweep.
//
// A seam like WithTraces and for the same reason: retention was built before
// this subsystem existed, and a sweeper that could not be assembled without it
// would make every test that only cares about events carry a table it never
// writes to.
func (r *Retention) WithSessions(sessions ports.HealthRepository) *Retention {
	r.sessions = sessions
	return r
}

// WithTraces adds the transaction tables to the sweep.
//
// A setter rather than a sixth constructor argument, for the reason the ingest
// path has them: adding the next area should not rewrite every call site
// again. A Retention built without it sweeps exactly what it swept before
// tracing existed.
func (r *Retention) WithTraces(traces ports.TraceRepository) *Retention {
	r.traces = traces
	return r
}

// artifactSweeper is what retention needs of the artefact use case: one call
// that removes what nothing will ever ask for again.
//
// The windows themselves live with the artefacts (domain.ChunkRetention,
// domain.OrphanArtifactRetention) rather than here, because they are not a
// project setting: an abandoned chunk and an artefact with no release to die
// with expire on the same schedule everywhere.
type artifactSweeper interface {
	Sweep(ctx context.Context, batch int) (int64, error)
}

// WithArtifacts adds the uploaded source maps and the chunk staging area to
// the sweep.
//
// A setter, like WithTraces, and for the same reason: a Retention built
// without it sweeps exactly what it swept before source maps existed.
func (r *Retention) WithArtifacts(artifacts artifactSweeper) *Retention {
	r.artifacts = artifacts
	return r
}

// SweepResult reports what one pass removed.
type SweepResult struct {
	Projects int
	Deleted  int64
}

// Job presents retention as a scheduled job.
//
// It is registered unconditionally, and that is the one deliberate exception
// to the rule that a subsystem gets no goroutine unless somebody switched it
// on (ADR 005, ADR 014). Retention is not opt-in: without it a single-file
// store grows until the disk is full, and the promise being kept is a server
// nobody has to tend. The rule protects users from paying for features they
// did not ask for; nobody asks for unbounded growth.
func (r *Retention) Job() RetentionJob { return RetentionJob{retention: r} }

// RetentionJob is the sweeper seen through the scheduler's contract.
//
// It holds no state of its own — the interval and the work both come from the
// Retention it wraps — so "how often" is not written down in a second place.
// It names no scheduler type either: the contract is structural, which is what
// keeps the use cases from depending on the thing that runs them.
type RetentionJob struct{ retention *Retention }

// Name identifies the job wherever it is reported.
func (j RetentionJob) Name() string { return "retention" }

// Interval is how long the scheduler waits between sweeps.
func (j RetentionJob) Interval() time.Duration { return j.retention.Interval }

// Run performs one sweep.
//
// A failure is returned rather than swallowed: the scheduler logs it, counts
// it, exposes it on the jobs endpoint and schedules the next pass anyway. This
// used to be a loop that logged and carried on by itself, and every job that
// followed would have had to remember to do the same.
func (j RetentionJob) Run(ctx context.Context) error {
	result, err := j.retention.Sweep(ctx)
	if err != nil {
		return err
	}
	if result.Deleted > 0 {
		slog.Info("retention swept",
			"deleted", result.Deleted,
			"projects", result.Projects,
		)
	}
	return nil
}

// Sweep runs one pass over every project.
func (r *Retention) Sweep(ctx context.Context) (SweepResult, error) {
	projects, err := r.projects.List(ctx)
	if err != nil {
		return SweepResult{}, err
	}

	result := SweepResult{Projects: len(projects)}
	for index := range projects {
		deleted, err := r.sweepProject(ctx, projects[index].ID)
		if err != nil {
			return result, err
		}
		result.Deleted += deleted
	}

	// The minute buckets are swept once per pass and not once per project,
	// because their window is not a project setting: it is the resolution the
	// storage design offers, and it is the same 48 hours everywhere (ADR
	// 021). Running it inside the loop would issue the identical delete once
	// per project and report the first one's rows as every one's.
	minutes, err := r.sweepMinutes(ctx)
	result.Deleted += minutes
	if err != nil {
		return result, err
	}

	// Once per pass and not once per project, for the same reason the minute
	// buckets are: neither window is a project setting. An artefact uploaded
	// against a release is not swept at all — it is deleted with the release
	// by the schema's cascade, which is the retention rule of ADR 018 stated
	// where it cannot be forgotten.
	artifacts, err := r.sweepArtifacts(ctx)
	result.Deleted += artifacts
	return result, err
}

func (r *Retention) sweepArtifacts(ctx context.Context) (int64, error) {
	if r.artifacts == nil {
		return 0, nil
	}
	return r.artifacts.Sweep(ctx, r.BatchSize)
}

// sweepMinutes deletes minute buckets past the fixed 48-hour window.
//
// It is the backstop for the downsampling job rather than a duplicate of it. A
// project whose tracing was switched off before the job's next pass leaves
// minutes nobody will ever fold, and the job does not exist while no project
// accepts transactions (ADR 014) — so without this they would sit there until
// somebody noticed the file growing.
func (r *Retention) sweepMinutes(ctx context.Context) (int64, error) {
	if r.traces == nil {
		return 0, nil
	}
	return r.sweepBatched(ctx, r.clock.Now().Add(-domain.MinuteRetention),
		func(cutoff time.Time) (int64, error) {
			return r.traces.PruneMinutes(ctx, cutoff, r.BatchSize)
		})
}

// sweepProject deletes one project's expired events and expired aggregates.
//
// Two windows, not one, and their independence is the point. Events carry a
// category implicitly today — only errors are stored — so a single cutoff for
// them is correct and honest; when transactions land they will have their own,
// much shorter one, and that half becomes a loop over categories. The
// aggregates are on a far longer clock (400 days against 90), because they are
// what answers "was this worse last quarter" long after the payloads that fed
// them are gone. Sweeping them together would throw away the cheap history to
// save the expensive one (ADR 010).
//
// it with errors.Is(context.Canceled), and wrapping would only obscure that.
//
//nolint:wrapcheck // ctx.Err() is returned verbatim on purpose: callers match
func (r *Retention) sweepProject(ctx context.Context, projectID int64) (int64, error) {
	config, err := r.config.ProjectConfig(ctx, projectID)
	if err != nil {
		return 0, err
	}

	now := r.clock.Now()
	events, err := r.sweepBatched(ctx, now.Add(-retentionFor(config, engine.CategoryError)),
		func(cutoff time.Time) (int64, error) {
			return r.issues.DeleteEventsBefore(ctx, projectID, cutoff, r.BatchSize)
		})
	if err != nil {
		return events, err
	}

	aggregates, err := r.sweepBatched(ctx, now.Add(-retentionFor(config, engine.CategoryAggregates)),
		func(cutoff time.Time) (int64, error) {
			return r.aggregates.DeleteAggregatesBefore(ctx, projectID, cutoff, r.BatchSize)
		})
	if err != nil {
		return events + aggregates, err
	}

	transactions, err := r.sweepTransactions(ctx, projectID, config, now)
	if err != nil {
		return events + aggregates + transactions, err
	}

	sessions, err := r.sweepSessions(ctx, projectID, config, now)
	return events + aggregates + transactions + sessions, err
}

// sweepSessions deletes one project's expired release-health counters.
//
// On the aggregates' keep-window (400 days) and not on the session category's,
// because that is what these rows are: a summary that outlives whatever
// produced it. There are no session payloads to expire on a shorter clock —
// there are no session rows at all, which is the whole point of ADR 008 — so
// the long window is the only one that applies.
func (r *Retention) sweepSessions(
	ctx context.Context, projectID int64, config domain.ProjectConfig, now time.Time,
) (int64, error) {
	if r.sessions == nil {
		return 0, nil
	}
	return r.sweepBatched(ctx, now.Add(-retentionFor(config, engine.CategoryAggregates)),
		func(cutoff time.Time) (int64, error) {
			return r.sessions.PruneSessionsBefore(ctx, projectID, cutoff, r.BatchSize)
		})
}

// sweepTransactions deletes one project's hour buckets and sampled
// waterfalls.
//
// They share the project's `transaction` keep-window (7 days by default)
// because they are the two halves of the same answer: the hours are the
// numbers the performance page draws and the traces are the examples it links
// to, and keeping one past the other would leave either a chart with no
// examples or examples nothing points at. The minute buckets are on a
// different clock entirely and are swept once per pass, not here.
func (r *Retention) sweepTransactions(
	ctx context.Context, projectID int64, config domain.ProjectConfig, now time.Time,
) (int64, error) {
	if r.traces == nil {
		return 0, nil
	}

	window := now.Add(-retentionFor(config, engine.CategoryTransaction))
	hours, err := r.sweepBatched(ctx, window, func(cutoff time.Time) (int64, error) {
		return r.traces.PruneHours(ctx, projectID, cutoff, r.BatchSize)
	})
	if err != nil {
		return hours, err
	}

	traces, err := r.sweepBatched(ctx, window, func(cutoff time.Time) (int64, error) {
		return r.traces.PruneTraces(ctx, projectID, cutoff, r.BatchSize)
	})
	return hours + traces, err
}

// sweepBatched runs one delete to exhaustion, a bounded batch at a time.
//
// The batching is not an optimisation: the store has a single writer shared
// with ingestion, so one long delete transaction stalls the endpoint the
// product exists to keep answering. The pause between batches is what leaves
// the writer room to serve somebody.
//
//nolint:wrapcheck // ctx.Err() and the caller's own errors pass through.
func (r *Retention) sweepBatched(
	ctx context.Context, cutoff time.Time, remove func(time.Time) (int64, error),
) (int64, error) {
	var deleted int64
	for {
		// Cancellation is propagated rather than swallowed. It is not a
		// failure — Run checks the context before logging anything — but
		// returning nil here would make "we finished" and "we were stopped
		// halfway" indistinguishable to any other caller, including the CLI's
		// one-shot sweep.
		if err := ctx.Err(); err != nil {
			return deleted, err
		}

		batch, err := remove(cutoff)
		if err != nil {
			return deleted, err
		}
		deleted += batch

		if batch < int64(r.BatchSize) {
			// A short batch means the cutoff has been reached; another query
			// would find nothing.
			return deleted, nil
		}

		select {
		case <-ctx.Done():
			return deleted, ctx.Err()
		case <-time.After(r.Pause):
		}
	}
}

// retentionFor resolves a category's keep-window, preferring the project's
// configuration over the engine's default.
//
// A configured zero means keep nothing, not "use the default". It used to mean
// the default, which made it a setting an operator could write, read back and
// watch do nothing at all — and left no way to say "purge this" short of
// deleting rows by hand. The way to inherit the default is to omit the
// category, which is what the API has always documented and what the PUT
// already does by replacing the map (ADR 031, proposed).
func retentionFor(config domain.ProjectConfig, category engine.Category) time.Duration {
	if days, configured := config.RetentionDays[string(category)]; configured && days >= 0 {
		return time.Duration(days) * 24 * time.Hour
	}
	if window, known := engine.DefaultRetention()[category]; known {
		return window
	}
	return 90 * 24 * time.Hour
}
