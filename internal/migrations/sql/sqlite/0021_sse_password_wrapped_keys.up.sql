CREATE TABLE user_key_pw (
    user_id      INTEGER PRIMARY KEY,
    public_key   BLOB NOT NULL,
    pw_sealed_uk BLOB NOT NULL,
    pw_kdf       TEXT NOT NULL,
    pw_salt      BLOB NOT NULL,
    created_ms   INTEGER NOT NULL
);
ALTER TABLE file_keys ADD COLUMN scheme INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN sealed_uk BLOB NULL;
