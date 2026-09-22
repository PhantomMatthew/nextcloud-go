CREATE TABLE appconfig (
    appid       TEXT NOT NULL,
    configkey   TEXT NOT NULL,
    configvalue TEXT NOT NULL,
    PRIMARY KEY (appid, configkey)
);
