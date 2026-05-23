IF COL_LENGTH('sys_users', 'sessions_revoked_at') IS NOT NULL
    ALTER TABLE [sys_users] DROP COLUMN [sessions_revoked_at];
