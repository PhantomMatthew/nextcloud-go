CREATE TABLE shares (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    share_type      INT NOT NULL,
    owner_user_id   BIGINT NOT NULL,
    file_path       VARCHAR(768) NOT NULL,
    item_type       VARCHAR(16) NOT NULL,
    token           VARCHAR(32) NOT NULL,
    password_hash   VARCHAR(255) NOT NULL DEFAULT '',
    permissions     INT NOT NULL,
    label           VARCHAR(255) NOT NULL DEFAULT '',
    expire_ms       BIGINT NOT NULL DEFAULT 0,
    stime_ms        BIGINT NOT NULL,
    UNIQUE KEY uq_shares_token (token),
    CONSTRAINT fk_shares_user FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
