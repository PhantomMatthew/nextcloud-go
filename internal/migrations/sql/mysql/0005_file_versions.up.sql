CREATE TABLE file_versions (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    file_path       VARCHAR(768) NOT NULL,
    revision        VARCHAR(64) NOT NULL,
    size            BIGINT NOT NULL DEFAULT 0,
    checksum        VARCHAR(128) NOT NULL DEFAULT '',
    created_ms      BIGINT NOT NULL,
    UNIQUE KEY uq_file_versions_user_path_rev (user_id, file_path, revision),
    INDEX idx_file_versions_user_path (user_id, file_path),
    CONSTRAINT fk_file_versions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
