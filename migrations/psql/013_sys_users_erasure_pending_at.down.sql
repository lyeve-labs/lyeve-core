BEGIN;

ALTER TABLE sys_users DROP COLUMN IF EXISTS erasure_pending_at;

COMMIT;
