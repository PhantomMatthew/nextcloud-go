CREATE TABLE appconfig (
    appid       VARCHAR(191) NOT NULL,
    configkey   VARCHAR(191) NOT NULL,
    configvalue TEXT NOT NULL,
    PRIMARY KEY (appid, configkey)
);
