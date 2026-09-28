-- What each run reported, and when.
--
-- One row per check-in rather than a "last run" column on the monitor, for
-- two reasons that pull the same way. The first is that a person debugging a
-- backup asks "when did it last actually work", which a single column cannot
-- answer once it has been overwritten. The second is that a run announces its
-- start and its finish separately, so "started and never finished" — the
-- timeout case — is the *absence* of a second write, and an absence needs a
-- row to be absent from.
CREATE TABLE cron_checkins (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id  INTEGER NOT NULL REFERENCES cron_monitors(id) ON DELETE CASCADE,
    -- The id the SDK chose, and the only way a finish is matched to the start
    -- it belongs to. Empty for a ping, which has nowhere to put one: a curl
    -- at the end of a crontab line gets its correlation from being the only
    -- run in flight, which is true of every cron job that is not overlapping
    -- itself — and one that is overlapping itself has a worse problem than
    -- this column.
    checkin_id  TEXT    NOT NULL DEFAULT '',
    -- in_progress | ok | error. The protocol's own names (ADR 002): an SDK
    -- sends these strings and this product does not get to rename them.
    status      TEXT    NOT NULL,
    started_at  TEXT    NOT NULL,
    finished_at TEXT,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    environment TEXT    NOT NULL DEFAULT ''
) STRICT;

-- The history view: one monitor's runs, newest first.
CREATE INDEX idx_cron_checkins_monitor ON cron_checkins (monitor_id, started_at DESC);

-- And the sweep's second question: is there a run still open on this monitor.
-- A partial index, so the steady state of this table — rows that finished
-- long ago — is not walked once every thirty seconds for the life of the
-- installation.
CREATE INDEX idx_cron_checkins_open ON cron_checkins (monitor_id, started_at)
    WHERE finished_at IS NULL;
