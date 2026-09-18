CREATE TABLE trash_items (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    original_path   TEXT NOT NULL,
    location_id     TEXT NOT NULL,
    name            TEXT NOT NULL,
    is_dir          INTEGER NOT NULL,
    size            INTEGER NOT NULL DEFAULT 0,
    deleted_ms      INTEGER NOT NULL,
    deleted_by      TEXT NOT NULL DEFAULT '',
    UNIQUE (user_id, location_id)
);
CREATE INDEX idx_trash_user_deleted ON trash_items(user_id, deleted_ms);
