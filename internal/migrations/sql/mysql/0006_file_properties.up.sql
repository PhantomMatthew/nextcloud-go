CREATE TABLE file_properties (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    file_path       VARCHAR(768) NOT NULL,
    ns              VARCHAR(128) NOT NULL,
    name            VARCHAR(64) NOT NULL,
    value           TEXT NOT NULL,
    UNIQUE KEY uq_file_properties_user_path_ns_name (user_id, file_path, ns, name),
    INDEX idx_file_properties_user_path (user_id, file_path),
    CONSTRAINT fk_file_properties_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
