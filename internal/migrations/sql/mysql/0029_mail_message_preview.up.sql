-- Mail M6 (ADR-0108): sync-time list previews. preview carries the first 200
-- runes of the plain-text body with whitespace runs collapsed ('' when the
-- message has no plain part or exceeded the 1 MiB preview-fetch cap);
-- has_attachments is the list-view paperclip flag.
ALTER TABLE mail_messages ADD COLUMN preview VARCHAR(1024) NOT NULL DEFAULT '';
ALTER TABLE mail_messages ADD COLUMN has_attachments TINYINT NOT NULL DEFAULT 0;
