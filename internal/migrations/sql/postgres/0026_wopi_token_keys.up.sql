CREATE TABLE wopi_token_keys (
    token       TEXT PRIMARY KEY,
    sealed_uk   BYTEA NOT NULL,
    salt        BYTEA NOT NULL,
    created_ms  BIGINT NOT NULL
);
