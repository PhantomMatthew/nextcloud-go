CREATE TABLE addressbooks (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    uri             TEXT NOT NULL,
    displayname     TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    ctag            INTEGER NOT NULL DEFAULT 1,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE (user_id, uri)
);
CREATE TABLE addressbook_objects (
    id              BIGSERIAL PRIMARY KEY,
    addressbook_id  BIGINT NOT NULL REFERENCES addressbooks(id) ON DELETE CASCADE,
    uri             TEXT NOT NULL,
    uid             TEXT NOT NULL,
    fn              TEXT NOT NULL DEFAULT '',
    etag            TEXT NOT NULL,
    size            BIGINT NOT NULL,
    card_data       BYTEA NOT NULL,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE (addressbook_id, uri)
);
CREATE INDEX idx_addressbook_objects_uid ON addressbook_objects (addressbook_id, uid);
