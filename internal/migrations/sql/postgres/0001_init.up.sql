CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    uid             TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    email           TEXT,
    password_hash   TEXT NOT NULL,
    quota_bytes     BIGINT,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);

CREATE TABLE groups (
    id              BIGSERIAL PRIMARY KEY,
    gid             TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    created_at      BIGINT NOT NULL
);

CREATE TABLE group_members (
    group_id        BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

CREATE TABLE sessions (
    id              TEXT PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_agent      TEXT,
    ip              TEXT,
    created_at      BIGINT NOT NULL,
    last_seen_at    BIGINT NOT NULL,
    expires_at      BIGINT NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE app_passwords (
    id              TEXT PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      TEXT NOT NULL UNIQUE,
    login_name      TEXT NOT NULL,
    name            TEXT NOT NULL,
    type            INT NOT NULL,
    created_at      BIGINT NOT NULL,
    last_used_at    BIGINT,
    expires_at      BIGINT
);
CREATE INDEX idx_app_passwords_user ON app_passwords(user_id);

CREATE TABLE login_flows (
    poll_token      TEXT PRIMARY KEY,
    login_token     TEXT NOT NULL UNIQUE,
    state_token     TEXT NOT NULL UNIQUE,
    client_name     TEXT NOT NULL,
    state           INT NOT NULL,
    server          TEXT,
    login_name      TEXT,
    app_password    TEXT,
    created_at      BIGINT NOT NULL,
    expires_at      BIGINT NOT NULL
);

CREATE TABLE jobs (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    payload         BYTEA,
    run_at          BIGINT NOT NULL,
    started_at      BIGINT,
    completed_at    BIGINT,
    last_error      TEXT,
    attempts        INT NOT NULL DEFAULT 0,
    created_at      BIGINT NOT NULL
);
CREATE INDEX idx_jobs_run_at ON jobs(run_at);

CREATE TABLE module_config (
    module_id       TEXT NOT NULL,
    key             TEXT NOT NULL,
    value           BYTEA,
    PRIMARY KEY (module_id, key)
);
