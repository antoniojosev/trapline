package usecase

import (
	"context"
	"log/slog"
	"time"

	"github.com/antoniojosev/trapline/internal/ports"
)

// The notifier's own numbers.
const (
	// NotifierInterval is how often the outbox is drained.
	//
	// One second, and the reason is that this interval *is* the delivery
	// latency: a notification is written by the transaction that produced the
	// event, so everything between "it broke" and "you were told" is this
	// tick. The promise alerting makes is under a minute end to end, and a tick
	// measured in tens of seconds would spend the whole budget waiting.
	//
	// What it costs is one indexed lookup per second against a table whose
	// steady state is rows that are already sent — and only on an
	// installation that has configured a channel, because otherwise the job
	// does not exist at all (ADR 014).
	NotifierInterval = time.Second
	// NotifierBatch bounds one pass. Deliveries within a pass are sequential,
	// so this is also the ceiling on how long one pass can take.
	NotifierBatch = 20
	// NotificationKeep is how long a delivered notification stays in the log.
	NotificationKeep = 30 * 24 * time.Hour
	// pruneInterval is how often the log is swept.
	pruneInterval = time.Hour
	// pruneBatch bounds one sweep, so it never holds a long write transaction
	// on a live store — the same rule retention follows.
	pruneBatch = 500
)

// Notifier drains the outbox.
//
// It is the only thing that sends. Everything upstream of it writes rows and
// stops, which is what makes "the server restarted mid-incident" survivable:
// the decision to notify is durable before the attempt to notify begins.
type Notifier struct {
	repo   ports.AlertRepository
	sender ports.AlertSender
	clock  ports.Clock
	log    *slog.Logger

	// lastPrune keeps the log sweep off the once-a-second path. It is in
	// memory because losing it costs one extra sweep after a restart.
	lastPrune time.Time
}

// NewNotifier wires the outbox drain.
func NewNotifier(
	repo ports.AlertRepository, sender ports.AlertSender, clock ports.Clock, logger *slog.Logger,
) *Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{repo: repo, sender: sender, clock: clock, log: logger}
}

// DeliveryResult is what one pass did.
type DeliveryResult struct {
	// Sent is how many notifications were delivered.
	Sent int
	// Failed is how many attempts failed and will be retried, or were
	// declared dead.
	Failed int
	// Pruned is how many delivered rows were dropped from the log.
	Pruned int64
}

// Deliver drains one batch of the outbox.
//
// A failed delivery is recorded and the pass continues. One unreachable
// endpoint must not hold up the other four channels of the same alert, and it
// must not stop the alert after it either — the whole reason the outbox is a
// table and not a channel is that failures here are expected and survivable.
func (n *Notifier) Deliver(ctx context.Context) (DeliveryResult, error) {
	now := n.clock.Now()

	pending, err := n.repo.Claim(ctx, now, NotifierBatch)
	if err != nil {
		return DeliveryResult{}, err
	}

	var result DeliveryResult
	for index := range pending {
		item := &pending[index]
		err := n.sender.Send(ctx, &item.Channel, &item.Notification.Payload, item.DeliveryID)
		if err == nil {
			if markErr := n.repo.MarkSent(ctx, item.Notification.ID, n.clock.Now()); markErr != nil {
				// The message went out and this process could not write that
				// down. Reported loudly, because the visible consequence is a
				// duplicate on the next pass, and somebody looking at two
				// identical Slack messages deserves to find this line.
				n.log.Error("delivered a notification but could not record it",
					"notification_id", item.Notification.ID, "error", markErr)
			}
			result.Sent++
			continue
		}

		result.Failed++
		n.log.Warn("could not deliver a notification",
			"notification_id", item.Notification.ID,
			"channel", item.Channel.Name,
			"attempts", item.Notification.Attempts,
			"error", err)
		if markErr := n.repo.MarkFailed(ctx, item.Notification.ID, n.clock.Now(), err.Error()); markErr != nil {
			n.log.Error("could not record a failed delivery",
				"notification_id", item.Notification.ID, "error", markErr)
		}
	}

	if pruned, err := n.prune(ctx, now); err != nil {
		// A log that could not be swept is not a reason to report the pass as
		// failed: everything that mattered was delivered.
		n.log.Error("could not prune the notification log", "error", err)
	} else {
		result.Pruned = pruned
	}
	return result, nil
}

func (n *Notifier) prune(ctx context.Context, now time.Time) (int64, error) {
	if !n.lastPrune.IsZero() && now.Sub(n.lastPrune) < pruneInterval {
		return 0, nil
	}
	n.lastPrune = now
	return n.repo.PruneNotifications(ctx, now.Add(-NotificationKeep), pruneBatch)
}

// Job is the notifier seen through the scheduler's contract.
func (n *Notifier) Job() NotifierJob { return NotifierJob{notifier: n} }

// NotifierJob drains the outbox on a schedule.
//
// Like RetentionJob it names no scheduler type: the contract is structural, so
// the use cases do not depend on the thing that runs them (ADR 014).
type NotifierJob struct{ notifier *Notifier }

// Name identifies the job wherever it is reported.
func (j NotifierJob) Name() string { return "notifier" }

// Interval is how long the scheduler waits between passes.
func (j NotifierJob) Interval() time.Duration { return NotifierInterval }

// Run drains one batch.
func (j NotifierJob) Run(ctx context.Context) error {
	result, err := j.notifier.Deliver(ctx)
	if err != nil {
		return err
	}
	if result.Sent > 0 || result.Failed > 0 || result.Pruned > 0 {
		// Only when something happened. A line per second saying nothing
		// happened would bury the ones that say something did.
		j.notifier.log.Info("notifications",
			"sent", result.Sent, "failed", result.Failed, "pruned", result.Pruned)
	}
	return nil
}
