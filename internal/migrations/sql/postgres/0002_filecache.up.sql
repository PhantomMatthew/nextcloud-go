CREATE TABLE files (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id       BIGINT REFERENCES files(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    path            TEXT NOT NULL,
    is_dir          BOOLEAN NOT NULL,
    size            BIGINT NOT NULL DEFAULT 0,
    mtime_ms        BIGINT NOT NULL,
    etag            TEXT NOT NULL,
    checksum        TEXT,
    mime            TEXT NOT NULL,
    permissions     INT NOT NULL,
    UNIQUE (user_id, path)
);
CREATE INDEX idx_files_parent ON files(user_id, parent_id);
