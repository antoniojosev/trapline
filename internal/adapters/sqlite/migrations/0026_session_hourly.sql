-- Release health: how many sessions a release started, and how many of them
-- ended badly.
--
-- The one rule this table exists to keep is the one ADR 008 states as a
-- prohibition: **never a row per session**. A product that stores sessions
-- individually is a different product — session analytics — and it is where
-- the competitor spends its infrastructure. What release health actually needs
-- is four counters per (release, environment, hour), and that is all this is.
--
-- Counting distinct sessions still needs state, because an SDK does not send a
-- finished session: it sends updates of the same one (an `init`, then an
-- `exit` or a `crashed`). That state lives in a bounded in-memory window
-- (internal/domain/release_health.go) and only its verdicts reach this table.
-- A restart therefore loses the sessions in flight at that moment — an
-- accepted trade-off, not an oversight, and one the API says out loud so a
-- reader is not surprised by it (ADR 008, ADR 021).
--
-- The four counters are **disjoint**, exactly as the protocol's own `sessions`
-- aggregate item is: a session is counted once, under the worst thing that
-- happened to it. So
--
--     healthy = started - errored - crashed - abnormal
--
-- and no counter double-counts another. Storing overlapping counters instead
-- would make `crashed + errored > started` possible, which is the kind of
-- number nobody can interpret and everybody reports as a bug.
--
-- Counters and not rates, for the same reason the latency windows store a
-- sketch and not a percentile (ADR 007): a rate cannot be merged with another
-- window's. `crash_free_rate` is computed when somebody asks, from the sum of
-- the range — never stored.

CREATE TABLE session_hourly (
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- The release the SDK reported. Required: a session without one cannot be
    -- attributed to anything, and release health is the only question this
    -- table answers, so the ingest path refuses those rather than inventing a
    -- bucket for them.
    release     TEXT    NOT NULL,
    -- As sent. Empty is a legitimate value — plenty of SDKs report no
    -- environment — and it is kept as itself rather than renamed to
    -- "production", which would be this server inventing a fact about
    -- somebody's deployment.
    environment TEXT    NOT NULL,
    -- 'YYYY-MM-DDTHH' in UTC, the same layout the error aggregates and the
    -- transaction hours use (ADR 010, ADR 033), so a reader comparing a
    -- crash-free chart with an error chart is comparing two columns written
    -- the same way. The hour is the one the session **started** in, not the
    -- one it ended in: that is what makes "the 14:00 release was bad" a
    -- statement about the release rather than about when people closed their
    -- laptops.
    hour        TEXT    NOT NULL,

    -- Every session that reached a verdict in this bucket.
    started     INTEGER NOT NULL DEFAULT 0,
    -- Sessions that saw at least one error and still ended normally.
    errored     INTEGER NOT NULL DEFAULT 0,
    -- Sessions that ended in a crash. The numerator of the one number this
    -- whole table exists for.
    crashed     INTEGER NOT NULL DEFAULT 0,
    -- Sessions the SDK could not classify — killed process, closed tab mid
    -- flight. Kept apart from crashed on purpose: counting an app somebody
    -- force-quit as a crash is how a crash-free rate stops meaning anything.
    abnormal    INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (project_id, release, environment, hour)
) STRICT;

-- The dashboard's query: every release of one project over a range. The
-- primary key fixes the release before the hour, so "this project between
-- these two hours, across all releases" cannot use it.
CREATE INDEX idx_session_hourly_range ON session_hourly (project_id, hour);
