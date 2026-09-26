ALTER TABLE sessions DROP COLUMN sealed_uk;
ALTER TABLE file_keys DROP COLUMN scheme;
DROP TABLE IF EXISTS user_key_pw;
