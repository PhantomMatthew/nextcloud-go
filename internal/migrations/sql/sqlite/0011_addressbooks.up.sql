CREATE TABLE addressbooks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    uri             TEXT NOT NULL,
    displayname     TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    ctag            INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (user_id, uri)
);
CREATE TABLE addressbook_objects (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    addressbook_id  INTEGER NOT NULL REFERENCES addressbooks(id) ON DELETE CASCADE,
    uri             TEXT NOT NULL,
    uid             TEXT NOT NULL,
    fn              TEXT NOT NULL DEFAULT '',
    etag            TEXT NOT NULL,
    size            INTEGER NOT NULL,
    card_data       BLOB NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (addressbook_id, uri)
);
CREATE INDEX idx_addressbook_objects_uid ON addressbook_objects (addressbook_id, uid);
