-- Admin accounts for the panel. v1 is single-admin by design, but the table
-- is not artificially limited to one row: the constraint is a product
-- decision, not a schema one, and encoding it here would make the eventual
-- "add a second operator" a migration instead of a feature flag.
CREATE TABLE admins (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    created_at    TEXT    NOT NULL
) STRICT;

-- Sessions are server-side. The cookie carries a random id and nothing else,
-- so signing out actually invalidates the session and a stolen cookie stops
-- working the moment its row is deleted — neither of which is true of a
-- self-contained signed token.
--
-- The id column stores a hash of the session token, not the token itself: a
-- database dump must not be a set of usable cookies.
CREATE TABLE sessions (
    token_hash TEXT    NOT NULL PRIMARY KEY,
    admin_id   INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    created_at TEXT    NOT NULL,
    expires_at TEXT    NOT NULL
) STRICT;

-- Expired sessions are swept by expiry, and every lookup filters on it.
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);
