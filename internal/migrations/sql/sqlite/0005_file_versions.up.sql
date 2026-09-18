CREATE TABLE file_versions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    file_path       TEXT NOT NULL,
    revision        TEXT NOT NULL,
    size            INTEGER NOT NULL DEFAULT 0,
    checksum        TEXT NOT NULL DEFAULT '',
    created_ms      INTEGER NOT NULL,
    UNIQUE (user_id, file_path, revision)
);
CREATE INDEX idx_file_versions_user_path ON file_versions(user_id, file_path);
