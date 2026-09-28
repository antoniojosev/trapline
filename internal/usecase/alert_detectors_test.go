package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func newDetectors(t *testing.T, clock *movingClock) (*Alerts, *fakeAlerts) {
	t.Helper()
	repo := newFakeAlerts()
	origin, err := domain.ParseOrigin("https://errors.example.test")
	if err != nil {
		t.Fatalf("the fixture origin does not parse: %v", err)
	}
	return NewAlerts(repo, &fakeSender{}, clock, origin, nil), repo
}

// TestNothingIsCountedWithoutARule is ADR 005 on the hot path: an installation
// that has not configured alerting pays a slice walk per event and nothing
// else.
func TestNothingIsCountedWithoutARule(t *testing.T) {
	clock := &movingClock{now: alertNow}
	alerts, repo := newDetectors(t, clock)

	for range 1000 {
		alerts.ObserveEvent(context.Background(), 1)
	}
	if len(repo.events()) != 0 {
		t.Errorf("%d events were offered to the rules with none configured", len(repo.events()))
	}
}

func TestTheErrorRateFiresOnWhatArrived(t *testing.T) {
	clock := &movingClock{now: alertNow}
	alerts, repo := newDetectors(t, clock)
	channel := repo.addChannel("ops")
	repo.addRule(rule(t, `{"kind":"error_rate","window_s":60,"min_events_per_min":10}`,
		[]int64{channel.ID}, time.Hour))

	ctx := context.Background()
	// Nine events in the minute is under the threshold, and the evaluation is
	// throttled to once a second, so the clock has to move for the rules to be
	// consulted at all.
	for range 9 {
		alerts.ObserveEvent(ctx, 1)
		clock.now = clock.now.Add(2 * time.Second)
	}
	if len(repo.notifications()) != 0 {
		t.Fatalf("nine events per minute fired a rule set to ten")
	}

	alerts.ObserveEvent(ctx, 1)
	clock.now = clock.now.Add(2 * time.Second)
	alerts.ObserveEvent(ctx, 1)

	queued := repo.notifications()
	if len(queued) != 1 {
		t.Fatalf("crossing the threshold queued %d notifications, want 1", len(queued))
	}
	if queued[0].Payload.Event != domain.TriggerErrorRate {
		t.Errorf("the payload says %q", queued[0].Payload.Event)
	}
	// A project-wide alert, so the subject is the project and not an issue.
	if queued[0].SubjectKey != "project:1" {
		t.Errorf("the subject is %q", queued[0].SubjectKey)
	}
}

// TestTheCounterIsPerProject: one noisy project must not fire another's rule.
func TestTheCounterIsPerProject(t *testing.T) {
	clock := &movingClock{now: alertNow}
	alerts, repo := newDetectors(t, clock)
	channel := repo.addChannel("ops")
	repo.addRule(rule(t, `{"kind":"error_rate","window_s":60,"min_events_per_min":5}`,
		[]int64{channel.ID}, time.Hour))

	ctx := context.Background()
	for range 4 {
		alerts.ObserveEvent(ctx, 1)
		alerts.ObserveEvent(ctx, 2)
		clock.now = clock.now.Add(2 * time.Second)
	}
	if len(repo.notifications()) != 0 {
		t.Fatal("four events in each of two projects fired a rule set to five")
	}
}

// TestTheWindowSlides: a burst an hour ago is not a burst now.
func TestTheWindowSlides(t *testing.T) {
	clock := &movingClock{now: alertNow}
	alerts, repo := newDetectors(t, clock)
	channel := repo.addChannel("ops")
	repo.addRule(rule(t, `{"kind":"error_rate","window_s":60,"min_events_per_min":5}`,
		[]int64{channel.ID}, time.Hour))

	ctx := context.Background()
	for range 4 {
		alerts.ObserveEvent(ctx, 1)
		clock.now = clock.now.Add(2 * time.Second)
	}

	// An hour later the ring's slots hold minutes that are long gone, and they
	// must be recognised as stale rather than counted as current.
	clock.now = clock.now.Add(time.Hour)
	alerts.ObserveEvent(ctx, 1)
	clock.now = clock.now.Add(2 * time.Second)
	alerts.ObserveEvent(ctx, 1)

	if len(repo.notifications()) != 0 {
		t.Error("events from an hour ago were counted towards the current rate")
	}
}

func TestADetectorFailureDoesNotBreakIngestion(t *testing.T) {
	clock := &movingClock{now: alertNow}
	alerts, repo := newDetectors(t, clock)
	repo.failWith = errors.New("the disk is full")

	// Nothing returns an error here on purpose: an alerting subsystem that can
	// break ingestion is a worse trade than one that misses a notification.
	alerts.ObserveEvent(context.Background(), 1)
	alerts.DetectSpike(context.Background(), &domain.Issue{ID: 1, ProjectID: 1}, "production")
}

func TestASpikeIsJudgedAgainstTheIssuesOwnPast(t *testing.T) {
	clock := &movingClock{now: alertNow}
	alerts, repo := newDetectors(t, clock)
	channel := repo.addChannel("ops")
	repo.addRule(rule(t, `{"kind":"issue_spike","window_s":3600,"min_count":10,"factor":3}`,
		[]int64{channel.ID}, time.Hour))

	ctx := context.Background()
	issue := domain.Issue{ID: 4, ProjectID: 1, Title: "ValueError: boom", Level: domain.LevelError}

	// One quiet hour behind, and a current hour that is quiet too.
	repo.hourly = []int64{2, 5}
	alerts.DetectSpike(ctx, &issue, "production")
	if len(repo.notifications()) != 0 {
		t.Fatal("five events in an hour fired a rule with a floor of ten")
	}

	// Now the current hour is well over both the floor and the ratio.
	clock.now = clock.now.Add(time.Minute)
	repo.hourly = []int64{2, 40}
	alerts.DetectSpike(ctx, &issue, "production")

	queued := repo.notifications()
	if len(queued) != 1 {
		t.Fatalf("a spike queued %d notifications, want 1", len(queued))
	}
	if queued[0].Payload.Count != 40 || queued[0].Payload.Baseline != 2 {
		t.Errorf("the payload reports %d against a baseline of %d",
			queued[0].Payload.Count, queued[0].Payload.Baseline)
	}
	if queued[0].Payload.URL == "" {
		t.Error("the spike notification carries no link to the issue")
	}
}

// TestSpikesAreNotReEvaluatedPerEvent: this reads the aggregates, and a query
// per ingested event is exactly the cost ADR 005 refuses.
func TestSpikesAreNotReEvaluatedPerEvent(t *testing.T) {
	clock := &movingClock{now: alertNow}
	alerts, repo := newDetectors(t, clock)
	channel := repo.addChannel("ops")
	repo.addRule(rule(t, `{"kind":"issue_spike","window_s":3600,"min_count":1,"factor":1}`,
		[]int64{channel.ID}, 0))
	repo.hourly = []int64{0, 50}

	ctx := context.Background()
	issue := domain.Issue{ID: 4, ProjectID: 1}
	for range 100 {
		alerts.DetectSpike(ctx, &issue, "production")
	}

	// Every evaluation that got through offered an event to the rules, so the
	// count of offered events is the count of evaluations — whatever the
	// silence window then decided to do with them.
	if got := len(repo.events()); got != 1 {
		t.Errorf("a hundred events produced %d evaluations, want the throttle to allow one", got)
	}

	clock.now = clock.now.Add(spikeEvaluationInterval + time.Second)
	alerts.DetectSpike(ctx, &issue, "production")
	if got := len(repo.events()); got != 2 {
		t.Errorf("the throttle never expired: %d evaluations", got)
	}
}

func TestSpikeDoesNothingWithoutARule(t *testing.T) {
	clock := &movingClock{now: alertNow}
	alerts, repo := newDetectors(t, clock)
	alerts.DetectSpike(context.Background(), &domain.Issue{ID: 1, ProjectID: 1}, "production")
	if len(repo.events()) != 0 {
		t.Error("a spike was evaluated with no rule watching for one")
	}
}

func TestTheRateWindowEvictsUnderPressure(t *testing.T) {
	window := newRateWindow()
	now := alertNow

	for project := range int64(maxTrackedProjects + 50) {
		window.add(project+1, now)
		now = now.Add(time.Millisecond)
	}
	if len(window.projects) > maxTrackedProjects {
		t.Errorf("the counter holds %d projects, past its ceiling of %d",
			len(window.projects), maxTrackedProjects)
	}
	// Losing a counter costs at most one late notification; an unbounded map
	// costs the footprint promise.
	if window.perMinute(int64(maxTrackedProjects+50), now, time.Minute) == 0 {
		t.Error("the most recent project was the one evicted")
	}
}

func TestPerMinuteAveragesOverTheWindow(t *testing.T) {
	window := newRateWindow()
	base := alertNow.Truncate(time.Minute)

	for range 60 {
		window.add(1, base)
	}
	for range 30 {
		window.add(1, base.Add(time.Minute))
	}

	now := base.Add(time.Minute)
	if got := window.perMinute(1, now, time.Minute); got != 30 {
		t.Errorf("the last minute averaged %d, want 30", got)
	}
	if got := window.perMinute(1, now, 2*time.Minute); got != 45 {
		t.Errorf("two minutes averaged %d, want 45", got)
	}
	// A window longer than the counter is clamped rather than answered wrong.
	if got := window.perMinute(1, now, time.Hour); got != 90/rateWindowMinutes {
		t.Errorf("an over-long window averaged %d", got)
	}
	if got := window.perMinute(999, now, time.Minute); got != 0 {
		t.Errorf("a project nobody has counted reports %d", got)
	}
}

func TestTheEvaluationClockForgetsWholesaleUnderPressure(t *testing.T) {
	clock := newEvaluationClock(time.Minute)
	now := alertNow

	for key := range int64(maxTrackedIssues + 10) {
		clock.ready(key, now)
	}
	if len(clock.seen) > maxTrackedIssues {
		t.Errorf("the throttle holds %d keys, past its ceiling", len(clock.seen))
	}
}
