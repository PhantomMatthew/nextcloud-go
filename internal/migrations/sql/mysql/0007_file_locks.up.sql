CREATE TABLE file_locks (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    file_path       VARCHAR(768) NOT NULL,
    token           VARCHAR(128) NOT NULL,
    owner           VARCHAR(255) NOT NULL DEFAULT '',
    timeout_ms      BIGINT NOT NULL,
    created_ms      BIGINT NOT NULL,
    UNIQUE KEY uq_file_locks_user_path (user_id, file_path),
    UNIQUE KEY uq_file_locks_token (token),
    CONSTRAINT fk_file_locks_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
