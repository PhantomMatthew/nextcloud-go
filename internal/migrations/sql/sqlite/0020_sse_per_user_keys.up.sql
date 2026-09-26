CREATE TABLE user_keys (
    user_id     INTEGER PRIMARY KEY,
    sealed_uk   BLOB NOT NULL,
    key_id      INTEGER NOT NULL,
    created_ms  INTEGER NOT NULL
);
CREATE TABLE file_keys (
    key_uuid    BLOB NOT NULL,
    user_id     INTEGER NOT NULL,
    wrapped_fk  BLOB NOT NULL,
    created_ms  INTEGER NOT NULL,
    PRIMARY KEY (key_uuid, user_id)
);
ALTER TABLE files ADD COLUMN key_uuid BLOB NULL;
CREATE INDEX files_key_uuid_idx ON files(key_uuid);
