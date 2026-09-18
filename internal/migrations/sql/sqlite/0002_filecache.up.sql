CREATE TABLE files (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id       INTEGER REFERENCES files(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    path            TEXT NOT NULL,
    is_dir          INTEGER NOT NULL,
    size            INTEGER NOT NULL DEFAULT 0,
    mtime_ms        INTEGER NOT NULL,
    etag            TEXT NOT NULL,
    checksum        TEXT,
    mime            TEXT NOT NULL,
    permissions     INTEGER NOT NULL,
    UNIQUE (user_id, path)
);
CREATE INDEX idx_files_parent ON files(user_id, parent_id);
