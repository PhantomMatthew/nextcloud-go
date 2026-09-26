CREATE TABLE user_keys (
    user_id     BIGINT NOT NULL PRIMARY KEY,
    sealed_uk   BLOB NOT NULL,
    key_id      INT NOT NULL,
    created_ms  BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE file_keys (
    key_uuid    VARBINARY(16) NOT NULL,
    user_id     BIGINT NOT NULL,
    wrapped_fk  BLOB NOT NULL,
    created_ms  BIGINT NOT NULL,
    PRIMARY KEY (key_uuid, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
ALTER TABLE files ADD COLUMN key_uuid VARBINARY(16) NULL;
CREATE INDEX files_key_uuid_idx ON files(key_uuid);
