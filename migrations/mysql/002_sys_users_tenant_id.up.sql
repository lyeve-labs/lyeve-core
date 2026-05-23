-- Add tenant_id column to sys_users (MySQL)
-- Multi-tenancy: allows per-tenant user isolation.
-- Existing users get '' (empty string) as default tenant.
--
-- MySQL has no ADD COLUMN IF NOT EXISTS and no CREATE INDEX IF NOT EXISTS, so
-- each change reads information_schema first and runs only when it is absent.
-- The script can then run against a database that already holds both.

SET @lyeve_008_col = (SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND column_name = 'tenant_id');
SET @lyeve_008_sql = IF(@lyeve_008_col = 0,
    'ALTER TABLE `sys_users` ADD COLUMN `tenant_id` VARCHAR(255) NOT NULL DEFAULT ''''',
    'SELECT 1');
PREPARE lyeve_008_stmt FROM @lyeve_008_sql;
EXECUTE lyeve_008_stmt;
DEALLOCATE PREPARE lyeve_008_stmt;

SET @lyeve_008_idx = (SELECT COUNT(*) FROM information_schema.statistics
    WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND index_name = 'idx_sys_users_tenant');
SET @lyeve_008_sql = IF(@lyeve_008_idx = 0,
    'CREATE INDEX `idx_sys_users_tenant` ON `sys_users` (`tenant_id`)',
    'SELECT 1');
PREPARE lyeve_008_stmt FROM @lyeve_008_sql;
EXECUTE lyeve_008_stmt;
DEALLOCATE PREPARE lyeve_008_stmt;
