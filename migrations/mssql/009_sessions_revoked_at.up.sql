-- MSSQL: 009_sessions_revoked_at
-- sys_users.sessions_revoked_at, when the account's sessions last ended. See
-- the psql migration.

IF COL_LENGTH('sys_users', 'sessions_revoked_at') IS NULL
    ALTER TABLE [sys_users] ADD [sessions_revoked_at] DATETIME2(7) NULL;
