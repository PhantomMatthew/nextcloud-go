CREATE TABLE user_key_pw (
    user_id      BIGINT NOT NULL PRIMARY KEY,
    public_key   BLOB NOT NULL,
    pw_sealed_uk BLOB NOT NULL,
    pw_kdf       TEXT NOT NULL,
    pw_salt      BLOB NOT NULL,
    created_ms   BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
ALTER TABLE file_keys ADD COLUMN scheme INT NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN sealed_uk BLOB NULL;
