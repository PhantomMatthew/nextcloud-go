CREATE TABLE user_key_pw (
    user_id      BIGINT PRIMARY KEY,
    public_key   BYTEA NOT NULL,
    pw_sealed_uk BYTEA NOT NULL,
    pw_kdf       TEXT NOT NULL,
    pw_salt      BYTEA NOT NULL,
    created_ms   BIGINT NOT NULL
);
ALTER TABLE file_keys ADD COLUMN scheme INT NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN sealed_uk BYTEA NULL;
