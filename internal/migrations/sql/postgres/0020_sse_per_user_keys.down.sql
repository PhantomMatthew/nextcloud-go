DROP INDEX IF EXISTS files_key_uuid_idx;
ALTER TABLE files DROP COLUMN key_uuid;
DROP TABLE IF EXISTS file_keys;
DROP TABLE IF EXISTS user_keys;
