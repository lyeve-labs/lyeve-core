-- Remove tenant_id column from sys_users

BEGIN;

DROP INDEX IF EXISTS idx_sys_users_tenant;

ALTER TABLE sys_users DROP COLUMN IF EXISTS tenant_id;

COMMIT;
