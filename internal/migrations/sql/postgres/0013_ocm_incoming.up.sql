CREATE TABLE ocm_incoming (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_uid     TEXT NOT NULL,
    name         TEXT NOT NULL,
    remote       TEXT NOT NULL,
    remote_id    TEXT NOT NULL,
    owner        TEXT NOT NULL DEFAULT '',
    token        TEXT NOT NULL DEFAULT '',
    item_type    TEXT NOT NULL DEFAULT 'file',
    permissions  INTEGER NOT NULL DEFAULT 1,
    accepted     INTEGER NOT NULL DEFAULT 1,
    created_at   BIGINT NOT NULL,
    UNIQUE (user_id, remote, remote_id)
);
CREATE INDEX idx_ocm_incoming_user_id ON ocm_incoming (user_id, id);
