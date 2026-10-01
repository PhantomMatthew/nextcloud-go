CREATE TABLE mail_accounts (
    id              BIGSERIAL PRIMARY KEY,
    user_id         TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    email           TEXT NOT NULL,
    imap_host       TEXT NOT NULL,
    imap_port       INTEGER NOT NULL,
    imap_ssl_mode   TEXT NOT NULL DEFAULT 'ssl',
    imap_user       TEXT NOT NULL,
    smtp_host       TEXT NOT NULL,
    smtp_port       INTEGER NOT NULL,
    smtp_ssl_mode   TEXT NOT NULL DEFAULT 'ssl',
    smtp_user       TEXT NOT NULL,
    password_sealed BYTEA NOT NULL,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);
CREATE INDEX idx_mail_accounts_user_id ON mail_accounts (user_id, id);
