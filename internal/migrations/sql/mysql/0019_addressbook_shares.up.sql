CREATE TABLE addressbook_shares (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    addressbook_id  BIGINT NOT NULL,
    target_user_id  BIGINT NOT NULL,
    access          VARCHAR(16) NOT NULL DEFAULT 'read',
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE KEY uq_addressbook_shares_book_target (addressbook_id, target_user_id),
    CONSTRAINT fk_addressbook_shares_book FOREIGN KEY (addressbook_id) REFERENCES addressbooks(id) ON DELETE CASCADE,
    CONSTRAINT fk_addressbook_shares_target FOREIGN KEY (target_user_id) REFERENCES users(id) ON DELETE CASCADE
);
