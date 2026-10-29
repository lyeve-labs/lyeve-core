-- MSSQL: 013_sys_users_erasure_pending_at
-- sys_users.erasure_pending_at, when an erasure locked the account. See the
-- psql migration.

IF COL_LENGTH('sys_users', 'erasure_pending_at') IS NULL
    ALTER TABLE [sys_users] ADD [erasure_pending_at] DATETIME2(7) NULL;
