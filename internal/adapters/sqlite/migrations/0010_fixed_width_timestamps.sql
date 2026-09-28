-- Rewrite every stored instant to the fixed-width layout (ADR 033).
--
-- Rows written before this migration used Go's RFC3339Nano, which drops
-- trailing zeros from the fraction. An instant on an exact second was written
-- `2026-08-29T10:00:00Z` and one 500 ms later `2026-08-29T10:00:00.5Z`, and
-- because '.' (0x2E) sorts before 'Z' (0x5A) SQLite reads the later one as
-- earlier. That comparison is the issue list's ORDER BY, the keyset cursor and
-- the retention WHERE, so the consequence was an event *after* the retention
-- cutoff being swept as if it were before it: silent data loss in a product
-- whose one job is not to lose errors.
--
-- The fix is a shape, not a rule: with the fraction always present and always
-- nine digits, every value is the same width and a byte comparison of two of
-- them is a comparison of the instants. This migration gives the rows already
-- on disk that shape.
--
-- The conversion, applied to every column below:
--
--   * the GLOB is a shape guard. A value that does not look like
--     `YYYY-MM-DDThh:mm:ss…Z` is left exactly as it was rather than mangled
--     into something that parses; NULL fails the GLOB and stays NULL.
--   * `substr(c, 1, 19)` is the part every value already shares.
--   * `ltrim(replace(substr(c, 20), 'Z', ''), '.')` is the fraction's digits
--     with no leading dot and no trailing Z — the empty string when there was
--     no fraction at all.
--   * padding with nine zeros and taking nine characters right-pads a short
--     fraction and leaves a full one alone, which is what makes this migration
--     idempotent: a value already in the new shape converts to itself.
--
-- Every UPDATE below is a full-table rewrite, `events` included. There is no
-- cheaper version: the rows that need changing are precisely the ones with no
-- fraction or a short one, and telling those apart costs the same scan as
-- rewriting them. This runs once, inside the migration transaction, on a
-- database that is not yet serving.
--
-- The `issues` UPDATE touches no column the full-text triggers watch
-- (migration 0007 narrows them to title, culprit and sample_message), so it
-- does not re-index anything.
--
-- Not touched, and checked rather than assumed: the `hour` columns of
-- issue_hourly, project_hourly and project_hourly_dims. Those are
-- domain.HourLayout, 'YYYY-MM-DDTHH', which has no fraction and is already
-- fixed width, so text order there has always been chronological order.

UPDATE projects SET
    created_at = CASE WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(created_at, 1, 19) || '.' || substr(ltrim(replace(substr(created_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE created_at END;

UPDATE project_keys SET
    created_at = CASE WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(created_at, 1, 19) || '.' || substr(ltrim(replace(substr(created_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    revoked_at = CASE WHEN revoked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(revoked_at, 1, 19) || '.' || substr(ltrim(replace(substr(revoked_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE revoked_at END;

UPDATE admins SET
    created_at = CASE WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(created_at, 1, 19) || '.' || substr(ltrim(replace(substr(created_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE created_at END;

-- expires_at is compared on every authenticated request and swept by the
-- session cleanup, so this is one of the columns where the old order was a
-- live correctness problem and not only a cosmetic one.
UPDATE sessions SET
    created_at = CASE WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(created_at, 1, 19) || '.' || substr(ltrim(replace(substr(created_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    expires_at = CASE WHEN expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(expires_at, 1, 19) || '.' || substr(ltrim(replace(substr(expires_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE expires_at END;

UPDATE api_tokens SET
    created_at = CASE WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(created_at, 1, 19) || '.' || substr(ltrim(replace(substr(created_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    expires_at = CASE WHEN expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(expires_at, 1, 19) || '.' || substr(ltrim(replace(substr(expires_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE expires_at END,
    last_used = CASE WHEN last_used GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(last_used, 1, 19) || '.' || substr(ltrim(replace(substr(last_used, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE last_used END;

-- last_seen is the sort key of the product's main screen and half of the
-- keyset cursor.
UPDATE issues SET
    first_seen = CASE WHEN first_seen GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(first_seen, 1, 19) || '.' || substr(ltrim(replace(substr(first_seen, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE first_seen END,
    last_seen = CASE WHEN last_seen GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(last_seen, 1, 19) || '.' || substr(ltrim(replace(substr(last_seen, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE last_seen END,
    resolved_at = CASE WHEN resolved_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(resolved_at, 1, 19) || '.' || substr(ltrim(replace(substr(resolved_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE resolved_at END;

-- received_at is what the retention sweep compares. This is the column whose
-- wrong order deleted events that should have survived.
UPDATE events SET
    received_at = CASE WHEN received_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(received_at, 1, 19) || '.' || substr(ltrim(replace(substr(received_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE received_at END,
    occurred_at = CASE WHEN occurred_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(occurred_at, 1, 19) || '.' || substr(ltrim(replace(substr(occurred_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE occurred_at END;

-- first_event_at and last_event_at are widened with MIN() and MAX() over the
-- stored text on every ingested event that names a release, so a mixed-width
-- column made a release's window drift to the wrong instants.
UPDATE releases SET
    created_at = CASE WHEN created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(created_at, 1, 19) || '.' || substr(ltrim(replace(substr(created_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE created_at END,
    date_released = CASE WHEN date_released GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(date_released, 1, 19) || '.' || substr(ltrim(replace(substr(date_released, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE date_released END,
    first_event_at = CASE WHEN first_event_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(first_event_at, 1, 19) || '.' || substr(ltrim(replace(substr(first_event_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE first_event_at END,
    last_event_at = CASE WHEN last_event_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(last_event_at, 1, 19) || '.' || substr(ltrim(replace(substr(last_event_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE last_event_at END;

UPDATE release_commits SET
    timestamp = CASE WHEN timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(timestamp, 1, 19) || '.' || substr(ltrim(replace(substr(timestamp, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE timestamp END;

UPDATE deploys SET
    started_at = CASE WHEN started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(started_at, 1, 19) || '.' || substr(ltrim(replace(substr(started_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE started_at END,
    finished_at = CASE WHEN finished_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(finished_at, 1, 19) || '.' || substr(ltrim(replace(substr(finished_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE finished_at END;

-- schema_migrations is created in Go rather than by a migration file, but its
-- applied_at is a stored instant like any other and a `doctor` output that
-- mixed two shapes in one table would be the next person's confusing hour.
-- Rows 1-9 are rewritten here; this migration's own row is inserted after this
-- statement by the migrator, already in the new layout.
UPDATE schema_migrations SET
    applied_at = CASE WHEN applied_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
        THEN substr(applied_at, 1, 19) || '.' || substr(ltrim(replace(substr(applied_at, 20), 'Z', ''), '.') || '000000000', 1, 9) || 'Z'
        ELSE applied_at END;
