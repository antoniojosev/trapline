package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/secrets"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var (
	_ ports.AlertRepository = (*AlertRepository)(nil)
	_ ports.AlertChannels   = (*AlertRepository)(nil)
)

// queryer is what the enqueue path needs: a database or a transaction.
//
// It exists because the same code runs in two places. A new issue is
// discovered inside the ingest transaction and its notification has to be
// written there, in that transaction (ADR 015); a spike or a rate crossing is
// detected afterwards and writes in a transaction of its own. One
// implementation, two callers, and neither of them can accidentally get the
// non-transactional version.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AlertRepository stores channels, rules, silence state and the outbox.
type AlertRepository struct {
	db     *DB
	cipher *secrets.Cipher

	// rules is a cached snapshot of every enabled rule.
	//
	// It is here for the ingest path, which asks "does any rule care about a
	// new issue?" once per event. Reading that from SQLite per event would be
	// a query on the hot path for a subsystem most installations have not
	// configured, which is the cost ADR 005 exists to refuse. The cache is
	// invalidated by the only code that can change the answer — the writes on
	// this same type — so, like the rate limiter's, it cannot be assembled in
	// a way that forgets to invalidate.
	mu     sync.RWMutex
	rules  []domain.AlertRule
	loaded bool
}

// NewAlertRepository wires the repository to an open database and the cipher
// that protects the credentials in it.
func NewAlertRepository(db *DB, cipher *secrets.Cipher) *AlertRepository {
	return &AlertRepository{db: db, cipher: cipher}
}

// invalidate drops the cached rules. Called by every write that could change
// which rules exist or which channels they can reach.
func (r *AlertRepository) invalidate() {
	r.mu.Lock()
	r.rules, r.loaded = nil, false
	r.mu.Unlock()
}

// cachedRules returns the enabled rules, loading them once.
func (r *AlertRepository) cachedRules(ctx context.Context, q queryer) ([]domain.AlertRule, error) {
	r.mu.RLock()
	if r.loaded {
		rules := r.rules
		r.mu.RUnlock()
		return rules, nil
	}
	r.mu.RUnlock()

	rules, err := readRules(ctx, q, "WHERE enabled = 1", nil)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.rules, r.loaded = rules, true
	r.mu.Unlock()
	return rules, nil
}

// CreateChannel stores a channel with its configuration encrypted.
func (r *AlertRepository) CreateChannel(
	ctx context.Context, channel *domain.AlertChannel,
) (domain.AlertChannel, error) {
	// The linter is right that this marshals a field called "secret", and that
	// is the point: the JSON goes straight into Seal below and only the
	// ciphertext reaches the column. Serialising it is how it gets encrypted.
	//nolint:gosec // G117: marshalled to be encrypted on the next line, never stored or logged
	plaintext, err := json.Marshal(channel.Config)
	if err != nil {
		return domain.AlertChannel{}, fmt.Errorf("encoding channel configuration: %w", err)
	}
	sealed, err := r.cipher.Seal(plaintext)
	if err != nil {
		return domain.AlertChannel{}, fmt.Errorf("encrypting channel configuration: %w", err)
	}

	result, err := r.db.ExecContext(ctx, `
		INSERT INTO alert_channels (type, name, config_enc, digest, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		string(channel.Type), channel.Name, sealed, boolToInt(channel.Digest), formatTime(channel.CreatedAt))
	if err != nil {
		return domain.AlertChannel{}, fmt.Errorf("inserting channel: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.AlertChannel{}, fmt.Errorf("reading the new channel id: %w", err)
	}
	saved := *channel
	saved.ID = id
	// A new channel can make the notifier's subsystem non-empty, which is the
	// question the scheduler re-asks (ADR 014).
	r.invalidate()
	return saved, nil
}

const channelColumns = `id, type, name, config_enc, digest, created_at`

// ListChannels returns every channel, oldest first.
func (r *AlertRepository) ListChannels(ctx context.Context) ([]domain.AlertChannel, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+channelColumns+" FROM alert_channels ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("listing channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	channels := []domain.AlertChannel{}
	for rows.Next() {
		channel, err := r.scanChannel(rows)
		if err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating channels: %w", err)
	}
	return channels, nil
}

// FindChannel returns one channel.
func (r *AlertRepository) FindChannel(ctx context.Context, id int64) (domain.AlertChannel, error) {
	return r.findChannel(ctx, r.db, id)
}

func (r *AlertRepository) findChannel(ctx context.Context, q queryer, id int64) (domain.AlertChannel, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+channelColumns+" FROM alert_channels WHERE id = ?", id)
	channel, err := r.scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AlertChannel{}, fmt.Errorf("%w: no channel %d", domain.ErrAlertNotFound, id)
	}
	if err != nil {
		return domain.AlertChannel{}, err
	}
	return channel, nil
}

func (r *AlertRepository) scanChannel(row scanner) (domain.AlertChannel, error) {
	var (
		channel   domain.AlertChannel
		rawType   string
		sealed    []byte
		digest    int64
		createdAt string
	)
	if err := row.Scan(&channel.ID, &rawType, &channel.Name, &sealed, &digest, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AlertChannel{}, err //nolint:wrapcheck // the sentinel is the caller's to map
		}
		return domain.AlertChannel{}, fmt.Errorf("reading channel: %w", err)
	}

	plaintext, err := r.cipher.Open(sealed)
	if err != nil {
		// Named with the channel, because the operator's next question is
		// which of their channels are affected — and the answer is all of
		// them, but a message about "channel 3 (slack)" is what makes that
		// obvious rather than abstract.
		return domain.AlertChannel{}, fmt.Errorf("channel %d (%s): %w", channel.ID, channel.Name, err)
	}
	if err := json.Unmarshal(plaintext, &channel.Config); err != nil {
		return domain.AlertChannel{}, fmt.Errorf("decoding channel %d configuration: %w", channel.ID, err)
	}

	channel.Type = domain.ChannelType(rawType)
	channel.Digest = digest != 0
	if channel.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.AlertChannel{}, err
	}
	return channel, nil
}

// DeleteChannel removes a channel, drops it from the rules that named it and
// kills the notifications that were queued for it.
//
// All three in one transaction. A rule still naming a channel that is gone
// would look configured and notify nobody; a pending notification for a
// deleted channel would be retried ten times and then declared dead, which is
// the same outcome reached slowly and with ten misleading log lines.
func (r *AlertRepository) DeleteChannel(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, "DELETE FROM alert_channels WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("deleting channel: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading the delete result: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: no channel %d", domain.ErrAlertNotFound, id)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE notifications
		   SET status = ?, last_error = ?
		 WHERE channel_id = ? AND status IN (?, ?)`,
		string(domain.NotificationDead), "the channel was deleted",
		id, string(domain.NotificationPending), string(domain.NotificationFailed),
	); err != nil {
		return fmt.Errorf("closing the notifications of a deleted channel: %w", err)
	}

	if err := removeChannelFromRules(ctx, tx, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the channel deletion: %w", err)
	}
	r.invalidate()
	return nil
}

// removeChannelFromRules rewrites the channel lists that named a deleted
// channel, and disables the rules left with none.
//
// Disabling rather than deleting: the rule is somebody's configuration and
// deleting it silently would be worse than leaving it visibly switched off,
// which is a state they can see and fix.
func removeChannelFromRules(ctx context.Context, q queryer, channelID int64) error {
	rules, err := readRules(ctx, q, "", nil)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		remaining := make([]int64, 0, len(rule.ChannelIDs))
		for _, id := range rule.ChannelIDs {
			if id != channelID {
				remaining = append(remaining, id)
			}
		}
		if len(remaining) == len(rule.ChannelIDs) {
			continue
		}
		encoded, err := json.Marshal(remaining)
		if err != nil {
			return fmt.Errorf("encoding channel ids: %w", err)
		}
		enabled := rule.Enabled && len(remaining) > 0
		if _, err := q.ExecContext(ctx,
			"UPDATE alert_rules SET channel_ids = ?, enabled = ? WHERE id = ?",
			string(encoded), boolToInt(enabled), rule.ID); err != nil {
			return fmt.Errorf("updating rule %d after a channel was deleted: %w", rule.ID, err)
		}
	}
	return nil
}

// HasChannels reports whether any channel is configured.
func (r *AlertRepository) HasChannels(ctx context.Context) (bool, error) {
	var exists int
	err := r.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM alert_channels)").Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("counting channels: %w", err)
	}
	return exists != 0, nil
}

// Channels describes every channel in the terms the rest of the product is
// allowed to know: where it delivers, whether it asked for the digest, and
// whether the key opened it (ADR 035).
//
// One row that will not decrypt does not fail the call. The whole reason
// `doctor` asks this question is to find the channel whose secret has stopped
// working, and an error return would report "the channel list is broken" for
// an installation whose other four channels are fine — which is both less
// true and less useful than naming the one that is not.
func (r *AlertRepository) Channels(ctx context.Context) ([]ports.AlertChannel, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+channelColumns+" FROM alert_channels ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("listing channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	channels := []ports.AlertChannel{}
	for rows.Next() {
		var (
			id        int64
			rawType   string
			name      string
			sealed    []byte
			digest    int64
			createdAt string
		)
		if err := rows.Scan(&id, &rawType, &name, &sealed, &digest, &createdAt); err != nil {
			return nil, fmt.Errorf("reading channel: %w", err)
		}
		view := ports.AlertChannel{ID: id, Type: rawType, Name: name, Digest: digest != 0}

		channel := domain.AlertChannel{ID: id, Type: domain.ChannelType(rawType), Name: name}
		switch plaintext, err := r.cipher.Open(sealed); {
		case err != nil:
			view.SecretError = err.Error()
		default:
			if err := json.Unmarshal(plaintext, &channel.Config); err != nil {
				view.SecretError = err.Error()
			} else {
				view.Endpoint = channel.Endpoint()
			}
		}
		channels = append(channels, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating channels: %w", err)
	}
	return channels, nil
}

// EnqueueDigest files a rendered weekly report for delivery.
//
// The channel is checked first, and the check is not a formality: the digest
// job reads the channel list, builds a report that takes a moment, and only
// then queues it, so a channel deleted in between would otherwise become a
// row the notifier picks up, fails to resolve and declares dead — a delivery
// failure in the log for something nobody was owed.
func (r *AlertRepository) EnqueueDigest(ctx context.Context, channelID int64, subject, body string) error {
	if _, err := r.findChannel(ctx, r.db, channelID); err != nil {
		return err
	}

	now := time.Now().UTC()
	payload := domain.NewDigestPayload(subject, body, now)
	encoded, err := payload.Encode()
	if err != nil {
		return err
	}
	deliveryID, err := domain.NewDeliveryID()
	if err != nil {
		return err
	}

	// rule_id 0, because no rule produced it. The column has no foreign key
	// precisely so the log can hold rows whose origin is not a rule (see
	// migration 0015), and a channel test already uses the same value.
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO notifications
			(rule_id, channel_id, delivery_id, subject_key, payload, status,
			 attempts, next_attempt_at, last_error, created_at)
		VALUES (0, ?, ?, ?, ?, ?, 0, ?, '', ?)`,
		channelID, deliveryID, digestSubjectKey(now), string(encoded),
		string(domain.NotificationPending), formatTime(now), formatTime(now),
	); err != nil {
		return fmt.Errorf("queueing the weekly digest: %w", err)
	}
	return nil
}

// digestSubjectKey names what a digest row is about, in the same
// `kind:id` shape every other subject key uses (ADR 015).
func digestSubjectKey(at time.Time) string {
	return "digest:" + at.Format("2006-01-02")
}

const ruleColumns = `id, project_id, name, trigger_kind, trigger_config, channel_ids, silence_seconds, enabled`

// CreateRule stores a rule.
func (r *AlertRepository) CreateRule(ctx context.Context, rule domain.AlertRule) (domain.AlertRule, error) {
	trigger, err := rule.Trigger.Encode()
	if err != nil {
		return domain.AlertRule{}, err
	}
	channels, err := json.Marshal(rule.ChannelIDs)
	if err != nil {
		return domain.AlertRule{}, fmt.Errorf("encoding channel ids: %w", err)
	}

	// Every named channel has to exist. A rule pointing at a channel that was
	// never created is the most common way to configure alerting that looks
	// right and delivers nothing, and it costs one query to refuse.
	for _, id := range rule.ChannelIDs {
		if _, err := r.findChannel(ctx, r.db, id); err != nil {
			return domain.AlertRule{}, err
		}
	}

	result, err := r.db.ExecContext(ctx, `
		INSERT INTO alert_rules
			(project_id, name, trigger_kind, trigger_config, channel_ids, silence_seconds, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nullableID(rule.ProjectID), rule.Name, string(rule.Trigger.Kind), string(trigger),
		string(channels), rule.SilenceSeconds, boolToInt(rule.Enabled), formatTime(time.Now()))
	if err != nil {
		return domain.AlertRule{}, fmt.Errorf("inserting rule: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.AlertRule{}, fmt.Errorf("reading the new rule id: %w", err)
	}
	rule.ID = id
	r.invalidate()
	return rule, nil
}

// ListRules returns the rules covering a project, or all of them.
func (r *AlertRepository) ListRules(ctx context.Context, projectID *int64) ([]domain.AlertRule, error) {
	if projectID == nil {
		return readRules(ctx, r.db, "", nil)
	}
	return readRules(ctx, r.db, "WHERE project_id IS NULL OR project_id = ?", []any{*projectID})
}

// FindRule returns one rule.
func (r *AlertRepository) FindRule(ctx context.Context, id int64) (domain.AlertRule, error) {
	rules, err := readRules(ctx, r.db, "WHERE id = ?", []any{id})
	if err != nil {
		return domain.AlertRule{}, err
	}
	if len(rules) == 0 {
		return domain.AlertRule{}, fmt.Errorf("%w: no rule %d", domain.ErrAlertNotFound, id)
	}
	return rules[0], nil
}

// DeleteRule removes a rule and the silence it had accumulated.
func (r *AlertRepository) DeleteRule(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, "DELETE FROM alert_rules WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("deleting rule: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading the delete result: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: no rule %d", domain.ErrAlertNotFound, id)
	}
	// alert_state cascades with the rule: silence is meaningless without the
	// rule it silenced, and a re-created rule with the same id would inherit
	// a quiet period nobody asked for.
	r.invalidate()
	return nil
}

func readRules(ctx context.Context, q queryer, where string, args []any) ([]domain.AlertRule, error) {
	query := "SELECT " + ruleColumns + " FROM alert_rules"
	if where != "" {
		query += " " + where
	}
	query += " ORDER BY id"

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	rules := []domain.AlertRule{}
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rules: %w", err)
	}
	return rules, nil
}

func scanRule(row scanner) (domain.AlertRule, error) {
	var (
		rule      domain.AlertRule
		projectID sql.NullInt64
		kind      string
		trigger   string
		channels  string
		enabled   int64
	)
	if err := row.Scan(&rule.ID, &projectID, &rule.Name, &kind, &trigger, &channels,
		&rule.SilenceSeconds, &enabled); err != nil {
		return domain.AlertRule{}, fmt.Errorf("reading rule: %w", err)
	}
	if projectID.Valid {
		id := projectID.Int64
		rule.ProjectID = &id
	}
	parsed, err := domain.ParseTrigger([]byte(trigger))
	if err != nil {
		return domain.AlertRule{}, fmt.Errorf("rule %d: %w", rule.ID, err)
	}
	rule.Trigger = parsed
	if err := json.Unmarshal([]byte(channels), &rule.ChannelIDs); err != nil {
		return domain.AlertRule{}, fmt.Errorf("rule %d: decoding channel ids: %w", rule.ID, err)
	}
	rule.Enabled = enabled != 0
	return rule, nil
}

// Enqueue offers an event to the rules and writes what it produces.
func (r *AlertRepository) Enqueue(
	ctx context.Context, event *domain.AlertEvent, issueURL string,
) (int, error) {
	// The cheap question first, and off the transaction: if no enabled rule
	// reacts to this kind, there is nothing to open a transaction for. On an
	// installation with no alerting configured this is a slice length.
	interested, err := r.interested(ctx, r.db, event)
	if err != nil {
		return 0, err
	}
	if len(interested) == 0 {
		return 0, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	written, err := r.enqueueTx(ctx, tx, event, issueURL)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing notifications: %w", err)
	}
	return written, nil
}

// interested is the rules that match this event, before the silence is
// consulted.
func (r *AlertRepository) interested(
	ctx context.Context, q queryer, event *domain.AlertEvent,
) ([]domain.AlertRule, error) {
	rules, err := r.cachedRules(ctx, q)
	if err != nil {
		return nil, err
	}
	var matched []domain.AlertRule
	for _, rule := range rules {
		if rule.Matches(event) {
			matched = append(matched, rule)
		}
	}
	return matched, nil
}

// enqueueTx is the whole of "an event became notifications", inside whatever
// transaction the caller is already in.
//
// Read the silence, write the silence, write one row per channel. The order
// matters: the silence is written in the same transaction as the rows it
// authorised, so a crash between them cannot produce a notification the
// silence does not know about, nor a silence with nothing behind it.
func (r *AlertRepository) enqueueTx(
	ctx context.Context, tx *sql.Tx, event *domain.AlertEvent, issueURL string,
) (int, error) {
	interested, err := r.interested(ctx, tx, event)
	if err != nil {
		return 0, err
	}
	if len(interested) == 0 {
		return 0, nil
	}

	subject := event.SubjectKey()
	written := 0
	for _, rule := range interested {
		fired, err := lastFired(ctx, tx, rule.ID, subject)
		if err != nil {
			return 0, err
		}
		if domain.Silenced(fired, rule.Silence(), event.At) {
			continue
		}

		payload := domain.NewAlertPayload(rule.Name, event, issueURL)
		encoded, err := payload.Encode()
		if err != nil {
			return 0, err
		}
		deliveryID, err := domain.NewDeliveryID()
		if err != nil {
			return 0, err
		}

		for _, channelID := range rule.ChannelIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO notifications
					(rule_id, channel_id, delivery_id, subject_key, payload, status,
					 attempts, next_attempt_at, last_error, created_at)
				VALUES (?, ?, ?, ?, ?, ?, 0, ?, '', ?)`,
				rule.ID, channelID, deliveryID, subject, string(encoded),
				string(domain.NotificationPending), formatTime(event.At), formatTime(event.At),
			); err != nil {
				return 0, fmt.Errorf("queueing a notification: %w", err)
			}
			written++
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO alert_state (rule_id, subject_key, last_fired_at)
			VALUES (?, ?, ?)
			ON CONFLICT (rule_id, subject_key) DO UPDATE SET last_fired_at = excluded.last_fired_at`,
			rule.ID, subject, formatTime(event.At)); err != nil {
			return 0, fmt.Errorf("recording that a rule fired: %w", err)
		}
	}
	return written, nil
}

func lastFired(ctx context.Context, q queryer, ruleID int64, subject string) (time.Time, error) {
	var raw string
	err := q.QueryRowContext(ctx,
		"SELECT last_fired_at FROM alert_state WHERE rule_id = ? AND subject_key = ?",
		ruleID, subject).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the silence window: %w", err)
	}
	return parseTime(raw)
}

const notificationColumns = `id, rule_id, channel_id, delivery_id, subject_key, payload, status,
	attempts, next_attempt_at, last_error, created_at, sent_at`

// Claim leases the notifications that are due.
//
// The lease is the next_attempt_at column itself, pushed forward by the same
// backoff a failure would use. That means a process that dies mid-delivery
// does not hold anything: the row simply becomes due again on schedule, and
// no second status is needed to represent "in flight" — a status that, being
// in a database, would need a reaper of its own for the rows a crash left in
// it.
func (r *AlertRepository) Claim(
	ctx context.Context, now time.Time, limit int,
) ([]ports.PendingNotification, error) {
	if limit <= 0 {
		limit = 20
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT `+notificationColumns+`
		  FROM notifications
		 WHERE status IN (?, ?) AND next_attempt_at <= ?
		 ORDER BY next_attempt_at, id
		 LIMIT ?`,
		string(domain.NotificationPending), string(domain.NotificationFailed),
		formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("reading due notifications: %w", err)
	}

	var (
		claimed  []domain.Notification
		delivery = map[int64]string{}
	)
	for rows.Next() {
		notification, deliveryID, err := scanNotification(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		claimed = append(claimed, notification)
		delivery[notification.ID] = deliveryID
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterating due notifications: %w", err)
	}
	_ = rows.Close()

	pending := make([]ports.PendingNotification, 0, len(claimed))
	for index := range claimed {
		notification := &claimed[index]
		notification.Attempts++
		next := now.Add(domain.RetryDelay(notification.Attempts))
		if _, err := tx.ExecContext(ctx,
			"UPDATE notifications SET attempts = ?, next_attempt_at = ? WHERE id = ?",
			notification.Attempts, formatTime(next), notification.ID); err != nil {
			return nil, fmt.Errorf("leasing a notification: %w", err)
		}
		notification.NextAttemptAt = next

		channel, err := r.findChannel(ctx, tx, notification.ChannelID)
		if err != nil {
			// The channel is gone or unreadable. Either way this row will
			// never be delivered, and saying so once is better than ten
			// attempts that all fail for a reason no retry can change.
			if _, markErr := tx.ExecContext(ctx,
				"UPDATE notifications SET status = ?, last_error = ? WHERE id = ?",
				string(domain.NotificationDead), err.Error(), notification.ID); markErr != nil {
				return nil, fmt.Errorf("closing an undeliverable notification: %w", markErr)
			}
			continue
		}
		pending = append(pending, ports.PendingNotification{
			Notification: *notification,
			Channel:      channel,
			DeliveryID:   delivery[notification.ID],
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing the lease: %w", err)
	}
	return pending, nil
}

func scanNotification(row scanner) (domain.Notification, string, error) {
	var (
		notification domain.Notification
		deliveryID   string
		status       string
		payload      string
		nextAttempt  string
		createdAt    string
		sentAt       sql.NullString
	)
	if err := row.Scan(&notification.ID, &notification.RuleID, &notification.ChannelID,
		&deliveryID, &notification.SubjectKey, &payload, &status, &notification.Attempts,
		&nextAttempt, &notification.LastError, &createdAt, &sentAt); err != nil {
		return domain.Notification{}, "", fmt.Errorf("reading notification: %w", err)
	}

	decoded, err := domain.DecodeAlertPayload([]byte(payload))
	if err != nil {
		return domain.Notification{}, "", err
	}
	notification.Payload = decoded
	notification.Status = domain.NotificationStatus(status)
	if notification.NextAttemptAt, err = parseTime(nextAttempt); err != nil {
		return domain.Notification{}, "", err
	}
	if notification.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.Notification{}, "", err
	}
	if sentAt.Valid {
		sent, err := parseTime(sentAt.String)
		if err != nil {
			return domain.Notification{}, "", err
		}
		notification.SentAt = &sent
	}
	return notification, deliveryID, nil
}

// MarkSent records a delivery.
func (r *AlertRepository) MarkSent(ctx context.Context, id int64, at time.Time) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE notifications SET status = ?, sent_at = ?, last_error = '' WHERE id = ?`,
		string(domain.NotificationSent), formatTime(at), id); err != nil {
		return fmt.Errorf("recording a delivery: %w", err)
	}
	return nil
}

// MarkFailed records a failed attempt, declaring the row dead once it has
// spent its attempts.
func (r *AlertRepository) MarkFailed(ctx context.Context, id int64, at time.Time, reason string) error {
	// The status is decided from the attempt count already stored, so two
	// passes cannot disagree about whether the budget is spent.
	if _, err := r.db.ExecContext(ctx, `
		UPDATE notifications
		   SET status = CASE WHEN attempts >= ? THEN ? ELSE ? END,
		       last_error = ?
		 WHERE id = ?`,
		domain.MaxNotificationAttempts, string(domain.NotificationDead),
		string(domain.NotificationFailed), truncateError(reason), id); err != nil {
		return fmt.Errorf("recording a failed delivery: %w", err)
	}
	_ = at
	return nil
}

// maxStoredError bounds what a failing endpoint can write into this database.
// A body is attacker-influenced — it is whatever the far end returned — and an
// error column is not a place to store a megabyte of somebody's HTML.
const maxStoredError = 500

func truncateError(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) <= maxStoredError {
		return reason
	}
	return reason[:maxStoredError] + "…"
}

// ListNotifications reads the delivery log, newest first.
func (r *AlertRepository) ListNotifications(
	ctx context.Context, filter ports.NotificationFilter,
) ([]domain.Notification, error) {
	query := "SELECT " + notificationColumns + " FROM notifications"
	var args []any
	if filter.Status != "" {
		query += " WHERE status = ?"
		args = append(args, string(filter.Status))
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing notifications: %w", err)
	}
	defer func() { _ = rows.Close() }()

	notifications := []domain.Notification{}
	for rows.Next() {
		notification, _, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		notifications = append(notifications, notification)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating notifications: %w", err)
	}
	return notifications, nil
}

// RetryNotification puts a row back at the front of the queue.
//
// The attempt count is reset rather than kept, because an operator pressing
// retry is saying the reason it failed has been dealt with, and a row with
// nine attempts already spent would give that fix one chance instead of ten.
// The delivery id is not reset: a receiver that already saw this delivery
// should still be able to recognise it.
func (r *AlertRepository) RetryNotification(
	ctx context.Context, id int64, now time.Time,
) (domain.Notification, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE notifications
		   SET status = ?, attempts = 0, next_attempt_at = ?, last_error = '', sent_at = NULL
		 WHERE id = ? AND status <> ?`,
		string(domain.NotificationPending), formatTime(now), id, string(domain.NotificationSent))
	if err != nil {
		return domain.Notification{}, fmt.Errorf("requeueing a notification: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return domain.Notification{}, fmt.Errorf("reading the retry result: %w", err)
	}
	if affected == 0 {
		// Either it does not exist or it was already delivered. Both are
		// "there is nothing to retry", and telling them apart would mean
		// answering differently for a row the caller can simply read.
		return domain.Notification{}, fmt.Errorf(
			"%w: no notification %d is waiting to be sent", domain.ErrAlertNotFound, id)
	}

	rows, err := r.ListNotificationByID(ctx, id)
	if err != nil {
		return domain.Notification{}, err
	}
	return rows, nil
}

// ListNotificationByID reads one row of the delivery log.
func (r *AlertRepository) ListNotificationByID(ctx context.Context, id int64) (domain.Notification, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+notificationColumns+" FROM notifications WHERE id = ?", id)
	notification, _, err := scanNotification(row)
	if err != nil {
		return domain.Notification{}, err
	}
	return notification, nil
}

// PruneNotifications deletes delivered notifications older than cutoff.
//
// The outbox is a log, and a log with no end is a disk that fills. It is swept
// by the notifier rather than by retention because it only exists when
// alerting does, and a retention sweep that knew about a table belonging to a
// subsystem nobody switched on would be exactly the coupling ADR 005 refuses.
// Only delivered rows: a dead one is evidence, and it is what someone reads
// when they ask why they never got the message.
func (r *AlertRepository) PruneNotifications(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 500
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM notifications
		 WHERE id IN (
			SELECT id FROM notifications
			 WHERE status = ? AND created_at < ?
			 LIMIT ?)`,
		string(domain.NotificationSent), formatTime(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("pruning notifications: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reading the prune result: %w", err)
	}
	return deleted, nil
}

// IssueHourlyCounts reads one issue's events per hour from the aggregates.
func (r *AlertRepository) IssueHourlyCounts(
	ctx context.Context, projectID, issueID int64, at time.Time, hours int,
) ([]int64, error) {
	if hours <= 0 {
		hours = 1
	}
	end := at.UTC().Truncate(time.Hour)
	start := end.Add(-time.Duration(hours-1) * time.Hour)

	rows, err := r.db.QueryContext(ctx, `
		SELECT hour, count FROM issue_hourly
		 WHERE issue_id = ? AND project_id = ? AND hour >= ? AND hour <= ?`,
		issueID, projectID, domain.HourBucket(start), domain.HourBucket(end))
	if err != nil {
		return nil, fmt.Errorf("reading an issue's hourly counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := make([]int64, hours)
	for rows.Next() {
		var (
			hour  string
			count int64
		)
		if err := rows.Scan(&hour, &count); err != nil {
			return nil, fmt.Errorf("scanning an hourly count: %w", err)
		}
		parsed, err := domain.ParseHourBucket(hour)
		if err != nil {
			return nil, fmt.Errorf("%w: hour %q: %w", ErrSchema, hour, err)
		}
		index := int(parsed.UTC().Sub(start) / time.Hour)
		if index >= 0 && index < hours {
			counts[index] = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating hourly counts: %w", err)
	}
	return counts, nil
}

func boolToInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func nullableID(id *int64) any {
	if id == nil {
		return nil
	}
	return *id
}

// RulesOfKind returns the enabled rules of one kind covering a project.
func (r *AlertRepository) RulesOfKind(
	ctx context.Context, kind domain.TriggerKind, projectID int64,
) ([]domain.AlertRule, error) {
	rules, err := r.cachedRules(ctx, r.db)
	if err != nil {
		return nil, err
	}
	var matching []domain.AlertRule
	for _, rule := range rules {
		if rule.Trigger.Kind == kind && rule.AppliesTo(projectID) {
			matching = append(matching, rule)
		}
	}
	return matching, nil
}

// Watches reports whether any enabled rule reacts to a kind.
func (r *AlertRepository) Watches(ctx context.Context, kind domain.TriggerKind) (bool, error) {
	rules, err := r.cachedRules(ctx, r.db)
	if err != nil {
		return false, err
	}
	for _, rule := range rules {
		if rule.Trigger.Kind == kind {
			return true, nil
		}
	}
	return false, nil
}
