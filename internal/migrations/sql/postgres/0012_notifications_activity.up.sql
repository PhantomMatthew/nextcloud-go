CREATE TABLE notifications (
    id                         BIGSERIAL PRIMARY KEY,
    user_id                    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app                        TEXT NOT NULL,
    user_uid                   TEXT NOT NULL,
    object_type                TEXT NOT NULL DEFAULT '',
    object_id                  TEXT NOT NULL DEFAULT '',
    subject                    TEXT NOT NULL DEFAULT '',
    subject_rich               TEXT NOT NULL DEFAULT '',
    subject_rich_parameters    TEXT NOT NULL DEFAULT '{}',
    message                    TEXT NOT NULL DEFAULT '',
    message_rich               TEXT NOT NULL DEFAULT '',
    message_rich_parameters    TEXT NOT NULL DEFAULT '[]',
    link                       TEXT NOT NULL DEFAULT '',
    icon                       TEXT NOT NULL DEFAULT '',
    should_notify              INTEGER NOT NULL DEFAULT 1,
    created_at                 BIGINT NOT NULL
);
CREATE INDEX idx_notifications_user_id ON notifications (user_id, id);
CREATE TABLE activities (
    id                         BIGSERIAL PRIMARY KEY,
    user_id                    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_uid                  TEXT NOT NULL DEFAULT '',
    app                        TEXT NOT NULL,
    type                       TEXT NOT NULL DEFAULT '',
    subject                    TEXT NOT NULL DEFAULT '',
    subject_rich               TEXT NOT NULL DEFAULT '',
    subject_rich_parameters    TEXT NOT NULL DEFAULT '{}',
    message                    TEXT NOT NULL DEFAULT '',
    object_type                TEXT NOT NULL DEFAULT '',
    object_id                  BIGINT NOT NULL DEFAULT 0,
    object_name                TEXT NOT NULL DEFAULT '',
    link                       TEXT NOT NULL DEFAULT '',
    icon                       TEXT NOT NULL DEFAULT '',
    created_at                 BIGINT NOT NULL
);
CREATE INDEX idx_activities_user_id ON activities (user_id, id);
