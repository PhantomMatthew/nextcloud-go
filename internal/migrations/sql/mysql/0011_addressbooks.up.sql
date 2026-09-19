CREATE TABLE addressbooks (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    uri             VARCHAR(255) NOT NULL,
    displayname     VARCHAR(255) NOT NULL DEFAULT '',
    description     VARCHAR(1024) NOT NULL DEFAULT '',
    enabled         TINYINT NOT NULL DEFAULT 1,
    ctag            INT NOT NULL DEFAULT 1,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE KEY uq_addressbooks_user_uri (user_id, uri),
    CONSTRAINT fk_addressbooks_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE addressbook_objects (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    addressbook_id  BIGINT NOT NULL,
    uri             VARCHAR(255) NOT NULL,
    uid             VARCHAR(255) NOT NULL,
    fn              VARCHAR(255) NOT NULL DEFAULT '',
    etag            VARCHAR(64) NOT NULL,
    size            BIGINT NOT NULL,
    card_data       LONGBLOB NOT NULL,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE KEY uq_addressbook_objects_book_uri (addressbook_id, uri),
    KEY idx_addressbook_objects_uid (addressbook_id, uid),
    CONSTRAINT fk_addressbook_objects_book FOREIGN KEY (addressbook_id) REFERENCES addressbooks(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
