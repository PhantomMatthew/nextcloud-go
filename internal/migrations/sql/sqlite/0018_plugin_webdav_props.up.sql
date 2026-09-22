CREATE TABLE plugin_webdav_props (
    plugin_id TEXT NOT NULL,
    name      TEXT NOT NULL,
    getter    TEXT NOT NULL,
    setter    TEXT NOT NULL,
    PRIMARY KEY (plugin_id, name)
);
