package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sentry"
)

// SessionFlushInterval is how often the settled counters are written down.
//
// A minute, and the number is a bound on what a crash costs rather than a
// performance knob: whatever has been settled and not yet flushed is what an
// unclean stop loses. Shorter would buy a smaller loss at the price of a write
// transaction per few sessions on a store with one writer; longer would make
// the freshest hour of a dashboard stale enough to notice during a deploy,
// which is the exact moment somebody is watching it.
const SessionFlushInterval = time.Minute

// VolatilityNote is what every health response says out loud.
//
// It is not an apology, it is the documented half of ADR 008's trade-off. The
// window lives in memory, so a restart loses the sessions in flight; a reader
// who compares two numbers across a deploy and finds them different deserves
// to have been told why before they go looking for a bug. A surprise that was
// documented is not a surprise.
const VolatilityNote = "sessions are counted in a bounded in-memory window, " +
	"so figures for the most recent hour may change after a restart"

// Health is release health: how many sessions a release started and how many
// of them ended badly.
//
// It owns the in-memory window of ADR 008 and is the only thing that touches
// it, which is why the mutex lives here rather than inside the domain type:
// the window is a plain data structure with no clock and no lock, so every
// awkward rule in it — the LRU, the TTL, merging two updates about one session
// — is testable without a database and without waiting for anything.
type Health struct {
	repo  ports.HealthRepository
	clock ports.Clock
	log   *slog.Logger

	// Interval is how often the flush job runs. A field rather than a
	// constant so a test can drive a flush without waiting a minute.
	Interval time.Duration

	mu     sync.Mutex
	window *domain.SessionWindow
}

// NewHealth wires the use case.
//
// The window's size is an argument because it is the one thing an operator may
// need to change — a machine with less memory than this product assumes, or a
// gate that has to prove a full window degrades rather than falls over — and
// zero means the default (domain.DefaultSessionWindowSize).
func NewHealth(
	repo ports.HealthRepository, clock ports.Clock, logger *slog.Logger, windowSize int,
) *Health {
	if logger == nil {
		logger = slog.Default()
	}
	return &Health{
		repo:     repo,
		clock:    clock,
		log:      logger,
		Interval: SessionFlushInterval,
		window:   domain.NewSessionWindow(windowSize, domain.SessionTTL),
	}
}

// SessionReceipt says what one ingested session item accounted for.
type SessionReceipt struct {
	// Sessions is how many sessions this item spoke about. One for a `session`
	// item; the sum of an aggregate's buckets for a `sessions` one.
	Sessions int64
	// Aggregated is true when the SDK had already added the counts up itself.
	Aggregated bool
}

// Accept folds one session item into the window.
//
// Both item types land here because they are two spellings of one fact. A
// `session` item is an update about one run and needs the window to know
// whether it has been seen before; a `sessions` item is counts an SDK added up
// itself and needs nothing at all. Routing them to two use cases would put the
// same four counters behind two code paths that could drift.
func (h *Health) Accept(
	ctx context.Context, projectID int64, itemType string, payload []byte,
) (SessionReceipt, error) {
	if itemType == "sessions" {
		return h.acceptAggregates(projectID, payload)
	}
	return h.acceptSession(projectID, payload)
}

func (h *Health) acceptSession(projectID int64, payload []byte) (SessionReceipt, error) {
	decoded, err := sentry.DecodeSession(payload)
	if err != nil {
		return SessionReceipt{}, err
	}

	update := domain.SessionUpdate{
		ID:          decoded.SID,
		ProjectID:   projectID,
		Release:     decoded.Release,
		Environment: decoded.Environment,
		Started:     decoded.Started,
		Status:      domain.ParseSessionStatus(decoded.Status),
		Errors:      decoded.Errors,
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.window.Observe(update, h.clock.Now()); err != nil {
		return SessionReceipt{}, err
	}
	return SessionReceipt{Sessions: 1}, nil
}

func (h *Health) acceptAggregates(projectID int64, payload []byte) (SessionReceipt, error) {
	decoded, err := sentry.DecodeSessionAggregates(payload)
	if err != nil {
		return SessionReceipt{}, err
	}
	if decoded.Release == "" {
		return SessionReceipt{}, fmt.Errorf(
			"%w: an aggregate needs a release", domain.ErrInvalidSessionUpdate)
	}

	receipt := SessionReceipt{Aggregated: true}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, bucket := range decoded.Buckets {
		h.window.AddAggregate(domain.HealthKey{
			ProjectID:   projectID,
			Release:     decoded.Release,
			Environment: decoded.Environment,
			Hour:        domain.HourBucket(bucket.Started),
		}, domain.SessionCounts{
			Started:  bucket.Total(),
			Errored:  bucket.Errored,
			Crashed:  bucket.Crashed,
			Abnormal: bucket.Abnormal,
		})
		receipt.Sessions += bucket.Total()
	}
	return receipt, nil
}

// Flush settles what has timed out and writes down everything settled.
//
// Expiry first, and the order is load-bearing: a session that went quiet an
// hour ago has to reach a verdict before that verdict can be written, or it
// would wait a further minute for no reason.
func (h *Health) Flush(ctx context.Context) (int, error) {
	h.mu.Lock()
	expired := h.window.Expire(h.clock.Now())
	drained := h.window.Drain()
	inFlight := h.window.InFlight()
	h.mu.Unlock()

	if expired > 0 {
		h.log.Debug("sessions settled without an ending",
			"sessions", expired, "in_flight", inFlight)
	}
	if len(drained) == 0 {
		return 0, nil
	}

	buckets := make([]ports.SessionBucket, 0, len(drained))
	for key, counts := range drained {
		buckets = append(buckets, ports.SessionBucket{Key: key, Counts: counts})
	}

	if err := h.repo.AddSessionCounts(ctx, buckets); err != nil {
		// Put back rather than dropped. A disk that was full for a minute must
		// not cost a release's numbers, and the counters are commutative — the
		// next flush writes the sum, which is the same row either way.
		h.mu.Lock()
		for _, bucket := range buckets {
			h.window.AddPending(bucket.Key, bucket.Counts)
		}
		h.mu.Unlock()
		return 0, err
	}
	return len(buckets), nil
}

// Drain is the flush a clean shutdown performs.
//
// A separate name for the same work because the intent is different and worth
// reading at the call site: a stopped server has already refused new sessions,
// so this is the last chance to write down what it knows. A clean stop that
// lost the last minute of counters would be losing them on purpose (ADR 008).
func (h *Health) Drain(ctx context.Context) error {
	written, err := h.Flush(ctx)
	if err != nil {
		return fmt.Errorf("draining the session window: %w", err)
	}
	h.mu.Lock()
	stranded := h.window.InFlight()
	h.mu.Unlock()

	if written > 0 || stranded > 0 {
		// Said at info, once, at the moment it happens. This is the sentence
		// that turns "the numbers moved after the restart" from a mystery into
		// a line somebody can grep for.
		h.log.Info("session window drained",
			"buckets_written", written,
			"sessions_in_flight_lost", stranded)
	}
	return nil
}

// HasWork answers the scheduler's question: does any project accept sessions?
//
// Asked of the configuration rather than of the table, so a project switched on
// a minute ago has a flush job before its first session rather than after — and
// an installation that never enables sessions has no goroutine, no timer and no
// row in /system/jobs (ADR 005, ADR 014).
func (h *Health) HasWork(ctx context.Context) (bool, error) {
	return h.repo.HasSessions(ctx)
}

// Job is the flush seen through the scheduler's contract.
func (h *Health) Job() HealthJob { return HealthJob{health: h} }

// HealthJob writes the settled counters down on an interval.
type HealthJob struct{ health *Health }

// Name identifies the job wherever it is reported.
func (j HealthJob) Name() string { return "session-flush" }

// Interval is how long the scheduler waits between passes.
func (j HealthJob) Interval() time.Duration { return j.health.Interval }

// Run performs one flush.
func (j HealthJob) Run(ctx context.Context) error {
	written, err := j.health.Flush(ctx)
	if err != nil {
		return err
	}
	if written > 0 {
		j.health.log.Debug("session counters written", "buckets", written)
	}
	return nil
}

// WindowView is the state of the in-memory window, as a client sees it.
//
// It rides along with every health answer rather than living on a diagnostics
// endpoint nobody opens, because it is the only place the caveat can be read
// at the moment it matters: beside the number it applies to.
type WindowView struct {
	// InFlight is how many sessions are being tracked right now — the ones a
	// restart would lose.
	InFlight int `json:"in_flight"`
	// Capacity is the ceiling.
	Capacity int `json:"capacity"`
	// Evicted is how many sessions were settled early because the window was
	// full. Non-zero means precision was traded for stability, which is the
	// documented direction (ADR 008) and something an operator should be able
	// to see without reading a log.
	Evicted int64 `json:"evicted"`
	// Expired is how many were settled because they went quiet past the TTL.
	Expired int64 `json:"expired"`
	// Note says the volatility out loud, in one sentence, in every response.
	Note string `json:"note"`
}

// Window reports the state of the in-memory window.
func (h *Health) Window() WindowView {
	h.mu.Lock()
	defer h.mu.Unlock()
	return WindowView{
		InFlight: h.window.InFlight(),
		Capacity: h.window.Capacity(),
		Evicted:  h.window.Evicted(),
		Expired:  h.window.Expired(),
		Note:     VolatilityNote,
	}
}

// HealthPoint is one hour of a release's health.
type HealthPoint struct {
	Hour string `json:"hour"`
	domain.SessionCounts
	// CrashFreeRate is null for an hour with no sessions. A zero would read as
	// "everything crashed" and a one as "everything was fine", and both are
	// claims about an hour nothing reported in.
	CrashFreeRate *float64 `json:"crash_free_rate"`
}

// ReleaseHealth is one release's answer.
type ReleaseHealth struct {
	Release string       `json:"release"`
	Range   domain.Range `json:"-"`
	domain.SessionCounts
	// Healthy is the sessions that ended with nothing wrong. Derived rather
	// than stored, because it is exactly what the four disjoint counters
	// already say.
	Healthy       int64         `json:"healthy"`
	CrashFreeRate *float64      `json:"crash_free_rate"`
	Series        []HealthPoint `json:"series"`
	Window        WindowView    `json:"window"`
}

// Release answers "how is this version doing" over a range.
func (h *Health) Release(
	ctx context.Context, projectID int64, release string, window domain.Range,
) (ReleaseHealth, error) {
	if release == "" {
		return ReleaseHealth{}, fmt.Errorf(
			"%w: a release version is required", domain.ErrInvalidRelease)
	}

	hours, err := h.repo.ReleaseSeries(ctx, projectID, release,
		window.FirstBucket(), window.LastBucket())
	if err != nil {
		return ReleaseHealth{}, err
	}

	byHour := make(map[string]domain.SessionCounts, len(hours))
	var total domain.SessionCounts
	for _, hour := range hours {
		byHour[hour.Hour] = hour.Counts
		total = total.Add(hour.Counts)
	}

	buckets := window.Buckets()
	series := make([]HealthPoint, 0, len(buckets))
	for _, bucket := range buckets {
		counts := byHour[bucket]
		series = append(series, HealthPoint{
			Hour:          bucket,
			SessionCounts: counts,
			CrashFreeRate: crashFreeRate(counts),
		})
	}

	return ReleaseHealth{
		Release:       release,
		Range:         window,
		SessionCounts: total,
		Healthy:       total.Healthy(),
		CrashFreeRate: crashFreeRate(total),
		Series:        series,
		Window:        h.Window(),
	}, nil
}

// ReleaseSummary is one row of the project-wide answer.
type ReleaseSummary struct {
	Release string `json:"release"`
	domain.SessionCounts
	Healthy       int64    `json:"healthy"`
	CrashFreeRate *float64 `json:"crash_free_rate"`
	// FirstSeen and LastSeen are the hours this release actually reported in,
	// which is what tells a live release from one that stopped reporting
	// yesterday and would otherwise sit beside it looking identical.
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
}

// ProjectHealth is every release of a project over a range.
type ProjectHealth struct {
	Range domain.Range `json:"-"`
	domain.SessionCounts
	Healthy       int64            `json:"healthy"`
	CrashFreeRate *float64         `json:"crash_free_rate"`
	Releases      []ReleaseSummary `json:"releases"`
	Window        WindowView       `json:"window"`
}

// Project answers "which release is the bad one", which is the question
// somebody opens this page with.
func (h *Health) Project(
	ctx context.Context, projectID int64, window domain.Range, limit int,
) (ProjectHealth, error) {
	totals, err := h.repo.ReleaseTotals(ctx, projectID,
		window.FirstBucket(), window.LastBucket(), limit)
	if err != nil {
		return ProjectHealth{}, err
	}

	releases := make([]ReleaseSummary, 0, len(totals))
	var overall domain.SessionCounts
	for _, release := range totals {
		overall = overall.Add(release.Counts)
		releases = append(releases, ReleaseSummary{
			Release:       release.Release,
			SessionCounts: release.Counts,
			Healthy:       release.Counts.Healthy(),
			CrashFreeRate: crashFreeRate(release.Counts),
			FirstSeen:     release.FirstHour,
			LastSeen:      release.LastHour,
		})
	}

	return ProjectHealth{
		Range:         window,
		SessionCounts: overall,
		Healthy:       overall.Healthy(),
		CrashFreeRate: crashFreeRate(overall),
		Releases:      releases,
		Window:        h.Window(),
	}, nil
}

// crashFreeRate renders the rate as a pointer, so "no sessions" is null rather
// than a number that reads as an opinion.
func crashFreeRate(counts domain.SessionCounts) *float64 {
	rate, known := counts.CrashFreeRate()
	if !known {
		return nil
	}
	return &rate
}
