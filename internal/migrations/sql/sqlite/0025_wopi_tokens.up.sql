CREATE TABLE wopi_tokens (
    token       TEXT PRIMARY KEY,
    uid         TEXT NOT NULL,
    file_id     INTEGER NOT NULL,
    can_write   INTEGER NOT NULL DEFAULT 0,
    expires_at  INTEGER NOT NULL
);
CREATE INDEX idx_wopi_tokens_expires ON wopi_tokens(expires_at);
