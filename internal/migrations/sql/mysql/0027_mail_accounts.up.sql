CREATE TABLE mail_accounts (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id         VARCHAR(255) NOT NULL,
    name            VARCHAR(255) NOT NULL DEFAULT '',
    email           VARCHAR(255) NOT NULL,
    imap_host       VARCHAR(255) NOT NULL,
    imap_port       INT NOT NULL,
    imap_ssl_mode   VARCHAR(16) NOT NULL DEFAULT 'ssl',
    imap_user       VARCHAR(255) NOT NULL,
    smtp_host       VARCHAR(255) NOT NULL,
    smtp_port       INT NOT NULL,
    smtp_ssl_mode   VARCHAR(16) NOT NULL DEFAULT 'ssl',
    smtp_user       VARCHAR(255) NOT NULL,
    password_sealed BLOB NOT NULL,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    KEY idx_mail_accounts_user_id (user_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
