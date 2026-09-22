CREATE TABLE plugin_routes (
    plugin_id    VARCHAR(191) NOT NULL,
    kind         VARCHAR(8) NOT NULL,
    method       VARCHAR(8) NOT NULL,
    path         VARCHAR(255) NOT NULL,
    handler_name VARCHAR(128) NOT NULL,
    PRIMARY KEY (plugin_id, kind, method, path)
);
