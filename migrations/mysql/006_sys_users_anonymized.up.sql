-- Add anonymized flag to sys_users for GDPR right-to-be-forgotten tracking.

ALTER TABLE `sys_users` ADD COLUMN `anonymized` TINYINT(1) NOT NULL DEFAULT 0;
