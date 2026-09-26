CREATE TABLE app_token_keys (
    app_password_id VARCHAR(255) NOT NULL PRIMARY KEY,
    sealed_uk       BLOB NOT NULL,
    salt            BLOB NOT NULL,
    created_ms      BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
