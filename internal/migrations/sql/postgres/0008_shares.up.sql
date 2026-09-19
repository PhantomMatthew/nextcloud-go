CREATE TABLE shares (
    id              BIGSERIAL PRIMARY KEY,
    share_type      INTEGER NOT NULL,
    owner_user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    file_path       TEXT NOT NULL,
    item_type       TEXT NOT NULL,
    token           TEXT NOT NULL,
    password_hash   TEXT NOT NULL DEFAULT '',
    permissions     INTEGER NOT NULL,
    label           TEXT NOT NULL DEFAULT '',
    expire_ms       BIGINT NOT NULL DEFAULT 0,
    stime_ms        BIGINT NOT NULL,
    UNIQUE (token)
);
