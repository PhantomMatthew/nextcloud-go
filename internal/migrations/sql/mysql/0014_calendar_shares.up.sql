CREATE TABLE calendar_shares (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    calendar_id     BIGINT NOT NULL,
    target_user_id  BIGINT NOT NULL,
    access          VARCHAR(16) NOT NULL DEFAULT 'read',
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE KEY uq_calendar_shares_cal_target (calendar_id, target_user_id),
    CONSTRAINT fk_calendar_shares_cal FOREIGN KEY (calendar_id) REFERENCES calendars(id) ON DELETE CASCADE,
    CONSTRAINT fk_calendar_shares_target FOREIGN KEY (target_user_id) REFERENCES users(id) ON DELETE CASCADE
);
