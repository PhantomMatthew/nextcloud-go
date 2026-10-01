CREATE TABLE wopi_tokens (
    token       VARCHAR(255) PRIMARY KEY,
    uid         VARCHAR(255) NOT NULL,
    file_id     BIGINT NOT NULL,
    can_write   BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at  BIGINT NOT NULL,
    KEY idx_wopi_tokens_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
