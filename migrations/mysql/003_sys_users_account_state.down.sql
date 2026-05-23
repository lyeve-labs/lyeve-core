-- Remove disabled and expires_at columns from sys_users (MySQL)

ALTER TABLE `sys_users` DROP COLUMN `expires_at`;

ALTER TABLE `sys_users` DROP COLUMN `disabled`;
