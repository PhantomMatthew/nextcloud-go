CREATE TABLE plugins (
    id                TEXT PRIMARY KEY,
    version           TEXT NOT NULL,
    enabled           INTEGER NOT NULL DEFAULT 1,
    capabilities_json TEXT NOT NULL DEFAULT '',
    signature_keyid   TEXT NOT NULL DEFAULT '',
    archive_path      TEXT NOT NULL,
    installed_at      INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
