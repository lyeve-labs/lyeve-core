-- MSSQL: 011_sys_users_provider_email, reversed.

IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'uq_sys_users_tenant_provider_email' AND object_id = OBJECT_ID('sys_users'))
    DROP INDEX [uq_sys_users_tenant_provider_email] ON [sys_users];
IF COL_LENGTH('sys_users', 'provider_seen_at') IS NOT NULL
    ALTER TABLE [sys_users] DROP COLUMN [provider_seen_at];
IF COL_LENGTH('sys_users', 'provider_email') IS NOT NULL
    ALTER TABLE [sys_users] DROP COLUMN [provider_email];
