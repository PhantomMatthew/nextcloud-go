-- Mail M3 sync (ADR-0108 §5). No DB-level foreign keys: account deletion
-- cascades at the app layer (SQLStore.Delete), matching 0027's
-- explicit-lifecycle convention.
-- mail_messages.flags stores space-separated flag tokens WITHOUT the IMAP
-- backslash, wrapped in one leading and one trailing space (' Seen Flagged ')
-- so unseen predicates stay token-exact: flags NOT LIKE '% Seen %'.
CREATE TABLE mail_mailboxes (
    id             BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    account_id     BIGINT NOT NULL,
    name           VARCHAR(255) NOT NULL,
    delimiter      VARCHAR(16) NOT NULL DEFAULT '/',
    uidvalidity    BIGINT NOT NULL DEFAULT 0,
    uidnext        BIGINT NOT NULL DEFAULT 0,
    last_seen_uid  BIGINT NOT NULL DEFAULT 0,
    selectable     TINYINT NOT NULL DEFAULT 1,
    special_use    VARCHAR(32) NOT NULL DEFAULT '',
    UNIQUE KEY uniq_mail_mailboxes_account_name (account_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE mail_messages (
    id          BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    mailbox_id  BIGINT NOT NULL,
    uid         BIGINT NOT NULL,
    message_id  VARCHAR(255) NOT NULL DEFAULT '',
    subject     TEXT NOT NULL,
    from_addr   TEXT NOT NULL,
    to_addrs    TEXT NOT NULL,
    date_unix   BIGINT NOT NULL DEFAULT 0,
    flags       VARCHAR(255) NOT NULL DEFAULT '',
    size        BIGINT NOT NULL DEFAULT 0,
    UNIQUE KEY uniq_mail_messages_mailbox_uid (mailbox_id, uid),
    KEY idx_mail_messages_mailbox_date (mailbox_id, date_unix)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
