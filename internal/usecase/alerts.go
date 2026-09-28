package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// Alerts is the alerting subsystem seen from outside: channels, rules, the
// delivery log, and the two detectors the ingest path drives.
//
// The outbox is deliberately not here. A notification is written by the
// transaction that produced the event it is about, which happens inside the
// repository (ADR 015); this type is what a person or an agent talks to.
type Alerts struct {
	repo   ports.AlertRepository
	sender ports.AlertSender
	clock  ports.Clock
	origin domain.Origin
	// rates is the in-memory sliding window behind the error_rate trigger.
	// In memory because it has to be read before the rate limiter, on every
	// event, including the ones the limiter is about to refuse — and a
	// database write per refused event would make a flood more expensive to
	// reject than to accept, which is the opposite of what a limiter is for
	// (ADR 005).
	rates *rateWindow
	// spikes throttles how often one issue's history is re-read.
	spikes *evaluationClock
	// channelsChanged is called after a channel is added or removed, and is
	// how the scheduler learns that the notifier now has — or no longer has —
	// anything to do.
	channelsChanged func()
}

// NewAlerts wires the use case.
//
// channelsChanged is a positional argument rather than an optional setter on
// purpose. It is the same seam that was once a callback somebody had to
// remember to wire, which worked in production and silently did not in a test
// that assembled differently (ADR 005, ADR 014). A parameter cannot be
// forgotten: passing nil is a decision, and it is what a test that does not
// run a scheduler passes.
func NewAlerts(
	repo ports.AlertRepository, sender ports.AlertSender, clock ports.Clock,
	origin domain.Origin, channelsChanged func(),
) *Alerts {
	if channelsChanged == nil {
		channelsChanged = func() {}
	}
	return &Alerts{
		repo:            repo,
		sender:          sender,
		clock:           clock,
		origin:          origin,
		rates:           newRateWindow(),
		spikes:          newEvaluationClock(spikeEvaluationInterval),
		channelsChanged: channelsChanged,
	}
}

// AddChannel validates and stores a destination.
func (a *Alerts) AddChannel(
	ctx context.Context, channelType domain.ChannelType, name string,
	config domain.ChannelConfig, digest bool,
) (domain.AlertChannel, error) {
	channel, err := domain.NewAlertChannel(channelType, name, config, digest, a.clock.Now())
	if err != nil {
		return domain.AlertChannel{}, err
	}
	saved, err := a.repo.CreateChannel(ctx, &channel)
	if err != nil {
		return domain.AlertChannel{}, err
	}
	// The first channel is what brings the notifier into existence, and it
	// has to happen while the person who added it is still looking at the
	// screen — not at the next restart (ADR 014).
	a.channelsChanged()
	return saved, nil
}

// Channels lists every destination.
func (a *Alerts) Channels(ctx context.Context) ([]domain.AlertChannel, error) {
	return a.repo.ListChannels(ctx)
}

// Channel reads one destination.
func (a *Alerts) Channel(ctx context.Context, id int64) (domain.AlertChannel, error) {
	return a.repo.FindChannel(ctx, id)
}

// RemoveChannel deletes a destination.
func (a *Alerts) RemoveChannel(ctx context.Context, id int64) error {
	if err := a.repo.DeleteChannel(ctx, id); err != nil {
		return err
	}
	// And removing the last one gives the goroutine back.
	a.channelsChanged()
	return nil
}

// HasChannels reports whether anything is configured. This is what the
// scheduler asks before the notifier job exists (ADR 014).
func (a *Alerts) HasChannels(ctx context.Context) (bool, error) {
	return a.repo.HasChannels(ctx)
}

// TestChannel delivers a sample message, now, and reports what happened.
//
// Deliberately not through the outbox. The point of a test is to answer "is
// this configured correctly" while the person who typed the configuration is
// still looking at it, and a queued row that will be retried for eight hours
// answers that question with silence.
func (a *Alerts) TestChannel(ctx context.Context, id int64) error {
	channel, err := a.repo.FindChannel(ctx, id)
	if err != nil {
		return err
	}
	delivery, err := domain.NewDeliveryID()
	if err != nil {
		return err
	}
	sample := a.samplePayload("test", "")
	if err := a.sender.Send(ctx, &channel, &sample, delivery); err != nil {
		return fmt.Errorf("testing channel %d (%s): %w", channel.ID, channel.Name, err)
	}
	return nil
}

// AddRule validates and stores a rule.
func (a *Alerts) AddRule(
	ctx context.Context, projectID *int64, name string, trigger domain.Trigger,
	channelIDs []int64, silence time.Duration, enabled bool,
) (domain.AlertRule, error) {
	rule, err := domain.NewAlertRule(projectID, name, trigger, channelIDs, silence, enabled)
	if err != nil {
		return domain.AlertRule{}, err
	}
	return a.repo.CreateRule(ctx, rule)
}

// Rules lists the rules covering a project, or every rule.
func (a *Alerts) Rules(ctx context.Context, projectID *int64) ([]domain.AlertRule, error) {
	return a.repo.ListRules(ctx, projectID)
}

// Rule reads one rule.
func (a *Alerts) Rule(ctx context.Context, id int64) (domain.AlertRule, error) {
	return a.repo.FindRule(ctx, id)
}

// RemoveRule deletes a rule.
func (a *Alerts) RemoveRule(ctx context.Context, id int64) error {
	return a.repo.DeleteRule(ctx, id)
}

// ChannelResult is what one channel did with a test message.
type ChannelResult struct {
	// ChannelID is which channel this is about.
	ChannelID int64 `json:"channel_id"`
	// Name is the channel's name, so a caller does not need a second request
	// to say which one failed.
	Name string `json:"name"`
	// Type is the channel's kind.
	Type domain.ChannelType `json:"type"`
	// OK is whether the message was delivered.
	OK bool `json:"ok"`
	// Error is why it was not.
	Error string `json:"error,omitempty"`
}

// TestRule sends a synthetic alert through every channel the rule names.
//
// Every channel is tried even after one fails, and the failures are reported
// per channel rather than as the first error: "the rule works except for
// Discord" is the answer, and returning early would report it as "the rule
// does not work".
func (a *Alerts) TestRule(ctx context.Context, id int64) ([]ChannelResult, error) {
	rule, err := a.repo.FindRule(ctx, id)
	if err != nil {
		return nil, err
	}
	payload := a.samplePayload(rule.Name, rule.Trigger.Kind)

	results := make([]ChannelResult, 0, len(rule.ChannelIDs))
	for _, channelID := range rule.ChannelIDs {
		result := ChannelResult{ChannelID: channelID}
		channel, err := a.repo.FindChannel(ctx, channelID)
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		result.Name, result.Type = channel.Name, channel.Type

		delivery, err := domain.NewDeliveryID()
		if err != nil {
			return nil, err
		}
		if err := a.sender.Send(ctx, &channel, &payload, delivery); err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		result.OK = true
		results = append(results, result)
	}
	return results, nil
}

// samplePayload is the synthetic issue a test sends.
//
// It says plainly that it is a test. A message indistinguishable from a real
// alert, arriving in a channel other people also watch, is how somebody ends
// up investigating an outage that never happened.
func (a *Alerts) samplePayload(rule string, kind domain.TriggerKind) domain.AlertPayload {
	if kind == "" {
		kind = domain.TriggerNewIssue
	}
	return domain.NewAlertPayload(rule, &domain.AlertEvent{
		Kind:        kind,
		ProjectID:   0,
		IssueID:     0,
		Title:       "Test alert from trapline",
		Culprit:     "nothing is broken; this is a delivery test",
		Level:       domain.Level("info"),
		Environment: "test",
		Count:       1,
		At:          a.clock.Now(),
	}, a.origin.String())
}

// Notifications reads the delivery log.
func (a *Alerts) Notifications(
	ctx context.Context, status string, limit int,
) ([]domain.Notification, error) {
	filter := ports.NotificationFilter{Limit: limit}
	if status != "" {
		parsed := domain.NotificationStatus(status)
		if !parsed.Valid() {
			return nil, fmt.Errorf("%w: unknown status %q", domain.ErrInvalidAlert, status)
		}
		filter.Status = parsed
	}
	return a.repo.ListNotifications(ctx, filter)
}

// Retry puts a notification back at the front of the queue.
func (a *Alerts) Retry(ctx context.Context, id int64) (domain.Notification, error) {
	return a.repo.RetryNotification(ctx, id, a.clock.Now())
}
