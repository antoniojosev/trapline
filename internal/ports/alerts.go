package ports

import (
	"context"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// PendingNotification is one queued delivery with the channel it goes to,
// already decrypted.
//
// The channel travels with the notification rather than being looked up by
// the sender, so one pass of the notifier is one query however many rows it
// carries, and so the delivery path never has to decide what to do about a
// channel that was deleted between the enqueue and the send.
type PendingNotification struct {
	// Notification is the queued row.
	Notification domain.Notification
	// Channel is where it goes.
	Channel domain.AlertChannel
	// DeliveryID is the value of X-Trapline-Delivery. It is minted once, when
	// the notification is enqueued, and every retry of that notification
	// carries the same one — which is what makes it usable as an idempotency
	// key by a receiver.
	DeliveryID string
}

// NotificationFilter selects rows from the delivery log.
type NotificationFilter struct {
	// Status is optional; empty means every status.
	Status domain.NotificationStatus
	// Limit bounds the page.
	Limit int
}

// AlertRepository stores channels, rules, the silence state and the outbox.
//
// The four are one port rather than four because they are one transaction: an
// event that matches a rule has to read the rule, read the silence, write the
// silence and write the notifications atomically, or a crash in the middle
// either notifies twice or goes quiet forever (ADR 015).
type AlertRepository interface {
	// CreateChannel stores a channel, encrypting its configuration.
	CreateChannel(ctx context.Context, channel *domain.AlertChannel) (domain.AlertChannel, error)
	// ListChannels returns every channel, oldest first.
	ListChannels(ctx context.Context) ([]domain.AlertChannel, error)
	// FindChannel returns one channel, or domain.ErrAlertNotFound.
	FindChannel(ctx context.Context, id int64) (domain.AlertChannel, error)
	// DeleteChannel removes a channel and drops it from every rule that named
	// it. A rule left pointing at nothing would look configured and notify
	// nobody, which is the worst state this subsystem has.
	DeleteChannel(ctx context.Context, id int64) error
	// HasChannels reports whether any channel is configured. This is the
	// question the scheduler asks before the notifier job exists at all
	// (ADR 014).
	HasChannels(ctx context.Context) (bool, error)

	// CreateRule stores a rule.
	CreateRule(ctx context.Context, rule domain.AlertRule) (domain.AlertRule, error)
	// ListRules returns the rules covering a project, or every rule when
	// projectID is nil.
	ListRules(ctx context.Context, projectID *int64) ([]domain.AlertRule, error)
	// FindRule returns one rule, or domain.ErrAlertNotFound.
	FindRule(ctx context.Context, id int64) (domain.AlertRule, error)
	// DeleteRule removes a rule and the silence state that belonged to it.
	DeleteRule(ctx context.Context, id int64) error

	// RulesOfKind returns the enabled rules of one kind that cover a project.
	// It is answered from a cached snapshot, because the ingest path asks it
	// once per event and a subsystem nobody configured must not cost a query
	// per event (ADR 005).
	RulesOfKind(ctx context.Context, kind domain.TriggerKind, projectID int64) ([]domain.AlertRule, error)
	// Watches reports whether any enabled rule reacts to a kind at all. Same
	// cache, and it is the first thing every detector asks: on an
	// installation with no alerting it is the only thing they do.
	Watches(ctx context.Context, kind domain.TriggerKind) (bool, error)

	// Enqueue offers one domain event to the rules and writes the
	// notifications it produces, in one transaction. It returns how many rows
	// it wrote, which is zero both when nothing matched and when the silence
	// window had not expired.
	Enqueue(ctx context.Context, event *domain.AlertEvent, issueURL string) (int, error)

	// Claim leases the notifications whose next attempt has come, pushing
	// their next attempt into the future so a second pass cannot take them.
	Claim(ctx context.Context, now time.Time, limit int) ([]PendingNotification, error)
	// MarkSent records a delivery.
	MarkSent(ctx context.Context, id int64, at time.Time) error
	// MarkFailed records a failed attempt, and declares the row dead once it
	// has run out of attempts.
	MarkFailed(ctx context.Context, id int64, at time.Time, reason string) error
	// ListNotifications reads the delivery log, newest first.
	ListNotifications(ctx context.Context, filter NotificationFilter) ([]domain.Notification, error)
	// RetryNotification puts a row back at the front of the queue with its
	// attempt budget restored, which is what an operator asking for a retry
	// means.
	RetryNotification(ctx context.Context, id int64, now time.Time) (domain.Notification, error)

	// PruneNotifications deletes delivered notifications older than cutoff,
	// at most limit per call. The outbox is a log, and a log with no end is a
	// disk that fills.
	PruneNotifications(ctx context.Context, cutoff time.Time, limit int) (int64, error)

	// IssueHourlyCounts returns one issue's events per hour over the last
	// `hours` whole hours ending with the hour containing `at`, oldest first
	// and with absent hours present as zero. It reads the aggregates of
	// ADR 010 and never the events (ADR 001), and it exists so a spike can be
	// judged against the issue's own past rather than a number somebody
	// guessed.
	IssueHourlyCounts(ctx context.Context, projectID, issueID int64, at time.Time, hours int) ([]int64, error)
}

// AlertSender delivers one payload through one channel.
//
// Declared here rather than in the notify adapter so the use case depends on
// the capability and not on the package that happens to implement it: the
// delivery gate substitutes its own, and so does every test that would
// otherwise need five live services.
type AlertSender interface {
	Send(ctx context.Context, channel *domain.AlertChannel, payload *domain.AlertPayload, deliveryID string) error
}
