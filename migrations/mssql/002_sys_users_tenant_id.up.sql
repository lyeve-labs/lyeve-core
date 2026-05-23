-- Add tenant_id column to sys_users (MSSQL)
-- Multi-tenancy: allows per-tenant user isolation.
-- Existing users get '' (empty string) as default tenant.

ALTER TABLE [sys_users] ADD [tenant_id] NVARCHAR(255) NOT NULL CONSTRAINT [df_sys_users_tenant_id] DEFAULT '';

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_sys_users_tenant')
    CREATE INDEX [idx_sys_users_tenant] ON [sys_users] ([tenant_id]);
