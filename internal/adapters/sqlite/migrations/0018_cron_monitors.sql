-- The jobs this installation expects to hear from.
--
-- A cron monitor is the inverse of everything else in this product: the rest
-- of it reacts to something arriving, and this reacts to something *not*
-- arriving. That single difference is why the deadline is a stored column
-- rather than something derived when somebody looks. Nobody looks at a backup
-- that stopped running — that is the entire failure mode — so the question
-- "which monitors are overdue" has to be answerable by an indexed range scan
-- from a background sweep, not by evaluating a crontab expression per monitor
-- per pass (ADR 016).
CREATE TABLE cron_monitors (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- The identity an SDK sends on every check-in. Unique per project because
    -- that is the key a check-in arrives with, and because an SDK declaring a
    -- monitor it has already declared must find the one it made last time.
    slug          TEXT    NOT NULL,
    -- The secret in /ping/{ping_key}. Unique across the installation, not per
    -- project: the URL carries no project, on purpose — a line at the end of a
    -- crontab entry should be one token and nothing else. Thirty-two hex
    -- characters from crypto/rand, so it is a credential and not an id.
    ping_key      TEXT    NOT NULL,
    -- crontab | interval. Not a CHECK constraint, for the reason the alert
    -- channel type is not one either: the closed set lives in internal/domain,
    -- and a schema that has to be migrated to learn a third way of writing a
    -- schedule is a schema that makes adding one a database operation.
    schedule_type TEXT    NOT NULL,
    -- The expression itself: five crontab fields, or "<n> <unit>".
    schedule      TEXT    NOT NULL,
    -- An IANA zone name, never a fixed offset. "Three in the morning" is a
    -- different instant in Caracas and in Madrid, and an offset is a fact
    -- about one moment: a monitor stored as -04:00 would be an hour wrong for
    -- half the year anywhere that still changes its clocks, and the symptom
    -- would be a job that started reporting missed for no reason.
    timezone      TEXT    NOT NULL DEFAULT 'UTC',
    -- How late a run may be before it counts as missed, and how long a run
    -- that announced its start may go without reporting that it finished.
    checkin_margin_s INTEGER NOT NULL,
    max_runtime_s    INTEGER NOT NULL,
    -- ok | missed | timeout | error | unknown. A monitor starts unknown and
    -- never ok: a green light for a backup that has never run is the single
    -- most expensive lie a status page can tell.
    status        TEXT    NOT NULL DEFAULT 'unknown',
    last_checkin_at  TEXT,
    next_expected_at TEXT,
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT    NOT NULL,

    UNIQUE (project_id, slug),
    UNIQUE (ping_key)
) STRICT;

-- The sweep's only question, asked every thirty seconds: which enabled
-- monitors are past their deadline. Enabled first, because a switched-off
-- monitor must not be walked at all — the same reasoning as the alert rule
-- index, applied to the table the background job reads rather than the one
-- the hot path does.
CREATE INDEX idx_cron_monitors_due ON cron_monitors (enabled, next_expected_at);
