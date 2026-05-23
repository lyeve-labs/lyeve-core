-- Remove disabled and expires_at columns from sys_users (MSSQL)

ALTER TABLE [sys_users] DROP CONSTRAINT IF EXISTS [df_sys_users_disabled];

ALTER TABLE [sys_users] DROP COLUMN IF EXISTS [expires_at];

ALTER TABLE [sys_users] DROP COLUMN IF EXISTS [disabled];
