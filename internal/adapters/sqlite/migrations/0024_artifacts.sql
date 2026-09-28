-- The uploaded scripts and source maps, addressable the two ways an event can
-- name one (ADR 018).
--
-- The names come from a recording, not from the ADR. `kind` is
-- `minified_source` / `source_map` because that is the vocabulary of the
-- manifest inside a real artifact bundle; ADR 018 wrote `source` / `sourcemap`,
-- and a CHECK written from the ADR would have rejected the first genuine
-- upload (see the annex of ADR 018). A migration is written
-- once, so it is written against what is going to arrive.

CREATE TABLE artifacts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- NULL for the modern path, which is not an omission: `sourcemaps upload`
    -- with no --release names no release anywhere, and the debug id is the
    -- whole join (from the recording). The cascade is the retention rule of ADR 018
    -- stated as schema: an artefact uploaded with a release dies with it.
    release_id      INTEGER REFERENCES releases(id) ON DELETE CASCADE,
    -- Which build of that release. Empty when the upload named no dist.
    dist            TEXT    NOT NULL DEFAULT '',
    -- The debug id a script and its map SHARE. Empty when the build was never
    -- injected, which is why it is a column and not a key: half the uploads
    -- this server will ever see are addressed the other way.
    debug_id        TEXT    NOT NULL DEFAULT '',
    -- The identity of the archive this file arrived in, which is a different
    -- value from debug_id and belongs to no file (from the recording). Nothing looks an
    -- artefact up by it; it is here so an operator can tell which upload a
    -- file came from, and because ADR 018's schema had nowhere to put it.
    bundle_debug_id TEXT    NOT NULL DEFAULT '',
    -- The `~/path` url, normalised by domain.ArtifactURL on the way in so the
    -- uploader's `~/bundle.min.js` and an event's
    -- `https://app.example.test/static/bundle.min.js?v=8f3a` reduce to one
    -- string. They have to, or the second lookup silently finds nothing.
    name            TEXT    NOT NULL,
    kind            TEXT    NOT NULL CHECK (kind IN ('minified_source', 'source_map')),
    -- The `sourcemap` header of a script: the FILE NAME of its map, not its
    -- url. It is what joins the two when there is no debug id, and ADR 018
    -- had no column for it (from the recording).
    sourcemap_ref   TEXT    NOT NULL DEFAULT '',
    -- Of the stored content, not of the wire. The protocol's sha1 names chunks
    -- in flight; this identifies bytes at rest, and conflating them is how a
    -- server ends up verifying the hash of what arrived compressed instead of
    -- the hash of what it decompresses to (from the recording, ADR 002).
    sha256          TEXT    NOT NULL,
    -- Uncompressed, because that is the number the project's budget is
    -- counted in and the number an operator recognises. `blob` is zstd, like
    -- every other stored payload.
    size            INTEGER NOT NULL,
    -- Named next to the blob so a future change of codec can be read back
    -- instead of needing every old row rewritten, exactly as `events` does
    -- with `payload_codec`: the reader compares it against the codec it was
    -- built with and refuses a row it cannot honestly decompress, rather than
    -- handing zstd bytes to whatever comes next.
    codec           TEXT    NOT NULL DEFAULT 'zstd',
    blob            BLOB    NOT NULL,
    created_at      TEXT    NOT NULL
) STRICT;

-- One row per (build, kind). A script and its map share a debug id and differ
-- in kind, so both fit; re-uploading the same build replaces rather than
-- duplicating, which is what makes a pipeline that reruns a step cheap.
--
-- Partial, because the empty string is not an identity: every artefact
-- uploaded the old way carries no debug id, and a unique index over all of
-- them would let the first legacy upload block the second.
CREATE UNIQUE INDEX idx_artifacts_debug_id_kind
    ON artifacts (project_id, debug_id, kind) WHERE debug_id <> '';

-- And the other identity, for the rows the one above deliberately excludes:
-- an artefact with no debug id is addressed by release, dist and url, so that
-- triple is what a re-upload has to replace.
--
-- IFNULL because SQLite treats NULLs in a unique index as distinct from each
-- other: without it, uploading `~/bundle.min.js` with no release twice would
-- store it twice and go on storing it once per deploy forever.
CREATE UNIQUE INDEX idx_artifacts_release_url
    ON artifacts (project_id, IFNULL(release_id, 0), dist, name) WHERE debug_id = '';

-- The first lookup of ADR 018: an event's debug_meta image to its map.
CREATE INDEX idx_artifacts_debug_id ON artifacts (project_id, debug_id);

-- The second: a release and a normalised abs_path.
CREATE INDEX idx_artifacts_release_name ON artifacts (release_id, name);

-- Listing a project's artefacts, and the sweep that removes the ones with no
-- release to die with (domain.OrphanArtifactRetention).
CREATE INDEX idx_artifacts_project_created ON artifacts (project_id, created_at);
