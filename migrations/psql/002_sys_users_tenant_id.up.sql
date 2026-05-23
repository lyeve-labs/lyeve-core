-- Add tenant_id column to sys_users
-- Multi-tenancy: allows per-tenant user isolation.
-- Existing users get '' (empty string) as default tenant.

BEGIN;

ALTER TABLE sys_users ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_sys_users_tenant ON sys_users (tenant_id);

COMMIT;
