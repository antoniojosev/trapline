-- The outbox.
--
-- A notification is written here by the same transaction that produced the
-- event it is about, and sent afterwards by a background job. That ordering is
-- the whole design: nothing about a pending delivery lives in memory, so a
-- process that dies between "an issue appeared" and "Slack was told" comes
-- back and tells Slack. An error tracker that goes quiet exactly when the
-- server restarted — which is when it is most needed — is not one (ADR 015).
CREATE TABLE notifications (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    -- No foreign key to alert_rules, deliberately. The log has to outlive the
    -- rule: "why did I get this message three weeks ago" is a question about a
    -- rule that may since have been deleted, and a cascade would erase the
    -- evidence along with the configuration.
    rule_id     INTEGER NOT NULL,
    channel_id  INTEGER NOT NULL,
    -- The value of X-Trapline-Delivery, minted once and reused by every retry
    -- of this row. Delivery is at-least-once — a crash between a successful
    -- POST and this row being marked sent produces a second one — so a
    -- receiver needs a stable key to deduplicate on, and this is it.
    delivery_id TEXT    NOT NULL,
    subject_key TEXT    NOT NULL,
    -- The rendered message, frozen at enqueue time. Re-deriving it at delivery
    -- would report the present as the past: by the time a retry succeeds four
    -- hours later, the issue has a different count and possibly a different
    -- status, and the message would describe neither the incident nor now.
    payload     TEXT    NOT NULL,
    -- pending | sent | failed | dead.
    status      TEXT    NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    -- When the notifier may next pick this up. It doubles as the lease: a pass
    -- that claims a row pushes this into the future, so two passes cannot send
    -- the same notification and no extra status is needed to say "in flight".
    next_attempt_at TEXT NOT NULL,
    last_error  TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL,
    sent_at     TEXT
) STRICT;

-- The notifier's only question: what is due. Status first because the steady
-- state of this table is rows that are already sent, and they must not be
-- walked once a second for the life of the installation.
CREATE INDEX idx_notifications_due ON notifications (status, next_attempt_at);

-- And the log, newest first, which is what `alerts log` and the panel read.
CREATE INDEX idx_notifications_created ON notifications (created_at DESC);
