-- MySQL: 013_sys_users_erasure_pending_at
-- sys_users.erasure_pending_at, when an erasure locked the account. See the
-- psql migration.

ALTER TABLE `sys_users` ADD COLUMN `erasure_pending_at` DATETIME(6) NULL DEFAULT NULL;
