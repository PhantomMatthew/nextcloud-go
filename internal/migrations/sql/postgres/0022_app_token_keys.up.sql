CREATE TABLE app_token_keys (
    app_password_id TEXT PRIMARY KEY,
    sealed_uk       BYTEA NOT NULL,
    salt            BYTEA NOT NULL,
    created_ms      BIGINT NOT NULL
);
