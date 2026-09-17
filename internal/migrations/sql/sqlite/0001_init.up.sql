CREATE TABLE users (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    uid             TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    email           TEXT,
    password_hash   TEXT NOT NULL,
    quota_bytes     INTEGER,
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE groups (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    gid             TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);

CREATE TABLE group_members (
    group_id        INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

CREATE TABLE sessions (
    id              TEXT PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_agent      TEXT,
    ip              TEXT,
    created_at      INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE app_passwords (
    id              TEXT PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      TEXT NOT NULL UNIQUE,
    login_name      TEXT NOT NULL,
    name            TEXT NOT NULL,
    type            INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER,
    expires_at      INTEGER
);
CREATE INDEX idx_app_passwords_user ON app_passwords(user_id);

CREATE TABLE login_flows (
    poll_token      TEXT PRIMARY KEY,
    login_token     TEXT NOT NULL UNIQUE,
    state_token     TEXT NOT NULL UNIQUE,
    client_name     TEXT NOT NULL,
    state           INTEGER NOT NULL,
    server          TEXT,
    login_name      TEXT,
    app_password    TEXT,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL
);

CREATE TABLE jobs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL,
    payload         BLOB,
    run_at          INTEGER NOT NULL,
    started_at      INTEGER,
    completed_at    INTEGER,
    last_error      TEXT,
    attempts        INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_jobs_run_at ON jobs(run_at);

CREATE TABLE module_config (
    module_id       TEXT NOT NULL,
    key             TEXT NOT NULL,
    value           BLOB,
    PRIMARY KEY (module_id, key)
);
