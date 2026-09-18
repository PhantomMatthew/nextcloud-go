CREATE TABLE files (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    parent_id       BIGINT NULL,
    name            VARCHAR(255) NOT NULL,
    path            VARCHAR(768) NOT NULL,
    is_dir          BOOLEAN NOT NULL,
    size            BIGINT NOT NULL DEFAULT 0,
    mtime_ms        BIGINT NOT NULL,
    etag            VARCHAR(255) NOT NULL,
    checksum        VARCHAR(255) NULL,
    mime            VARCHAR(255) NOT NULL,
    permissions     INT NOT NULL,
    UNIQUE KEY uq_files_user_path (user_id, path),
    INDEX idx_files_parent (user_id, parent_id),
    CONSTRAINT fk_files_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT fk_files_parent FOREIGN KEY (parent_id) REFERENCES files(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
