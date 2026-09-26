CREATE TABLE app_token_keys (
    app_password_id TEXT PRIMARY KEY,
    sealed_uk       BLOB NOT NULL,
    salt            BLOB NOT NULL,
    created_ms      INTEGER NOT NULL
);
