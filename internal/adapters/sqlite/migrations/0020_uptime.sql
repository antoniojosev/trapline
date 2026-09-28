-- What is being watched, and what every look at it found.
--
-- Two tables, and the split is the same one ADR 001 makes everywhere else in
-- this product: the thing that answers a question fast, and the raw rows it
-- was derived from. `uptime_monitors` carries its own current state — status,
-- consecutive failures, when the next check is due — so "which monitors are
-- due" and "is this up" are both index lookups and never a scan of the
-- history. `uptime_results` is the history, and it is pruned at ninety days;
-- the daily roll-up that outlives it is 0021.

CREATE TABLE uptime_monitors (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id              INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name                    TEXT    NOT NULL,
    -- The URL this server will fetch, unattended, for as long as the monitor
    -- exists. Stored as given — normalising it would make a monitor check
    -- something other than what its author read back — and validated before
    -- it ever gets here: syntax by the domain, and the addresses behind it by
    -- internal/ssrfguard, which is the reason this column is not an SSRF
    -- (ADR 016).
    url                     TEXT    NOT NULL,
    -- GET or HEAD. Not a CHECK constraint: the closed set lives in
    -- internal/domain, like the channel types of 0014, and a schema that has
    -- to be migrated to learn a method is a schema that makes adding one a
    -- database operation.
    method                  TEXT    NOT NULL,
    interval_s              INTEGER NOT NULL,
    timeout_s               INTEGER NOT NULL,
    -- The status range that counts as healthy, inclusive at both ends.
    expected_status_min     INTEGER NOT NULL,
    expected_status_max     INTEGER NOT NULL,
    -- What the body must contain, empty when the body is not read. This is
    -- what tells "the load balancer answers" from "the application works".
    expected_body_substring TEXT    NOT NULL DEFAULT '',
    follow_redirects        INTEGER NOT NULL DEFAULT 1,
    -- This monitor's half of the permission to reach an address that is not
    -- globally routable. The other half is the installation's
    -- -uptime-allow-private, and neither is sufficient alone: a flag in a
    -- database that granted network access on its own would be one row away
    -- from an SSRF, and the person who can write this row is not always the
    -- person who runs the server (ADR 016).
    allow_private           INTEGER NOT NULL DEFAULT 0,
    -- Whether it appears on the public status page (ADR 017).
    public                  INTEGER NOT NULL DEFAULT 0,
    enabled                 INTEGER NOT NULL DEFAULT 1,

    -- The current state, carried on the monitor rather than derived.
    --
    -- Derived would mean reading the history on every check to count the
    -- failures since the last success — a query per check, over a table that
    -- is pruned, for a number the previous write already knew. Worse: the
    -- pruning would eventually erase the evidence and a monitor that had been
    -- down for four months would quietly become "up".
    status                  TEXT    NOT NULL DEFAULT 'unknown',
    consecutive_failures     INTEGER NOT NULL DEFAULT 0,
    last_checked_at         TEXT,
    -- When the next check is due. Stored so the job's question — "what is
    -- overdue" — is a range scan on an index rather than a computation over
    -- every monitor in the installation, and so a lease can be taken by
    -- moving it forward (ADR 014).
    next_check_at           TEXT    NOT NULL,
    last_status_change_at   TEXT,
    created_at              TEXT    NOT NULL
) STRICT;

-- The job's one query, once per tick: the enabled monitors that are overdue.
-- Without this index that is a scan of every monitor every thirty seconds.
CREATE INDEX idx_uptime_monitors_due ON uptime_monitors (enabled, next_check_at);

-- The listing, and the status page's "which of this project's monitors are
-- public".
CREATE INDEX idx_uptime_monitors_project ON uptime_monitors (project_id, id);

-- One row per check.
CREATE TABLE uptime_results (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id  INTEGER NOT NULL REFERENCES uptime_monitors(id) ON DELETE CASCADE,
    checked_at  TEXT    NOT NULL,
    ok          INTEGER NOT NULL,
    -- Zero when nothing answered at all, which is a different failure from a
    -- 500 and reads as one.
    status_code INTEGER NOT NULL DEFAULT 0,
    latency_ms  INTEGER NOT NULL DEFAULT 0,
    -- Why it failed. The single field that makes a red bar on a status page
    -- into something somebody can act on.
    error       TEXT    NOT NULL DEFAULT ''
) STRICT;

-- Newest-first history for one monitor, and the retention sweep's cutoff.
CREATE INDEX idx_uptime_results_monitor_time ON uptime_results (monitor_id, checked_at);

-- The sweep's own query, which is across monitors rather than within one.
CREATE INDEX idx_uptime_results_time ON uptime_results (checked_at);
