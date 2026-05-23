-- Remove token_version column from sys_users

BEGIN;

ALTER TABLE sys_users DROP COLUMN IF EXISTS token_version;

COMMIT;
