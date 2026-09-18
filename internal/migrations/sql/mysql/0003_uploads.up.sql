CREATE TABLE uploads (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    transfer_id     VARCHAR(64) NOT NULL,
    destination     VARCHAR(768) NOT NULL DEFAULT '',
    total_length    BIGINT NOT NULL DEFAULT 0,
    created_ms      BIGINT NOT NULL,
    UNIQUE KEY uq_uploads_user_tid (user_id, transfer_id),
    CONSTRAINT fk_uploads_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
