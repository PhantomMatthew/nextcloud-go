CREATE TABLE addressbook_shares (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    addressbook_id  INTEGER NOT NULL REFERENCES addressbooks(id) ON DELETE CASCADE,
    target_user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access          TEXT NOT NULL DEFAULT 'read',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (addressbook_id, target_user_id)
);
