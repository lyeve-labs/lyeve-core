-- Add token_version column to sys_users (MSSQL)

ALTER TABLE [sys_users] ADD [token_version] INT NOT NULL CONSTRAINT [df_sys_users_token_version] DEFAULT 1;
