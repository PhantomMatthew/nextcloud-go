CREATE TABLE calendar_shares (
    id              BIGSERIAL PRIMARY KEY,
    calendar_id     BIGINT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    target_user_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access          TEXT NOT NULL DEFAULT 'read',
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE (calendar_id, target_user_id)
);
