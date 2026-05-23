-- Add disabled and expires_at columns to sys_users (MySQL)
-- Account lockout: disabled flag for manual admin disable.
-- Account expiration: expires_at for time-limited accounts.

ALTER TABLE `sys_users` ADD COLUMN `disabled` TINYINT(1) NOT NULL DEFAULT 0;

ALTER TABLE `sys_users` ADD COLUMN `expires_at` DATETIME(6) NULL DEFAULT NULL;
