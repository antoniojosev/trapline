-- API tokens are the machine credential: the CLI, an agent through MCP, or
-- any integration. Separate from admin sessions because they have different
-- lifetimes, different revocation needs and different scopes.
--
-- token_hash, never the token: a database dump must not be a set of usable
-- credentials. A plain SHA-256 is the right hash here, unlike for a password
-- — a token is 32 bytes of cryptographic randomness, so there is no
-- dictionary to attack and a slow hash would tax every authenticated request.
CREATE TABLE api_tokens (
    token_hash TEXT    NOT NULL PRIMARY KEY,
    name       TEXT    NOT NULL,
    scopes     TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    expires_at TEXT,
    last_used  TEXT
) STRICT;

CREATE INDEX idx_api_tokens_created_at ON api_tokens (created_at);
