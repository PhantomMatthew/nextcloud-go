CREATE TABLE plugin_routes (
    plugin_id    TEXT NOT NULL,
    kind         TEXT NOT NULL,
    method       TEXT NOT NULL,
    path         TEXT NOT NULL,
    handler_name TEXT NOT NULL,
    PRIMARY KEY (plugin_id, kind, method, path)
);
