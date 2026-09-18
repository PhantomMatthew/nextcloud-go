CREATE TABLE file_locks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    file_path       TEXT NOT NULL,
    token           TEXT NOT NULL,
    owner           TEXT NOT NULL DEFAULT '',
    timeout_ms      INTEGER NOT NULL,
    created_ms      INTEGER NOT NULL,
    UNIQUE (user_id, file_path),
    UNIQUE (token)
);
