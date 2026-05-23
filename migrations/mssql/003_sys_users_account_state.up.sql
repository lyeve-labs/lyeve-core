-- Add disabled and expires_at columns to sys_users (MSSQL)
-- Account lockout: disabled flag for manual admin disable.
-- Account expiration: expires_at for time-limited accounts.

ALTER TABLE [sys_users] ADD [disabled] BIT NOT NULL CONSTRAINT [df_sys_users_disabled] DEFAULT 0;

ALTER TABLE [sys_users] ADD [expires_at] DATETIME2(7) NULL;
