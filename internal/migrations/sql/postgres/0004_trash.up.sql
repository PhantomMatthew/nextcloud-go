CREATE TABLE trash_items (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    original_path   TEXT NOT NULL,
    location_id     TEXT NOT NULL,
    name            TEXT NOT NULL,
    is_dir          BOOLEAN NOT NULL,
    size            BIGINT NOT NULL DEFAULT 0,
    deleted_ms      BIGINT NOT NULL,
    deleted_by      TEXT NOT NULL DEFAULT '',
    UNIQUE (user_id, location_id)
);
CREATE INDEX idx_trash_user_deleted ON trash_items(user_id, deleted_ms);
