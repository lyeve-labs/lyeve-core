BEGIN;

ALTER TABLE sys_users DROP COLUMN IF EXISTS sessions_revoked_at;

COMMIT;
