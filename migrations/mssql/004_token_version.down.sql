-- Remove token_version column from sys_users (MSSQL)

ALTER TABLE [sys_users] DROP CONSTRAINT IF EXISTS [df_sys_users_token_version];

ALTER TABLE [sys_users] DROP COLUMN IF EXISTS [token_version];
