-- The waterfalls: one row per sampled trace, with its spans as one blob.
--
-- Separate from 0022 because it is a different kind of thing kept for a
-- different reason. `txn_minute` and `txn_hour` are the numbers, written for
-- 100% of what arrives; this is the examples, written for the share
-- `traces_sample_rate` allows. Losing a row here costs one waterfall nobody
-- was going to open; losing a row there moves a p95 (ADR 021).
--
-- The sampling decision is `hash(trace_id) < rate`, taken in the domain and
-- deterministic in the trace id — never per transaction. Two transactions of
-- one request reach the same verdict in this process and in the next one, so a
-- waterfall is stored whole or not at all. A coin flip per transaction would
-- produce waterfalls with holes in them that look exactly like instrumentation
-- somebody forgot to add, which is the worst possible way to be wrong about a
-- distributed trace.
--
-- The spans are one compressed blob and not rows. Nothing queries inside them:
-- a waterfall is read whole, by trace id, exactly once, and only after
-- somebody clicked a row that came from the aggregates. Rows would buy a
-- filtering ability nobody has asked for and cost an index over the highest
-- cardinality data in the product — which is precisely the shape ADR 001
-- forbids.

CREATE TABLE traces (
    -- The trace id as the SDK sent it, and the identity of the whole request.
    -- Primary key rather than an autoincrement: the second transaction of a
    -- trace arriving later must find the first one rather than create a
    -- duplicate.
    trace_id    TEXT    NOT NULL PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- The root transaction's name, timing and outcome: what a listing of
    -- recent traces shows without decompressing anything.
    txn_name    TEXT    NOT NULL,
    timestamp   TEXT    NOT NULL,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    status      TEXT    NOT NULL DEFAULT '',
    op          TEXT    NOT NULL DEFAULT '',
    -- The waterfall itself: the transaction's own span plus its children, as
    -- normalised JSON, zstd-compressed like every other stored payload. It is
    -- normalised rather than kept verbatim so the API's shape does not depend
    -- on which SDK produced it, and so the scrubber has walked the span data
    -- before it reached the disk (SECURITY.md).
    spans       BLOB    NOT NULL
) STRICT;

-- The retention sweep's cutoff, and the "recent traces for this project"
-- listing, which are the same range scan read two ways.
CREATE INDEX idx_traces_project_time ON traces (project_id, timestamp);

-- One transaction's own examples: "show me a slow one" starts from the name
-- somebody clicked in the listing, not from the trace id they do not have.
CREATE INDEX idx_traces_transaction ON traces (project_id, txn_name, duration_ms);
