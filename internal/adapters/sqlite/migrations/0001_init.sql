-- Projects are tenants. The id is an INTEGER because it appears in the
-- ingest path that official SDKs build from a DSN (ADR 002), so it must stay
-- the shape those SDKs are exercised against.
CREATE TABLE projects (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    created_at TEXT    NOT NULL
) STRICT;

-- DSN keys. Stored in the clear on purpose: a DSN public key ships inside
-- browser bundles, so it is a public identifier whose only authority is
-- "append events to this project" (see SECURITY.md). The panel has to be
-- able to show it back to the user.
--
-- revoked_at NULL means active. Rotation issues a second key, waits for
-- deployments to pick it up, then revokes the first — which is why more than
-- one active key per project is allowed, but bounded in the domain.
CREATE TABLE project_keys (
    public_key TEXT    NOT NULL PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_at TEXT    NOT NULL,
    revoked_at TEXT
) STRICT;

-- Authentication on the hot path is "does this public key exist, is it
-- active, and does it belong to the project id in the URL". This partial
-- index keeps that lookup off the revoked rows entirely.
CREATE INDEX idx_project_keys_active
    ON project_keys (project_id)
    WHERE revoked_at IS NULL;
