-- Ninety days of history, in ninety rows.
--
-- The status page draws a bar per day for ninety days (ADR 017). Reading that
-- from `uptime_results` would be a quarter of a million rows scanned per
-- monitor per page load, on a table that exists to be written once a minute —
-- which is exactly the shape ADR 001 forbids, and the same argument the hourly
-- issue aggregates make.
--
-- So the job that writes a result also increments the day it belongs to, in
-- the same transaction. The aggregate is what survives the ninety-day pruning
-- of the results: after that, this table is the only record that a service was
-- down last March, which is the question anyone renewing a contract asks.
--
-- `latency_sum` and not `latency_mean`: a stored average cannot be merged with
-- another period's, and every range the page draws — 24 h, 7 d, 90 d — is a
-- merge. A sum and a count can be added; an average cannot (ADR 007's rule,
-- applied to a much smaller number).
CREATE TABLE uptime_daily (
    monitor_id  INTEGER NOT NULL REFERENCES uptime_monitors(id) ON DELETE CASCADE,
    -- YYYY-MM-DD in UTC. Text, like every other instant in this database, and
    -- fixed width so it sorts lexicographically in the order it sorts
    -- chronologically (see time.go). UTC and not the reader's zone: a day
    -- that moves with whoever is looking is not a day.
    day         TEXT    NOT NULL,
    checks      INTEGER NOT NULL DEFAULT 0,
    failures    INTEGER NOT NULL DEFAULT 0,
    latency_sum INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (monitor_id, day)
) STRICT;

-- The sweep that drops aggregates older than the status page's window. Their
-- retention is far longer than the results', because they are what answers
-- "was this worse last quarter" once the checks themselves are gone.
CREATE INDEX idx_uptime_daily_day ON uptime_daily (day);
