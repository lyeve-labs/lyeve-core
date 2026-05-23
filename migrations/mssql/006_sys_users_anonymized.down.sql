ALTER TABLE [sys_users] DROP CONSTRAINT IF EXISTS [df_sys_users_anonymized];
ALTER TABLE [sys_users] DROP COLUMN IF EXISTS [anonymized];
