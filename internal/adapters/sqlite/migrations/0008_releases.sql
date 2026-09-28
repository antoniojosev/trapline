-- Releases: the version of the software an event came from, as an entity.
--
-- It is a table rather than the string already on every event because three
-- questions need somewhere to live and a column can answer none of them: when
-- was this deployed, what went into it, and what broke because of it.
--
-- The autoincrement id is load-bearing, not just a key. Most real release
-- identifiers are not versions — a git sha, a CI build number, a date stamp —
-- and nothing about the string says which came first. The id is assigned when
-- the release is first seen, so it *is* the first-sight order the domain
-- needs to compare two of them (ADR 012).
CREATE TABLE releases (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id     INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    version        TEXT    NOT NULL,
    created_at     TEXT    NOT NULL,
    -- NULL until someone finalises it. That is what tells "we are still
    -- deploying this" apart from "this is live", and a default of now would
    -- erase the difference on every row.
    date_released  TEXT,
    -- NULL while the release has produced no events, which is the good case
    -- and worth being able to see at a glance.
    first_event_at TEXT,
    last_event_at  TEXT,
    commit_count   INTEGER NOT NULL DEFAULT 0,

    -- One release per version per project. This is what makes the implicit
    -- creation on ingest a single upsert rather than a read-then-write race
    -- that would duplicate a release under concurrent load.
    UNIQUE (project_id, version)
) STRICT;

-- The release list is "what have we shipped", newest first.
CREATE INDEX idx_releases_project ON releases (project_id, id DESC);

-- The commits that went into a release.
CREATE TABLE release_commits (
    release_id   INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    sha          TEXT    NOT NULL,
    message      TEXT    NOT NULL DEFAULT '',
    author_name  TEXT    NOT NULL DEFAULT '',
    author_email TEXT    NOT NULL DEFAULT '',
    -- NULL when the sender did not say, which several tools do not.
    timestamp    TEXT,
    repository   TEXT    NOT NULL DEFAULT '',
    -- Named ordinal because "order" is a reserved word, and a column that has
    -- to be quoted forever is a column somebody eventually forgets to quote.
    ordinal      INTEGER NOT NULL,

    PRIMARY KEY (release_id, sha)
) STRICT;

-- The files each commit touched.
--
-- This table is the entire input to answering "which commit probably caused
-- this issue": the intersection between the files in a stacktrace and the
-- files a release changed. Recording the paths when the commits arrive is
-- what makes that answer a lookup later instead of a clone of the repository
-- at the moment somebody is trying to read an error.
CREATE TABLE release_commit_files (
    release_id  INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    sha         TEXT    NOT NULL,
    path        TEXT    NOT NULL,
    -- A, M or D, as the protocol's patch set spells them.
    change_type TEXT    NOT NULL DEFAULT 'M',

    PRIMARY KEY (release_id, sha, path)
) STRICT;

-- Lookup by path, which is the direction the suspect-commit query runs in:
-- from a frame's file to the commits that touched it.
CREATE INDEX idx_release_commit_files_path ON release_commit_files (path);

-- One release going out to one environment. A release is deployed to staging
-- before production, and often more than once, so this is a list and not two
-- columns on the release.
CREATE TABLE deploys (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    release_id  INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    environment TEXT    NOT NULL,
    name        TEXT    NOT NULL DEFAULT '',
    url         TEXT    NOT NULL DEFAULT '',
    started_at  TEXT,
    finished_at TEXT
) STRICT;

CREATE INDEX idx_deploys_release ON deploys (release_id, id DESC);
