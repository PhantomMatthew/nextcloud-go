CREATE TABLE wopi_token_keys (
    token       TEXT PRIMARY KEY,
    sealed_uk   BLOB NOT NULL,
    salt        BLOB NOT NULL,
    created_ms  INTEGER NOT NULL
);
