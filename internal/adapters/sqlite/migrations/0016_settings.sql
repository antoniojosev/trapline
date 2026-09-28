-- Installation settings: one key, one JSON value.
--
-- Everything in this schema so far belongs to something — a project, an
-- issue, a release. This table is for the handful of facts that belong to the
-- *installation*: the day and hour the weekly digest goes out, and later the
-- status page's title and whatever the MCP surface needs to remember.
-- None of them is worth a table, a migration and a repository each, and
-- a column on a table that has one row would be worse: there is no such table
-- here, and inventing one would mean every future setting is a migration.
--
-- Key-value with a JSON value rather than a TEXT value, for one reason: a
-- setting is almost never a scalar. The digest schedule is a weekday and an
-- hour, and storing it as two rows would let half of it be written. As one
-- JSON document it is written or it is not.
--
-- The value is TEXT and not a BLOB. SQLite's JSON functions read text, so a
-- future query can filter on a field without the application decoding the
-- row, and a human running `sqlite3` on the file can read what it says —
-- which is the same reason every timestamp here is text.
CREATE TABLE settings (
    key        TEXT NOT NULL PRIMARY KEY,
    -- A JSON document. Validated by CHECK rather than by convention: a
    -- setting that is not valid JSON is unreadable by everything that will
    -- ever look at it, and the failure would surface far from the write that
    -- caused it.
    value      TEXT NOT NULL CHECK (json_valid(value)),
    -- When it was last written. Not decoration: "did anyone ever configure
    -- this, or is it the default" is the first question when a digest goes
    -- out at the wrong hour, and a row with no timestamp cannot answer it.
    updated_at TEXT NOT NULL
) STRICT;
