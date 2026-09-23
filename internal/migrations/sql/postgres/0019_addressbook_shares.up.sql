CREATE TABLE addressbook_shares (
    id              BIGSERIAL PRIMARY KEY,
    addressbook_id  BIGINT NOT NULL REFERENCES addressbooks(id) ON DELETE CASCADE,
    target_user_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access          TEXT NOT NULL DEFAULT 'read',
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE (addressbook_id, target_user_id)
);
