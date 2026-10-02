-- Mail M3 sync (ADR-0108 §5). No DB-level foreign keys: account deletion
-- cascades at the app layer (SQLStore.Delete), matching 0027's
-- explicit-lifecycle convention.
-- mail_messages.flags stores space-separated flag tokens WITHOUT the IMAP
-- backslash, wrapped in one leading and one trailing space (' Seen Flagged ')
-- so unseen predicates stay token-exact: flags NOT LIKE '% Seen %'.
CREATE TABLE mail_mailboxes (
    id             BIGSERIAL PRIMARY KEY,
    account_id     BIGINT NOT NULL,
    name           TEXT NOT NULL,
    delimiter      TEXT NOT NULL DEFAULT '/',
    uidvalidity    BIGINT NOT NULL DEFAULT 0,
    uidnext        BIGINT NOT NULL DEFAULT 0,
    last_seen_uid  BIGINT NOT NULL DEFAULT 0,
    selectable     INTEGER NOT NULL DEFAULT 1,
    special_use    TEXT NOT NULL DEFAULT '',
    UNIQUE (account_id, name)
);
CREATE TABLE mail_messages (
    id          BIGSERIAL PRIMARY KEY,
    mailbox_id  BIGINT NOT NULL,
    uid         BIGINT NOT NULL,
    message_id  TEXT NOT NULL DEFAULT '',
    subject     TEXT NOT NULL DEFAULT '',
    from_addr   TEXT NOT NULL DEFAULT '',
    to_addrs    TEXT NOT NULL DEFAULT '',
    date_unix   BIGINT NOT NULL DEFAULT 0,
    flags       TEXT NOT NULL DEFAULT '',
    size        BIGINT NOT NULL DEFAULT 0,
    UNIQUE (mailbox_id, uid)
);
CREATE INDEX idx_mail_messages_mailbox_date ON mail_messages (mailbox_id, date_unix);
