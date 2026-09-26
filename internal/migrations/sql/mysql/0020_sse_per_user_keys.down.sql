DROP INDEX files_key_uuid_idx ON files;
ALTER TABLE files DROP COLUMN key_uuid;
DROP TABLE IF EXISTS file_keys;
DROP TABLE IF EXISTS user_keys;
