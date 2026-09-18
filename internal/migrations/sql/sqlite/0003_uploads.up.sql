CREATE TABLE uploads (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    transfer_id     TEXT NOT NULL,
    destination     TEXT NOT NULL DEFAULT '',
    total_length    INTEGER NOT NULL DEFAULT 0,
    created_ms      INTEGER NOT NULL,
    UNIQUE (user_id, transfer_id)
);
