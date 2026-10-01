CREATE TABLE wopi_token_keys (
    token       VARCHAR(255) PRIMARY KEY,
    sealed_uk   BLOB NOT NULL,
    salt        BLOB NOT NULL,
    created_ms  BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
