package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func newNotifier(t *testing.T) (*Notifier, *fakeAlerts, *fakeSender) {
	t.Helper()
	repo, sender := newFakeAlerts(), &fakeSender{}
	return NewNotifier(repo, sender, fixedClock{now: alertNow}, nil), repo, sender
}

func queue(t *testing.T, repo *fakeAlerts, channels int) {
	t.Helper()
	ids := make([]int64, 0, channels)
	for index := range channels {
		ids = append(ids, repo.addChannel(string(rune('a'+index))).ID)
	}
	repo.addRule(rule(t, `{"kind":"new_issue"}`, ids, 0))
	if _, err := repo.Enqueue(context.Background(), &domain.AlertEvent{
		Kind: domain.TriggerNewIssue, ProjectID: 1, IssueID: 2,
		Title: "ValueError: boom", At: alertNow,
	}, "https://errors.example.test/projects/1/issues/2"); err != nil {
		t.Fatalf("enqueueing: %v", err)
	}
}

func TestOnePassDeliversWhatIsDue(t *testing.T) {
	notifier, repo, sender := newNotifier(t)
	queue(t, repo, 2)

	result, err := notifier.Deliver(context.Background())
	if err != nil {
		t.Fatalf("delivering: %v", err)
	}
	if result.Sent != 2 || result.Failed != 0 {
		t.Errorf("the pass reported %+v, want two delivered", result)
	}
	if len(sender.delivered()) != 2 {
		t.Errorf("the sender saw %d messages", len(sender.delivered()))
	}
	for _, notification := range repo.notifications() {
		if notification.Status != domain.NotificationSent {
			t.Errorf("a delivered notification is recorded as %q", notification.Status)
		}
	}

	// And a second pass has nothing to do, which is the steady state this job
	// spends its life in.
	second, err := notifier.Deliver(context.Background())
	if err != nil {
		t.Fatalf("the second pass: %v", err)
	}
	if second.Sent != 0 {
		t.Errorf("the second pass delivered %d messages again", second.Sent)
	}
}

// TestOneUnreachableChannelDoesNotHoldUpTheOthers is why the outbox is a table
// and not a channel: failures here are expected and survivable.
func TestOneUnreachableChannelDoesNotHoldUpTheOthers(t *testing.T) {
	notifier, repo, sender := newNotifier(t)
	queue(t, repo, 3)
	sender.refuse(errRefused)

	result, err := notifier.Deliver(context.Background())
	if err != nil {
		t.Fatalf("delivering: %v", err)
	}
	if result.Failed != 3 {
		t.Errorf("the pass reported %+v, want three failures", result)
	}
	// Every row was attempted, none was abandoned, and each one carries the
	// reason — which is the difference between a fixable typo and a mystery.
	for _, notification := range repo.notifications() {
		if notification.Status != domain.NotificationFailed {
			t.Errorf("a failed notification is recorded as %q", notification.Status)
		}
		if notification.LastError != errRefused.Error() {
			t.Errorf("the reason came back as %q", notification.LastError)
		}
		if notification.Attempts != 1 {
			t.Errorf("attempts is %d after one pass", notification.Attempts)
		}
	}
}

// TestAFailedDeliveryComesBackWhenTheBackoffExpires, and not before: a retry
// loop with no backoff is a denial of service against whoever is already down.
func TestAFailedDeliveryComesBackWhenTheBackoffExpires(t *testing.T) {
	repo, sender := newFakeAlerts(), &fakeSender{}
	clock := &movingClock{now: alertNow}
	notifier := NewNotifier(repo, sender, clock, nil)
	queue(t, repo, 1)
	sender.refuse(errRefused)

	if _, err := notifier.Deliver(context.Background()); err != nil {
		t.Fatalf("the first pass: %v", err)
	}

	clock.now = alertNow.Add(time.Second)
	again, err := notifier.Deliver(context.Background())
	if err != nil {
		t.Fatalf("a pass one second later: %v", err)
	}
	if again.Failed != 0 {
		t.Error("the row was retried a second after failing, ignoring the backoff")
	}

	clock.now = alertNow.Add(domain.FirstRetryDelay + time.Second)
	sender.refuse(nil)
	recovered, err := notifier.Deliver(context.Background())
	if err != nil {
		t.Fatalf("a pass after the backoff: %v", err)
	}
	if recovered.Sent != 1 {
		t.Error("the row never came back after the backoff expired")
	}
}

// TestTheOutboxSurvivesARestart is the claim ADR 015 rests on, expressed as
// far as a unit test can express it: the queue is the repository, so a
// notifier that is thrown away and rebuilt — which is what a restart is —
// still delivers exactly what was pending, once.
func TestTheOutboxSurvivesARestart(t *testing.T) {
	repo, sender := newFakeAlerts(), &fakeSender{}
	queue(t, repo, 2)

	// A process that dies before its first pass.
	_ = NewNotifier(repo, sender, fixedClock{now: alertNow}, nil)

	revived := NewNotifier(repo, sender, fixedClock{now: alertNow}, nil)
	result, err := revived.Deliver(context.Background())
	if err != nil {
		t.Fatalf("delivering after a restart: %v", err)
	}
	if result.Sent != 2 {
		t.Errorf("delivered %d of 2 notifications after a restart", result.Sent)
	}
	if len(sender.delivered()) != 2 {
		t.Errorf("the sender saw %d messages, so something was lost or doubled", len(sender.delivered()))
	}
}

func TestAClaimFailureIsReported(t *testing.T) {
	notifier, repo, _ := newNotifier(t)
	repo.failWith = errors.New("the disk is full")

	if _, err := notifier.Deliver(context.Background()); err == nil {
		t.Fatal("a failed claim was reported as a successful pass")
	}
}

// TestThePruneRunsAtMostHourly keeps the log sweep off the once-a-second path.
func TestThePruneRunsAtMostHourly(t *testing.T) {
	repo, sender := newFakeAlerts(), &fakeSender{}
	clock := &movingClock{now: alertNow}
	notifier := NewNotifier(repo, sender, clock, nil)

	if _, err := notifier.Deliver(context.Background()); err != nil {
		t.Fatalf("the first pass: %v", err)
	}
	if repo.pruned != 1 {
		t.Errorf("the first pass pruned %d times, want once", repo.pruned)
	}

	clock.now = alertNow.Add(time.Minute)
	if _, err := notifier.Deliver(context.Background()); err != nil {
		t.Fatalf("a pass a minute later: %v", err)
	}
	if repo.pruned != 1 {
		t.Error("the log was swept again a minute later")
	}

	clock.now = alertNow.Add(2 * time.Hour)
	if _, err := notifier.Deliver(context.Background()); err != nil {
		t.Fatalf("a pass two hours later: %v", err)
	}
	if repo.pruned != 2 {
		t.Error("the log was never swept again")
	}
}

func TestTheJobIsWhatTheSchedulerNeeds(t *testing.T) {
	notifier, repo, _ := newNotifier(t)
	queue(t, repo, 1)

	job := notifier.Job()
	if job.Name() != "notifier" {
		t.Errorf("the job is called %q", job.Name())
	}
	// The interval IS the delivery latency: everything between "it broke" and
	// "you were told" is one tick.
	if job.Interval() != NotifierInterval || job.Interval() <= 0 {
		t.Errorf("the interval is %s", job.Interval())
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if len(repo.notifications()) != 1 || repo.notifications()[0].Status != domain.NotificationSent {
		t.Error("running the job did not drain the outbox")
	}
}

// movingClock is a clock a test advances by hand.
type movingClock struct{ now time.Time }

func (c *movingClock) Now() time.Time { return c.now }
