-- Add token_version column to sys_users (MySQL)

ALTER TABLE `sys_users` ADD COLUMN `token_version` INT NOT NULL DEFAULT 1;
