-- Remove token_version column from sys_users (MySQL)

ALTER TABLE `sys_users` DROP COLUMN `token_version`;
