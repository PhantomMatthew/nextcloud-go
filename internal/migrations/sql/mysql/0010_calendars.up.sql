CREATE TABLE calendars (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    uri             VARCHAR(255) NOT NULL,
    displayname     VARCHAR(255) NOT NULL DEFAULT '',
    description     VARCHAR(1024) NOT NULL DEFAULT '',
    calendar_color  VARCHAR(32) NOT NULL DEFAULT '#0082c9',
    calendar_order  INT NOT NULL DEFAULT 0,
    timezone        VARCHAR(255) NOT NULL DEFAULT '',
    enabled         TINYINT NOT NULL DEFAULT 1,
    ctag            INT NOT NULL DEFAULT 1,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE KEY uq_calendars_user_uri (user_id, uri),
    CONSTRAINT fk_calendars_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE calendar_objects (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    calendar_id     BIGINT NOT NULL,
    uri             VARCHAR(255) NOT NULL,
    uid             VARCHAR(255) NOT NULL,
    etag            VARCHAR(64) NOT NULL,
    size            BIGINT NOT NULL,
    component       VARCHAR(16) NOT NULL,
    first_occur_ms  BIGINT NOT NULL DEFAULT 0,
    last_occur_ms   BIGINT NOT NULL DEFAULT 0,
    calendar_data   LONGBLOB NOT NULL,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE KEY uq_calendar_objects_cal_uri (calendar_id, uri),
    KEY idx_calendar_objects_uid (calendar_id, uid),
    KEY idx_calendar_objects_range (calendar_id, first_occur_ms, last_occur_ms),
    CONSTRAINT fk_calendar_objects_cal FOREIGN KEY (calendar_id) REFERENCES calendars(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
