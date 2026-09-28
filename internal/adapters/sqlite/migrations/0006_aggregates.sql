-- Hourly aggregates, written inside the ingest transaction (ADR 010).
--
-- The dashboard's every question — "how many errors an hour", "which issue is
-- loudest", "which release brought it" — is answered from these three tables
-- and never from a GROUP BY over events. Two reasons, and the second is the
-- one that decides it: a scan over events breaks the rule the whole design
-- rests on (ADR 001), and it stops working altogether the day retention
-- deletes the payloads. A dashboard that loses its history when the events
-- expire is not a dashboard. So the buckets carry their own, much longer,
-- keep-window (category "aggregates", 400 days) and outlive the rows they
-- were counted from.
--
-- The hour is TEXT 'YYYY-MM-DDTHH' in UTC, the same choice and for the same
-- reason as every other timestamp here: it sorts lexicographically in the
-- order it sorts chronologically, so a range is an index scan, and a human
-- reading a row can tell what it says.

-- Events per issue per hour. This is the sparkline on an issue, and the
-- "top issues in this range" list, which is the same table read two ways.
CREATE TABLE issue_hourly (
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour       TEXT    NOT NULL,
    count      INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (issue_id, hour)
) STRICT;

-- One issue's series is the primary key; a project's busiest issues in a range
-- needs the other order, and without this index that question is a full scan
-- of every issue's history.
CREATE INDEX idx_issue_hourly_project_hour ON issue_hourly (project_id, hour);

-- Events per project per hour, split by level, because "is this getting
-- worse" and "is any of it fatal" are the same chart with a legend.
CREATE TABLE project_hourly (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour       TEXT    NOT NULL,
    level      TEXT    NOT NULL,
    count      INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (project_id, hour, level)
) STRICT;

-- Events per project per hour, split by release or environment.
--
-- One table with a dim column rather than one table per dimension: the two
-- are read by exactly the same query with a different constant, and a third
-- dimension later is a value rather than a migration. The cardinality is
-- bounded by how many releases and environments a project actually has, which
-- is small — unlike tags, which are user-chosen and stay in issue_tags where
-- they are already aggregated per issue.
CREATE TABLE project_hourly_dims (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour       TEXT    NOT NULL,
    dim        TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    count      INTEGER NOT NULL DEFAULT 0,

    -- dim before hour: every read fixes the dimension and ranges over the
    -- hour, so this order makes the range a contiguous scan.
    PRIMARY KEY (project_id, dim, hour, value)
) STRICT;

-- The retention sweep deletes by project and hour without naming a dimension,
-- which the primary key cannot serve past its first column.
CREATE INDEX idx_project_hourly_dims_hour ON project_hourly_dims (project_id, hour);
