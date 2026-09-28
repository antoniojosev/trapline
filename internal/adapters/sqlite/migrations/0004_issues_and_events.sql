-- Issues are groups of events that are the same problem.
--
-- grouping_version travels with the row because changing how events group is
-- destructive for anyone already using the product: resolved issues reappear
-- and counters reset. Storing it makes a future change a migration with a plan
-- (ADR 003).
CREATE TABLE issues (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id       INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    fingerprint      TEXT    NOT NULL,
    grouping_version INTEGER NOT NULL,
    title            TEXT    NOT NULL,
    culprit          TEXT    NOT NULL DEFAULT '',
    level            TEXT    NOT NULL,
    status           TEXT    NOT NULL,
    first_seen       TEXT    NOT NULL,
    last_seen        TEXT    NOT NULL,
    times            INTEGER NOT NULL,
    last_release     TEXT    NOT NULL DEFAULT '',

    -- One issue per fingerprint per project. This is what makes ingestion's
    -- "find or create" a single upsert instead of a read-then-write race that
    -- would duplicate an issue under concurrent load.
    UNIQUE (project_id, fingerprint)
) STRICT;

-- The issue list is the product's main screen: unresolved issues for one
-- project, most recent first. This index is that query.
CREATE INDEX idx_issues_project_status_last_seen
    ON issues (project_id, status, last_seen DESC);

-- Events are the individual occurrences.
--
-- The payload is stored whole and compressed so the detail view and the issue
-- bundle have everything, while every column a listing or a dashboard touches
-- is extracted alongside it. No query ever scans a payload (ADR 001).
CREATE TABLE events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    event_id    TEXT    NOT NULL DEFAULT '',
    received_at TEXT    NOT NULL,
    occurred_at TEXT    NOT NULL,
    level       TEXT    NOT NULL,
    release     TEXT    NOT NULL DEFAULT '',
    environment TEXT    NOT NULL DEFAULT '',
    message     TEXT    NOT NULL DEFAULT '',
    payload     BLOB    NOT NULL,
    -- How the payload was compressed, so a future change of codec can be read
    -- back rather than needing every old row rewritten.
    payload_codec TEXT  NOT NULL DEFAULT 'zstd'
) STRICT;

-- "The latest events for this issue" is the detail view; "everything older
-- than X in this project" is the retention sweep.
CREATE INDEX idx_events_issue_occurred  ON events (issue_id, occurred_at DESC);
CREATE INDEX idx_events_project_received ON events (project_id, received_at);

-- Tags aggregated per issue, so filtering by tag never scans events.
--
-- The count is kept here rather than derived because deriving it means a
-- GROUP BY over the event table, which is exactly the scan this table exists
-- to avoid.
CREATE TABLE issue_tags (
    issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    key      TEXT    NOT NULL,
    value    TEXT    NOT NULL,
    count    INTEGER NOT NULL DEFAULT 1,

    PRIMARY KEY (issue_id, key, value)
) STRICT;

CREATE INDEX idx_issue_tags_lookup ON issue_tags (key, value);
