package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var alertNow = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

func newAlerts(t *testing.T) (*Alerts, *fakeAlerts, *fakeSender, *int) {
	t.Helper()
	repo, sender := newFakeAlerts(), &fakeSender{}
	origin, err := domain.ParseOrigin("https://errors.example.test")
	if err != nil {
		t.Fatalf("the fixture origin does not parse: %v", err)
	}
	reevaluations := 0
	alerts := NewAlerts(repo, sender, fixedClock{now: alertNow}, origin,
		func() { reevaluations++ })
	return alerts, repo, sender, &reevaluations
}

func rule(t *testing.T, triggerJSON string, channels []int64, silence time.Duration) domain.AlertRule {
	t.Helper()
	trigger, err := domain.ParseTrigger([]byte(triggerJSON))
	if err != nil {
		t.Fatalf("the fixture trigger does not parse: %v", err)
	}
	built, err := domain.NewAlertRule(nil, "a rule", trigger, channels, silence, true)
	if err != nil {
		t.Fatalf("building the rule: %v", err)
	}
	return built
}

// TestAddingAChannelWakesTheScheduler is the promise of ADR 014 seen from the
// other end: switching a subsystem on takes effect while the person who
// switched it on is still looking at the screen, not at the next restart.
func TestAddingAChannelWakesTheScheduler(t *testing.T) {
	alerts, _, _, reevaluations := newAlerts(t)
	ctx := context.Background()

	channel, err := alerts.AddChannel(ctx, domain.ChannelWebhook, "ops", domain.ChannelConfig{
		URL: "https://example.test/hook", Secret: "a-secret-long-enough",
	}, false)
	if err != nil {
		t.Fatalf("adding a channel: %v", err)
	}
	if *reevaluations != 1 {
		t.Errorf("the scheduler was told %d times, want once", *reevaluations)
	}

	if has, _ := alerts.HasChannels(ctx); !has {
		t.Error("the scheduler's question answers no with a channel configured")
	}
	if err := alerts.RemoveChannel(ctx, channel.ID); err != nil {
		t.Fatalf("removing: %v", err)
	}
	// Removing the last one gives the goroutine back.
	if *reevaluations != 2 {
		t.Errorf("the scheduler was told %d times after a removal, want twice", *reevaluations)
	}
}

func TestAnInvalidChannelNeverReachesStorage(t *testing.T) {
	alerts, repo, _, reevaluations := newAlerts(t)

	_, err := alerts.AddChannel(context.Background(), domain.ChannelWebhook, "ops",
		domain.ChannelConfig{URL: "https://example.test/hook"}, false)
	if !errors.Is(err, domain.ErrInvalidAlert) {
		t.Fatalf("an unsigned webhook was accepted: %v", err)
	}
	if channels, _ := repo.ListChannels(context.Background()); len(channels) != 0 {
		t.Error("the invalid channel was stored")
	}
	if *reevaluations != 0 {
		t.Error("a failed write woke the scheduler")
	}
}

func TestTestChannelDeliversImmediately(t *testing.T) {
	alerts, repo, sender, _ := newAlerts(t)
	ctx := context.Background()
	channel := repo.addChannel("ops")

	if err := alerts.TestChannel(ctx, channel.ID); err != nil {
		t.Fatalf("testing a channel: %v", err)
	}
	delivered := sender.delivered()
	if len(delivered) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(delivered))
	}
	// A test message indistinguishable from a real alert, arriving in a
	// channel other people watch, is how somebody investigates an outage that
	// never happened.
	if !strings.Contains(strings.ToLower(delivered[0].Text()), "test") {
		t.Errorf("the test message does not say it is one:\n%s", delivered[0].Text())
	}
	// And it never goes through the outbox: the point is an answer now, not a
	// row that will be retried for eight hours.
	if len(repo.notifications()) != 0 {
		t.Error("a test was queued instead of sent")
	}
}

func TestTestChannelReportsAFailure(t *testing.T) {
	alerts, repo, sender, _ := newAlerts(t)
	channel := repo.addChannel("ops")
	sender.refuse(errRefused)

	err := alerts.TestChannel(context.Background(), channel.ID)
	if err == nil {
		t.Fatal("a refused delivery was reported as a success")
	}
	if !strings.Contains(err.Error(), "ops") {
		t.Errorf("error %q does not name the channel", err)
	}

	if err := alerts.TestChannel(context.Background(), 999); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("testing a channel that does not exist gave %v", err)
	}
}

// TestTestRuleReportsEveryChannel: "it works except for Discord" is the answer
// somebody needs, and returning the first error would report it as "it does
// not work".
func TestTestRuleReportsEveryChannel(t *testing.T) {
	alerts, repo, sender, _ := newAlerts(t)
	ctx := context.Background()

	first := repo.addChannel("slack")
	second := repo.addChannel("discord")
	stored := repo.addRule(rule(t, `{"kind":"new_issue"}`, []int64{first.ID, second.ID, 999}, 0))

	results, err := alerts.TestRule(ctx, stored.ID)
	if err != nil {
		t.Fatalf("testing a rule: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want one per channel", len(results))
	}
	if !results[0].OK || !results[1].OK {
		t.Errorf("a working channel was reported as broken: %+v", results)
	}
	// The third names a channel that no longer exists, and the two before it
	// still delivered.
	if results[2].OK || results[2].Error == "" {
		t.Errorf("a missing channel was reported as fine: %+v", results[2])
	}
	if len(sender.delivered()) != 2 {
		t.Errorf("delivered %d messages, want 2", len(sender.delivered()))
	}

	if _, err := alerts.TestRule(ctx, 999); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("testing a rule that does not exist gave %v", err)
	}
}

func TestNotificationsFilterIsValidated(t *testing.T) {
	alerts, _, _, _ := newAlerts(t)
	ctx := context.Background()

	if _, err := alerts.Notifications(ctx, "sending", 10); !errors.Is(err, domain.ErrInvalidAlert) {
		t.Errorf("an unknown status was accepted: %v", err)
	}
	if _, err := alerts.Notifications(ctx, "", 10); err != nil {
		t.Errorf("an empty status was rejected: %v", err)
	}
	if _, err := alerts.Notifications(ctx, string(domain.NotificationDead), 10); err != nil {
		t.Errorf("a known status was rejected: %v", err)
	}
}

func TestRulesAndChannelsRoundTripThroughTheUseCase(t *testing.T) {
	alerts, repo, _, _ := newAlerts(t)
	ctx := context.Background()
	channel := repo.addChannel("ops")

	trigger, err := domain.ParseTrigger([]byte(`{"kind":"new_issue"}`))
	if err != nil {
		t.Fatalf("the fixture trigger does not parse: %v", err)
	}
	created, err := alerts.AddRule(ctx, nil, "deploys", trigger, []int64{channel.ID}, time.Minute, true)
	if err != nil {
		t.Fatalf("adding a rule: %v", err)
	}

	read, err := alerts.Rule(ctx, created.ID)
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if read.Name != "deploys" {
		t.Errorf("the rule came back as %+v", read)
	}
	if rules, _ := alerts.Rules(ctx, nil); len(rules) != 1 {
		t.Errorf("listing returned %d rules", len(rules))
	}
	if _, err := alerts.Channel(ctx, channel.ID); err != nil {
		t.Errorf("reading a channel: %v", err)
	}
	if channels, _ := alerts.Channels(ctx); len(channels) != 1 {
		t.Error("listing channels returned the wrong count")
	}
	if err := alerts.RemoveRule(ctx, created.ID); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if _, err := alerts.Rule(ctx, created.ID); !errors.Is(err, domain.ErrAlertNotFound) {
		t.Errorf("a removed rule is still readable: %v", err)
	}
}

func TestRetryGoesThroughTheRepository(t *testing.T) {
	alerts, repo, _, _ := newAlerts(t)
	ctx := context.Background()

	channel := repo.addChannel("ops")
	repo.addRule(rule(t, `{"kind":"new_issue"}`, []int64{channel.ID}, 0))
	if _, err := repo.Enqueue(ctx, &domain.AlertEvent{
		Kind: domain.TriggerNewIssue, ProjectID: 1, IssueID: 2, At: alertNow,
	}, "url"); err != nil {
		t.Fatalf("enqueueing: %v", err)
	}
	queued := repo.notifications()
	if len(queued) != 1 {
		t.Fatalf("queued %d rows", len(queued))
	}

	revived, err := alerts.Retry(ctx, queued[0].ID)
	if err != nil {
		t.Fatalf("retrying: %v", err)
	}
	if revived.Status != domain.NotificationPending {
		t.Errorf("the revived row is %q", revived.Status)
	}
}

var _ ports.AlertRepository = (*fakeAlerts)(nil)
var _ ports.AlertSender = (*fakeSender)(nil)
