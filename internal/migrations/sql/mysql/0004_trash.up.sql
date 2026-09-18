CREATE TABLE trash_items (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    original_path   VARCHAR(768) NOT NULL,
    location_id     VARCHAR(768) NOT NULL,
    name            VARCHAR(255) NOT NULL,
    is_dir          BOOLEAN NOT NULL,
    size            BIGINT NOT NULL DEFAULT 0,
    deleted_ms      BIGINT NOT NULL,
    deleted_by      VARCHAR(255) NOT NULL DEFAULT '',
    UNIQUE KEY uq_trash_user_loc (user_id, location_id),
    INDEX idx_trash_user_deleted (user_id, deleted_ms),
    CONSTRAINT fk_trash_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
