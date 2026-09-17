CREATE TABLE users (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    uid             VARCHAR(255) NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    email           TEXT,
    password_hash   TEXT NOT NULL,
    quota_bytes     BIGINT,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `groups` (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    gid             VARCHAR(255) NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    created_at      BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE group_members (
    group_id        BIGINT NOT NULL,
    user_id         BIGINT NOT NULL,
    PRIMARY KEY (group_id, user_id),
    CONSTRAINT fk_group_members_group FOREIGN KEY (group_id) REFERENCES `groups`(id) ON DELETE CASCADE,
    CONSTRAINT fk_group_members_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE sessions (
    id              VARCHAR(255) PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    user_agent      TEXT,
    ip              VARCHAR(255),
    created_at      BIGINT NOT NULL,
    last_seen_at    BIGINT NOT NULL,
    expires_at      BIGINT NOT NULL,
    CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE app_passwords (
    id              VARCHAR(255) PRIMARY KEY,
    user_id         BIGINT NOT NULL,
    token_hash      VARCHAR(255) NOT NULL UNIQUE,
    login_name      VARCHAR(255) NOT NULL,
    name            VARCHAR(255) NOT NULL,
    type            INT NOT NULL,
    created_at      BIGINT NOT NULL,
    last_used_at    BIGINT,
    expires_at      BIGINT,
    CONSTRAINT fk_app_passwords_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE INDEX idx_app_passwords_user ON app_passwords(user_id);

CREATE TABLE login_flows (
    poll_token      VARCHAR(255) PRIMARY KEY,
    login_token     VARCHAR(255) NOT NULL UNIQUE,
    state_token     VARCHAR(255) NOT NULL UNIQUE,
    client_name     VARCHAR(255) NOT NULL,
    state           INT NOT NULL,
    server          TEXT,
    login_name      VARCHAR(255),
    app_password    TEXT,
    created_at      BIGINT NOT NULL,
    expires_at      BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE jobs (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name            VARCHAR(255) NOT NULL,
    payload         BLOB,
    run_at          BIGINT NOT NULL,
    started_at      BIGINT,
    completed_at    BIGINT,
    last_error      TEXT,
    attempts        INT NOT NULL DEFAULT 0,
    created_at      BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE INDEX idx_jobs_run_at ON jobs(run_at);

CREATE TABLE module_config (
    module_id       VARCHAR(255) NOT NULL,
    `key`           VARCHAR(255) NOT NULL,
    value           BLOB,
    PRIMARY KEY (module_id, `key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
