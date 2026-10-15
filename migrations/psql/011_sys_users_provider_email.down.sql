BEGIN;

DROP INDEX IF EXISTS uq_sys_users_tenant_provider_email;
ALTER TABLE sys_users DROP COLUMN IF EXISTS provider_seen_at;
ALTER TABLE sys_users DROP COLUMN IF EXISTS provider_email;

COMMIT;
