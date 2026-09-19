CREATE TABLE calendars (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    uri             TEXT NOT NULL,
    displayname     TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    calendar_color  TEXT NOT NULL DEFAULT '#0082c9',
    calendar_order  INTEGER NOT NULL DEFAULT 0,
    timezone        TEXT NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    ctag            INTEGER NOT NULL DEFAULT 1,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE (user_id, uri)
);
CREATE TABLE calendar_objects (
    id              BIGSERIAL PRIMARY KEY,
    calendar_id     BIGINT NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    uri             TEXT NOT NULL,
    uid             TEXT NOT NULL,
    etag            TEXT NOT NULL,
    size            BIGINT NOT NULL,
    component       TEXT NOT NULL,
    first_occur_ms  BIGINT NOT NULL DEFAULT 0,
    last_occur_ms   BIGINT NOT NULL DEFAULT 0,
    calendar_data   BYTEA NOT NULL,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE (calendar_id, uri)
);
CREATE INDEX idx_calendar_objects_uid ON calendar_objects (calendar_id, uid);
CREATE INDEX idx_calendar_objects_range ON calendar_objects (calendar_id, first_occur_ms, last_occur_ms);
