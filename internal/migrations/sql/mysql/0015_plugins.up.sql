CREATE TABLE plugins (
    id                VARCHAR(191) PRIMARY KEY,
    version           VARCHAR(64) NOT NULL,
    enabled           TINYINT NOT NULL DEFAULT 1,
    capabilities_json TEXT NOT NULL,
    signature_keyid   VARCHAR(32) NOT NULL DEFAULT '',
    archive_path      TEXT NOT NULL,
    installed_at      BIGINT NOT NULL,
    updated_at        BIGINT NOT NULL
);
