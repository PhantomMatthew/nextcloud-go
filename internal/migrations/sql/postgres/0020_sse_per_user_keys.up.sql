CREATE TABLE user_keys (
    user_id     BIGINT PRIMARY KEY,
    sealed_uk   BYTEA NOT NULL,
    key_id      INT NOT NULL,
    created_ms  BIGINT NOT NULL
);
CREATE TABLE file_keys (
    key_uuid    BYTEA NOT NULL,
    user_id     BIGINT NOT NULL,
    wrapped_fk  BYTEA NOT NULL,
    created_ms  BIGINT NOT NULL,
    PRIMARY KEY (key_uuid, user_id)
);
ALTER TABLE files ADD COLUMN key_uuid BYTEA NULL;
CREATE INDEX files_key_uuid_idx ON files(key_uuid);
