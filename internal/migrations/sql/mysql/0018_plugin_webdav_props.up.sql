CREATE TABLE plugin_webdav_props (
    plugin_id VARCHAR(191) NOT NULL,
    name      VARCHAR(255) NOT NULL,
    getter    VARCHAR(128) NOT NULL,
    setter    VARCHAR(128) NOT NULL,
    PRIMARY KEY (plugin_id, name)
);
