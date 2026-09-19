CREATE TABLE ocm_incoming (
    id           BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id      BIGINT NOT NULL,
    user_uid     VARCHAR(255) NOT NULL,
    name         VARCHAR(255) NOT NULL,
    remote       VARCHAR(255) NOT NULL,
    remote_id    VARCHAR(255) NOT NULL,
    owner        VARCHAR(512) NOT NULL DEFAULT '',
    token        VARCHAR(255) NOT NULL DEFAULT '',
    item_type    VARCHAR(32) NOT NULL DEFAULT 'file',
    permissions  INT NOT NULL DEFAULT 1,
    accepted     TINYINT NOT NULL DEFAULT 1,
    created_at   BIGINT NOT NULL,
    UNIQUE KEY uniq_ocm_incoming_remote (user_id, remote, remote_id),
    KEY idx_ocm_incoming_user_id (user_id, id),
    CONSTRAINT fk_ocm_incoming_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
