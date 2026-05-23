-- MySQL: 009_sessions_revoked_at
-- sys_users.sessions_revoked_at, when the account's sessions last ended. See
-- the psql migration.

ALTER TABLE `sys_users` ADD COLUMN `sessions_revoked_at` DATETIME(6) NULL DEFAULT NULL;
