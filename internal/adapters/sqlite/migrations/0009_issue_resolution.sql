-- Resolving an issue "in the next release", and what came back.
--
-- Without these columns the product has one rule: any event on a resolved
-- issue reopens it. That rule is wrong in the single most common situation
-- there is — you fix a bug, you deploy, and the instances still running the
-- old build keep emitting the error for as long as the rollout takes. The
-- issue reopens within seconds of being resolved, and a user who watches that
-- happen twice stops believing the status field (ADR 012).
--
-- Added as columns on `issues` rather than a table of their own because they
-- are read on exactly the same row as the status they qualify, and a join per
-- issue detail would buy nothing.
ALTER TABLE issues ADD COLUMN resolved_at TEXT;
ALTER TABLE issues ADD COLUMN resolved_in_release TEXT NOT NULL DEFAULT '';
-- 0 or 1: SQLite in STRICT mode has no boolean type, and INTEGER is what the
-- rest of the schema already uses for a flag.
ALTER TABLE issues ADD COLUMN resolve_next_release INTEGER NOT NULL DEFAULT 0;
-- The release the very first event came from. Unlike last_release this never
-- moves: it is the answer to "what shipped this", which is a fact about the
-- past and not a pointer that follows the newest occurrence.
ALTER TABLE issues ADD COLUMN first_release TEXT NOT NULL DEFAULT '';
-- How many times this issue has come back after being resolved. An issue with
-- three of them is a different kind of problem from one with none, and this
-- is the only thing that says so.
ALTER TABLE issues ADD COLUMN regressions INTEGER NOT NULL DEFAULT 0;
-- The release of the most recent regression. Recorded rather than inferred
-- from last_release, which keeps moving with every later event: an issue that
-- came back in 1.0.1 and kept erroring through 1.0.3 would otherwise hand the
-- blame to 1.0.3, which shipped nothing to do with it.
ALTER TABLE issues ADD COLUMN regressed_in_release TEXT NOT NULL DEFAULT '';
-- How many events arrived from the resolved release after it was resolved,
-- and were therefore counted but not treated as a regression. Stored rather
-- than derived: deriving it means counting rows in `events` filtered by
-- release and timestamp, which is the scan the storage design exists to avoid
-- (ADR 001), and it would stop being answerable the moment retention deletes
-- those events.
ALTER TABLE issues ADD COLUMN seen_in_resolved_release_count INTEGER NOT NULL DEFAULT 0;

-- "Which issues did this release introduce" is the first question a release
-- detail answers, and it is a count over this column.
CREATE INDEX idx_issues_project_first_release ON issues (project_id, first_release);

-- And "which issues did it bring back" is a count over this one.
CREATE INDEX idx_issues_project_regressed_release ON issues (project_id, regressed_in_release);
