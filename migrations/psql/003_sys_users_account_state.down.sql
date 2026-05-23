-- Remove disabled and expires_at columns from sys_users

BEGIN;

ALTER TABLE sys_users DROP COLUMN IF EXISTS expires_at;

ALTER TABLE sys_users DROP COLUMN IF EXISTS disabled;

COMMIT;
