-- Remove tenant_id column from sys_users (MSSQL)

IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_sys_users_tenant')
    DROP INDEX [idx_sys_users_tenant] ON [sys_users];

ALTER TABLE [sys_users] DROP CONSTRAINT IF EXISTS [df_sys_users_tenant_id];

ALTER TABLE [sys_users] DROP COLUMN IF EXISTS [tenant_id];
