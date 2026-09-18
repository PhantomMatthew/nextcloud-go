CREATE TABLE uploads (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    transfer_id     TEXT NOT NULL,
    destination     TEXT NOT NULL DEFAULT '',
    total_length    BIGINT NOT NULL DEFAULT 0,
    created_ms      BIGINT NOT NULL,
    UNIQUE (user_id, transfer_id)
);
