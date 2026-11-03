IF COL_LENGTH('sys_users', 'erasure_pending_at') IS NOT NULL
    ALTER TABLE [sys_users] DROP COLUMN [erasure_pending_at];
