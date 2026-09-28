-- Where notifications go, what they are about, and what has already been said.
--
-- Three tables and no queue. The queue is 0015; these are the configuration
-- and the memory, and the memory is the reason they are on disk at all: a
-- silence window kept in RAM is lost on restart, and a restart is precisely
-- when the burst arrives. The first thing an operator does about a flood of
-- errors is restart the thing, and an alerting subsystem whose deduplication
-- resets at that exact moment sends its loudest storm at the worst possible
-- minute (ADR 015).

-- A destination. One row per place a message can go.
CREATE TABLE alert_channels (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    -- telegram | slack | discord | webhook | email. Not a CHECK constraint:
    -- the closed set lives in internal/domain, monitors add nothing to it but a
    -- later build might, and a schema that has to be migrated to learn a new
    -- channel type is a schema that makes adding one a database operation.
    type       TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    -- The credentials, AES-256-GCM. A BLOB rather than TEXT because that is
    -- what it is: a nonce and a ciphertext, not a string. The key lives in
    -- <db>.key and never inside this file, which is the whole point — and the
    -- reason `backup` says out loud that it is not copying it.
    config_enc BLOB    NOT NULL,
    -- Whether the weekly digest also goes here. Stored now so that the
    -- digest adds a job and not a migration.
    digest     INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL
) STRICT;

-- A condition, its destinations, and how long it then stays quiet.
CREATE TABLE alert_rules (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    -- NULL means every project. An installation with one application should
    -- be able to write its first rule without naming anything, and a rule
    -- that covers whatever gets added tomorrow is usually what was meant.
    project_id      INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    -- The kind is a column of its own, beside the JSON that already contains
    -- it, so the ingest path can ask "is there any enabled rule of this kind"
    -- with an index instead of decoding every rule per event. The JSON stays
    -- the single source of the parameters; this is a lookup key.
    trigger_kind    TEXT    NOT NULL,
    -- `trigger` is a reserved word in SQLite, and a column that needs quoting
    -- everywhere is a column that will one day not be quoted somewhere.
    trigger_config  TEXT    NOT NULL,
    -- A JSON array of channel ids. Not a join table: a rule's channels are
    -- read as a set, always all at once, never queried across, and a join
    -- table would buy referential integrity for a list of at most twenty
    -- integers at the cost of a second write per rule and a join per event.
    -- Deleting a channel rewrites the arrays that named it.
    channel_ids     TEXT    NOT NULL,
    silence_seconds INTEGER NOT NULL,
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL
) STRICT;

-- The ingest path's question, once per matching event: which enabled rules
-- react to this kind. Without the index that is a scan of every rule in the
-- installation on the hot path (ADR 001's rule, applied to a small table for
-- the same reason it applies to a large one).
CREATE INDEX idx_alert_rules_enabled_kind ON alert_rules (enabled, trigger_kind);

-- What a rule has already said, and about what.
--
-- Keyed by subject rather than by rule: a deploy that breaks two endpoints
-- should produce two notifications and then be quiet about both. A rule-wide
-- silence would let the first issue mask the second, which is exactly the
-- behaviour that makes people switch alerting off and never switch it back on.
CREATE TABLE alert_state (
    rule_id       INTEGER NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    -- issue:<id> | project:<id> | monitor:<id>.
    subject_key   TEXT    NOT NULL,
    last_fired_at TEXT    NOT NULL,

    PRIMARY KEY (rule_id, subject_key)
) STRICT;
