-- Full-text search over issues, and never over events (ADR 011).
--
-- The listing's `q` was a LIKE over lower(title) and lower(culprit): no index
-- can serve it, so it is a scan of every issue in the project, and at 10^5
-- issues that is the search box being the slowest thing in the product. FTS5
-- turns it into an index lookup.
--
-- Three columns, and only three: the title, the culprit — because searching
-- for a filename is what people actually do and the filename lives in the
-- culprit — and the message of the most recent event. The payloads stay out
-- of it, which is the rule the design rests on (ADR 001): indexing them would
-- be an index the size of the store, rebuilt on every event.

-- The last event's message, kept on the issue so the search index has
-- something to say about issues whose title is only an exception type.
-- Written in the ingest transaction alongside the counters, so it can never
-- describe an event that failed to store.
ALTER TABLE issues ADD COLUMN sample_message TEXT NOT NULL DEFAULT '';

-- content='issues' makes this an external-content index: the text is not
-- stored twice, only the terms are. A virtual table cannot be STRICT — that
-- is the module's shape, not a relaxation of the convention.
--
-- The tokenizer is `trigram`, not `unicode61`, and that choice is the whole
-- point of the index rather than a detail of it. unicode61 splits on word
-- boundaries, so it can only match a whole word or its prefix: `Value` finds
-- `ValueError` and `Error` does not. In a product whose subject matter is
-- exception names, that is not an edge case — it is the normal way people
-- search. `ConnectionTimeoutError`, `ECONNREFUSED` and `SQLSTATE[23000]` are
-- single tokens, and somebody looking for `Timeout`, `REFUSED` or `SQLSTATE`
-- is looking for the middle of one.
--
-- trigram indexes every three-character window instead, which makes a MATCH a
-- substring search served by the index — the semantics the `LIKE '%x%'` this
-- replaced had, without the scan. It costs about 5.5× the index size, which on
-- a table of issues is measured in megabytes against a store measured in
-- gigabytes (ADR 011).
--
-- remove_diacritics 1 is the whole reason a Spanish-speaking user can find
-- "función" by typing "funcion", and the reason someone who types the accent
-- still finds an issue recorded without it: both sides are folded, so neither
-- spelling is privileged.
CREATE VIRTUAL TABLE issues_fts USING fts5(
    title,
    culprit,
    sample_message,
    content='issues',
    content_rowid='id',
    tokenize='trigram remove_diacritics 1'
);

-- Existing issues, indexed once here rather than by a background pass: the
-- table is at most as large as the issue list, and a search that silently
-- ignores everything recorded before the upgrade is worse than no search.
INSERT INTO issues_fts (rowid, title, culprit, sample_message)
    SELECT id, title, culprit, sample_message FROM issues;

CREATE TRIGGER issues_fts_insert AFTER INSERT ON issues BEGIN
    INSERT INTO issues_fts (rowid, title, culprit, sample_message)
        VALUES (new.id, new.title, new.culprit, new.sample_message);
END;

CREATE TRIGGER issues_fts_delete AFTER DELETE ON issues BEGIN
    INSERT INTO issues_fts (issues_fts, rowid, title, culprit, sample_message)
        VALUES ('delete', old.id, old.title, old.culprit, old.sample_message);
END;

-- Narrowed twice, and both narrowings are on the ingest hot path.
--
-- Every stored event runs an UPDATE on its issue to move last_seen and bump
-- the counter. Without OF, that UPDATE would re-index the issue for full-text
-- search once per event — a delete and an insert into the index to record that
-- a number changed. With OF, only an update that mentions an indexed column
-- fires; with WHEN, only one that actually changes its value does. The common
-- case, the same error arriving again with the same message, costs nothing.
CREATE TRIGGER issues_fts_update
AFTER UPDATE OF title, culprit, sample_message ON issues
WHEN old.title IS NOT new.title
  OR old.culprit IS NOT new.culprit
  OR old.sample_message IS NOT new.sample_message
BEGIN
    INSERT INTO issues_fts (issues_fts, rowid, title, culprit, sample_message)
        VALUES ('delete', old.id, old.title, old.culprit, old.sample_message);
    INSERT INTO issues_fts (rowid, title, culprit, sample_message)
        VALUES (new.id, new.title, new.culprit, new.sample_message);
END;
