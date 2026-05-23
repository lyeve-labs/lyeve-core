-- Remove tenant_id column from sys_users (MySQL)

DROP INDEX `idx_sys_users_tenant` ON `sys_users`;

ALTER TABLE `sys_users` DROP COLUMN `tenant_id`;
