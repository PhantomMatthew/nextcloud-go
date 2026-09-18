CREATE TABLE file_properties (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    file_path       TEXT NOT NULL,
    ns              TEXT NOT NULL,
    name            TEXT NOT NULL,
    value           TEXT NOT NULL DEFAULT '',
    UNIQUE (user_id, file_path, ns, name)
);
CREATE INDEX idx_file_properties_user_path ON file_properties(user_id, file_path);
