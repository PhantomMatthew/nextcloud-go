CREATE TABLE wopi_tokens (
    token       TEXT PRIMARY KEY,
    uid         TEXT NOT NULL,
    file_id     BIGINT NOT NULL,
    can_write   BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at  BIGINT NOT NULL
);
CREATE INDEX idx_wopi_tokens_expires ON wopi_tokens(expires_at);
