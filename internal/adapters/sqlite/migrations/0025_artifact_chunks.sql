-- The staging area between a chunk upload and the assembly that names it.
--
-- The chunked protocol splits a bundle at a size the server announces, posts
-- the pieces across several requests, and only then asks for them to be put
-- back together (from the recording). So the pieces have to outlive a request, and
-- keeping them in memory would mean an assembly that lands on a different
-- process — or on the same one after a restart — silently finds nothing and
-- has to ask for the whole upload again.
--
-- They are keyed by the sha1 of their CONTENT, which is also what names them
-- on the wire, so a chunk that arrives twice costs one row: two projects
-- uploading builds that share a vendor bundle upload it once between them.

CREATE TABLE artifact_chunks (
    -- The sha1 of the INFLATED bytes. This is the trap the recording caught:
    -- the multipart field is called `file_gzip` and its `filename` is the
    -- checksum, and the checksum is of what the chunk decompresses to, not of
    -- what came down the wire. A server that verified the wire bytes would
    -- reject every gzip-announcing client while its own tests stayed green —
    -- the same shape as the bug in ADR 002 (from the recording).
    checksum   TEXT    NOT NULL PRIMARY KEY,
    bytes      BLOB    NOT NULL,
    size       INTEGER NOT NULL,
    created_at TEXT    NOT NULL
) STRICT;

-- The sweep. A chunk waits for an assembly for domain.ChunkRetention and no
-- longer: nothing will ever ask for the pieces of an upload that was
-- interrupted, and they are the same bytes as the bundle they would have made.
CREATE INDEX idx_artifact_chunks_created ON artifact_chunks (created_at);
