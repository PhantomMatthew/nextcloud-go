CREATE TABLE file_locks (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    file_path       TEXT NOT NULL,
    token           TEXT NOT NULL,
    owner           TEXT NOT NULL DEFAULT '',
    timeout_ms      BIGINT NOT NULL,
    created_ms      BIGINT NOT NULL,
    UNIQUE (user_id, file_path),
    UNIQUE (token)
);
