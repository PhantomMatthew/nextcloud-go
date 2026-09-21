CREATE TABLE calendar_shares (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    calendar_id     INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    target_user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access          TEXT NOT NULL DEFAULT 'read',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (calendar_id, target_user_id)
);
