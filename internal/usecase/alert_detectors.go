package usecase

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// How much the detectors are allowed to cost.
const (
	// rateWindowMinutes is the length of the in-memory counter behind
	// error_rate, in one-minute buckets. Five minutes is what ADR 015 fixes,
	// and it is also what makes the structure a fixed-size array rather than
	// something that has to be trimmed.
	rateWindowMinutes = 5
	// maxTrackedProjects bounds the counter's memory. A project id comes from
	// an authenticated DSN, so this is not an attacker-chosen key the way an
	// IP address is, but a map with no ceiling in a product that promises a
	// 30 MB footprint is still a promise with a hole in it.
	maxTrackedProjects = 512
	// rateEvaluationInterval is how often a project's rules are re-checked
	// against the counter. The counter itself is updated on every event; this
	// throttles the part that reads rules and can write to the database.
	rateEvaluationInterval = time.Second
	// spikeEvaluationInterval is how often one issue's history is re-read.
	// A spike is a statement about an hour, so asking more than twice a
	// minute buys nothing and costs a query per event.
	spikeEvaluationInterval = 30 * time.Second
	// maxTrackedIssues bounds the spike throttle's memory.
	maxTrackedIssues = 4096
)

// ObserveEvent counts an event towards the error-rate trigger.
//
// It is called BEFORE the rate limiter, and that ordering is the whole point:
// the signal a spike produces must be measured on what arrived, not on what
// survived the limiter, or a flood large enough to be refused would silence
// the alert about itself at exactly the moment it mattered (ADR 005). It is
// also why the counter is in memory — the events being counted here include
// the ones about to be thrown away, and a database write for each of those
// would make refusing a flood more expensive than accepting it.
//
// It returns nothing and can fail at nothing. An alerting subsystem that can
// break ingestion is a worse trade than one that occasionally misses a
// notification.
func (a *Alerts) ObserveEvent(ctx context.Context, projectID int64) {
	watched, err := a.repo.Watches(ctx, domain.TriggerErrorRate)
	if err != nil {
		slog.Error("could not decide whether any rule watches the error rate", "error", err)
		return
	}
	if !watched {
		// The common case, and it costs a read lock and a slice walk. An
		// installation that has not configured an error_rate rule pays this
		// and nothing else.
		return
	}

	now := a.clock.Now()
	a.rates.add(projectID, now)
	if !a.rates.readyToEvaluate(projectID, now) {
		return
	}

	rules, err := a.repo.RulesOfKind(ctx, domain.TriggerErrorRate, projectID)
	if err != nil {
		slog.Error("could not read the error-rate rules", "project_id", projectID, "error", err)
		return
	}
	for _, rule := range rules {
		rate := a.rates.perMinute(projectID, now, rule.Trigger.Window())
		if rate < int64(rule.Trigger.MinEventsPerMinute) {
			continue
		}
		event := domain.AlertEvent{
			Kind:      domain.TriggerErrorRate,
			ProjectID: projectID,
			Count:     rate,
			At:        now,
		}
		// Offered to the rules rather than fired directly, so the silence
		// window and the fan-out to channels are decided in the one place
		// that decides them for every trigger.
		if _, err := a.repo.Enqueue(ctx, &event, a.origin.String()); err != nil {
			slog.Error("could not queue an error-rate notification",
				"project_id", projectID, "error", err)
		}
		// One enqueue per pass. The rules are offered the same event inside
		// Enqueue, so a second call here would only re-ask a question that
		// was already asked of all of them.
		return
	}
}

// DetectSpike asks whether one issue just got much louder than it has been.
//
// Called after the event is recorded, deliberately: the hour bucket this reads
// was written by the transaction that stored the event (ADR 010), so "the
// current hour" already includes the occurrence being judged. Reading it
// before would compare an hour that is missing its newest event against a
// baseline that is not, and the comparison would be quietly wrong in the
// direction of never firing.
func (a *Alerts) DetectSpike(ctx context.Context, issue *domain.Issue, environment string) {
	watched, err := a.repo.Watches(ctx, domain.TriggerIssueSpike)
	if err != nil {
		slog.Error("could not decide whether any rule watches spikes", "error", err)
		return
	}
	if !watched {
		return
	}

	now := a.clock.Now()
	if !a.spikes.ready(issue.ID, now) {
		return
	}

	rules, err := a.repo.RulesOfKind(ctx, domain.TriggerIssueSpike, issue.ProjectID)
	if err != nil {
		slog.Error("could not read the spike rules", "project_id", issue.ProjectID, "error", err)
		return
	}
	if len(rules) == 0 {
		return
	}

	// The widest window any rule asked for, rounded up to whole hours because
	// the aggregates are hourly. A rule asking for ninety minutes is told, in
	// the API documentation, that it will be judged over two hours: rounding
	// silently would make the rule mean something other than what it says.
	baselineHours := 1
	for _, rule := range rules {
		hours := int(math.Ceil(rule.Trigger.Window().Hours()))
		if hours > baselineHours {
			baselineHours = hours
		}
	}

	// One extra hour: the last bucket is the hour in progress, which is the
	// number being judged, and the ones before it are what it is judged
	// against.
	counts, err := a.repo.IssueHourlyCounts(ctx, issue.ProjectID, issue.ID, now, baselineHours+1)
	if err != nil {
		slog.Error("could not read an issue's history", "issue_id", issue.ID, "error", err)
		return
	}
	if len(counts) < 2 {
		return
	}

	current := counts[len(counts)-1]
	baseline := average(counts[:len(counts)-1])

	event := domain.AlertEvent{
		Kind:        domain.TriggerIssueSpike,
		ProjectID:   issue.ProjectID,
		IssueID:     issue.ID,
		Title:       issue.Title,
		Culprit:     issue.Culprit,
		Level:       issue.Level,
		Release:     issue.LastRelease,
		Environment: environment,
		Count:       current,
		Baseline:    baseline,
		At:          now,
	}
	if _, err := a.repo.Enqueue(ctx, &event, a.origin.IssueURL(issue.ProjectID, issue.ID)); err != nil {
		slog.Error("could not queue a spike notification", "issue_id", issue.ID, "error", err)
	}
}

// average rounds down, so a baseline of "half an event an hour" is zero and
// the rule falls back to its min_count. Rounding up would invent history the
// issue does not have.
func average(counts []int64) int64 {
	if len(counts) == 0 {
		return 0
	}
	var total int64
	for _, count := range counts {
		total += count
	}
	return total / int64(len(counts))
}

// rateWindow counts events per project over the last few minutes.
type rateWindow struct {
	mu       sync.Mutex
	projects map[int64]*minuteRing
}

// minuteRing is a fixed ring of per-minute counters.
//
// A ring rather than a slice of timestamps: the memory is constant per project
// whatever the volume, which matters because this is written on the hot path
// by a product whose thesis is a 30 MB footprint. Each slot remembers which
// minute it holds, so a slot from an hour ago is recognised as stale rather
// than counted as current.
type minuteRing struct {
	counts  [rateWindowMinutes]int64
	minutes [rateWindowMinutes]int64
	// evaluatedAt throttles the part of the detector that reads rules.
	evaluatedAt time.Time
	// touchedAt is what eviction uses.
	touchedAt time.Time
}

func newRateWindow() *rateWindow {
	return &rateWindow{projects: map[int64]*minuteRing{}}
}

func (w *rateWindow) add(projectID int64, at time.Time) {
	minute := at.Unix() / 60
	slot := int(minute % rateWindowMinutes)

	w.mu.Lock()
	defer w.mu.Unlock()

	ring, found := w.projects[projectID]
	if !found {
		w.evictIfFullLocked(at)
		ring = &minuteRing{}
		w.projects[projectID] = ring
	}
	if ring.minutes[slot] != minute {
		ring.minutes[slot], ring.counts[slot] = minute, 0
	}
	ring.counts[slot]++
	ring.touchedAt = at
}

// perMinute is the average events per minute over a window.
//
// The minute in progress counts in full. It means the figure is slightly high
// at the start of a minute and exactly right at the end of one, which is the
// direction to be wrong in for a trigger whose job is to notice a fire early.
func (w *rateWindow) perMinute(projectID int64, at time.Time, window time.Duration) int64 {
	minutes := int(window / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	if minutes > rateWindowMinutes {
		minutes = rateWindowMinutes
	}
	current := at.Unix() / 60

	w.mu.Lock()
	defer w.mu.Unlock()

	ring, found := w.projects[projectID]
	if !found {
		return 0
	}
	var total int64
	for offset := range minutes {
		minute := current - int64(offset)
		slot := int(((minute % rateWindowMinutes) + rateWindowMinutes) % rateWindowMinutes)
		if ring.minutes[slot] == minute {
			total += ring.counts[slot]
		}
	}
	return total / int64(minutes)
}

// readyToEvaluate reports whether enough time has passed to re-check a
// project's rules, and records that it is being checked now.
func (w *rateWindow) readyToEvaluate(projectID int64, at time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	ring, found := w.projects[projectID]
	if !found {
		return false
	}
	if at.Sub(ring.evaluatedAt) < rateEvaluationInterval {
		return false
	}
	ring.evaluatedAt = at
	return true
}

// evictIfFullLocked drops the least recently used project when the map is at
// its ceiling. Losing a counter costs at most one late notification; an
// unbounded map costs the footprint promise.
func (w *rateWindow) evictIfFullLocked(now time.Time) {
	if len(w.projects) < maxTrackedProjects {
		return
	}
	var (
		oldest   int64
		oldestAt = now
		found    bool
	)
	for id, ring := range w.projects {
		if !found || ring.touchedAt.Before(oldestAt) {
			oldest, oldestAt, found = id, ring.touchedAt, true
		}
	}
	if found {
		delete(w.projects, oldest)
	}
}

// evaluationClock throttles a per-key evaluation.
type evaluationClock struct {
	mu       sync.Mutex
	interval time.Duration
	seen     map[int64]time.Time
}

func newEvaluationClock(interval time.Duration) *evaluationClock {
	return &evaluationClock{interval: interval, seen: map[int64]time.Time{}}
}

// ready reports whether a key may be evaluated now, and records that it was.
func (c *evaluationClock) ready(key int64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if last, found := c.seen[key]; found && now.Sub(last) < c.interval {
		return false
	}
	if len(c.seen) >= maxTrackedIssues {
		// Cleared wholesale rather than evicted one at a time. The cost of
		// forgetting is one extra evaluation per issue, the map is only a
		// throttle, and a full sweep of a bounded map is cheaper and far
		// shorter to read than a least-recently-used policy for something
		// this unimportant.
		clear(c.seen)
	}
	c.seen[key] = now
	return true
}
