package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// fakeAlerts is an in-memory ports.AlertRepository.
//
// The SQLite implementation is tested against a real database (ADR 009); this
// exists so the use-case tests can drive the decisions — when a detector
// fires, what the notifier does with a failure — without a schema in the way.
type fakeAlerts struct {
	mu sync.Mutex

	channels map[int64]domain.AlertChannel
	rules    []domain.AlertRule
	queue    []domain.Notification
	state    map[string]time.Time
	hourly   []int64

	nextID   int64
	enqueued []domain.AlertEvent
	// failWith makes every read fail, for the paths that must not take an
	// error as a "no".
	failWith error
	pruned   int64
}

func newFakeAlerts() *fakeAlerts {
	return &fakeAlerts{
		channels: map[int64]domain.AlertChannel{},
		state:    map[string]time.Time{},
	}
}

func (f *fakeAlerts) addChannel(name string) domain.AlertChannel {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	channel := domain.AlertChannel{
		ID: f.nextID, Type: domain.ChannelWebhook, Name: name,
		Config: domain.ChannelConfig{URL: "https://example.test/hook", Secret: "a-secret-long-enough"},
	}
	f.channels[channel.ID] = channel
	return channel
}

func (f *fakeAlerts) addRule(rule domain.AlertRule) domain.AlertRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	rule.ID = f.nextID
	f.rules = append(f.rules, rule)
	return rule
}

func (f *fakeAlerts) CreateChannel(_ context.Context, channel *domain.AlertChannel) (domain.AlertChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	saved := *channel
	saved.ID = f.nextID
	f.channels[saved.ID] = saved
	return saved, nil
}

func (f *fakeAlerts) ListChannels(context.Context) ([]domain.AlertChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	channels := make([]domain.AlertChannel, 0, len(f.channels))
	for _, channel := range f.channels {
		channels = append(channels, channel)
	}
	return channels, nil
}

func (f *fakeAlerts) FindChannel(_ context.Context, id int64) (domain.AlertChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	channel, found := f.channels[id]
	if !found {
		return domain.AlertChannel{}, fmt.Errorf("%w: no channel %d", domain.ErrAlertNotFound, id)
	}
	return channel, nil
}

func (f *fakeAlerts) DeleteChannel(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, found := f.channels[id]; !found {
		return fmt.Errorf("%w: no channel %d", domain.ErrAlertNotFound, id)
	}
	delete(f.channels, id)
	return nil
}

func (f *fakeAlerts) HasChannels(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.channels) > 0, nil
}

func (f *fakeAlerts) CreateRule(_ context.Context, rule domain.AlertRule) (domain.AlertRule, error) {
	return f.addRule(rule), nil
}

func (f *fakeAlerts) ListRules(_ context.Context, projectID *int64) ([]domain.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if projectID == nil {
		return append([]domain.AlertRule(nil), f.rules...), nil
	}
	var matching []domain.AlertRule
	for _, rule := range f.rules {
		if rule.AppliesTo(*projectID) {
			matching = append(matching, rule)
		}
	}
	return matching, nil
}

func (f *fakeAlerts) FindRule(_ context.Context, id int64) (domain.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rule := range f.rules {
		if rule.ID == id {
			return rule, nil
		}
	}
	return domain.AlertRule{}, fmt.Errorf("%w: no rule %d", domain.ErrAlertNotFound, id)
}

func (f *fakeAlerts) DeleteRule(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index, rule := range f.rules {
		if rule.ID == id {
			f.rules = append(f.rules[:index], f.rules[index+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: no rule %d", domain.ErrAlertNotFound, id)
}

func (f *fakeAlerts) RulesOfKind(
	_ context.Context, kind domain.TriggerKind, projectID int64,
) ([]domain.AlertRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	var matching []domain.AlertRule
	for _, rule := range f.rules {
		if rule.Trigger.Kind == kind && rule.AppliesTo(projectID) {
			matching = append(matching, rule)
		}
	}
	return matching, nil
}

func (f *fakeAlerts) Watches(_ context.Context, kind domain.TriggerKind) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return false, f.failWith
	}
	for _, rule := range f.rules {
		if rule.Trigger.Kind == kind && rule.Enabled {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAlerts) Enqueue(_ context.Context, event *domain.AlertEvent, issueURL string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return 0, f.failWith
	}
	f.enqueued = append(f.enqueued, *event)

	written := 0
	for _, rule := range f.rules {
		if !rule.Matches(event) {
			continue
		}
		key := fmt.Sprintf("%d/%s", rule.ID, event.SubjectKey())
		if domain.Silenced(f.state[key], rule.Silence(), event.At) {
			continue
		}
		f.state[key] = event.At
		for _, channelID := range rule.ChannelIDs {
			f.nextID++
			f.queue = append(f.queue, domain.Notification{
				ID: f.nextID, RuleID: rule.ID, ChannelID: channelID,
				SubjectKey: event.SubjectKey(),
				Payload:    domain.NewAlertPayload(rule.Name, event, issueURL),
				Status:     domain.NotificationPending, NextAttemptAt: event.At, CreatedAt: event.At,
			})
			written++
		}
	}
	return written, nil
}

func (f *fakeAlerts) Claim(_ context.Context, now time.Time, limit int) ([]ports.PendingNotification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	var claimed []ports.PendingNotification
	for index := range f.queue {
		notification := &f.queue[index]
		if notification.Status != domain.NotificationPending && notification.Status != domain.NotificationFailed {
			continue
		}
		if notification.NextAttemptAt.After(now) {
			continue
		}
		notification.Attempts++
		notification.NextAttemptAt = now.Add(domain.RetryDelay(notification.Attempts))
		channel, found := f.channels[notification.ChannelID]
		if !found {
			notification.Status = domain.NotificationDead
			continue
		}
		claimed = append(claimed, ports.PendingNotification{
			Notification: *notification, Channel: channel,
			DeliveryID: fmt.Sprintf("delivery-%d", notification.ID),
		})
		if len(claimed) >= limit {
			break
		}
	}
	return claimed, nil
}

func (f *fakeAlerts) MarkSent(_ context.Context, id int64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.queue {
		if f.queue[index].ID == id {
			sent := at
			f.queue[index].Status, f.queue[index].SentAt = domain.NotificationSent, &sent
			f.queue[index].LastError = ""
			return nil
		}
	}
	return domain.ErrAlertNotFound
}

func (f *fakeAlerts) MarkFailed(_ context.Context, id int64, _ time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.queue {
		if f.queue[index].ID != id {
			continue
		}
		f.queue[index].LastError = reason
		if f.queue[index].Attempts >= domain.MaxNotificationAttempts {
			f.queue[index].Status = domain.NotificationDead
		} else {
			f.queue[index].Status = domain.NotificationFailed
		}
		return nil
	}
	return domain.ErrAlertNotFound
}

func (f *fakeAlerts) ListNotifications(
	_ context.Context, filter ports.NotificationFilter,
) ([]domain.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matching []domain.Notification
	for _, notification := range f.queue {
		if filter.Status != "" && notification.Status != filter.Status {
			continue
		}
		matching = append(matching, notification)
	}
	return matching, nil
}

func (f *fakeAlerts) RetryNotification(_ context.Context, id int64, now time.Time) (domain.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.queue {
		if f.queue[index].ID != id || f.queue[index].Status == domain.NotificationSent {
			continue
		}
		f.queue[index].Status = domain.NotificationPending
		f.queue[index].Attempts, f.queue[index].NextAttemptAt = 0, now
		return f.queue[index], nil
	}
	return domain.Notification{}, domain.ErrAlertNotFound
}

func (f *fakeAlerts) PruneNotifications(_ context.Context, _ time.Time, _ int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned++
	return f.pruned, nil
}

func (f *fakeAlerts) IssueHourlyCounts(
	_ context.Context, _, _ int64, _ time.Time, hours int,
) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	counts := make([]int64, hours)
	// The fixture is read from the end, so a test writes the history it cares
	// about and the current hour is always the last element.
	for index := range counts {
		source := len(f.hourly) - hours + index
		if source >= 0 && source < len(f.hourly) {
			counts[index] = f.hourly[source]
		}
	}
	return counts, nil
}

func (f *fakeAlerts) events() []domain.AlertEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AlertEvent(nil), f.enqueued...)
}

func (f *fakeAlerts) notifications() []domain.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Notification(nil), f.queue...)
}

// fakeSender records what was delivered and can be told to refuse.
type fakeSender struct {
	mu       sync.Mutex
	sent     []domain.AlertPayload
	channels []int64
	err      error
}

func (s *fakeSender) Send(
	_ context.Context, channel *domain.AlertChannel, payload *domain.AlertPayload, _ string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, *payload)
	s.channels = append(s.channels, channel.ID)
	return nil
}

func (s *fakeSender) delivered() []domain.AlertPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.AlertPayload(nil), s.sent...)
}

func (s *fakeSender) refuse(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

var errRefused = errors.New("connection refused")
