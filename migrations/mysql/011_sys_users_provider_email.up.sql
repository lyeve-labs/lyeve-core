-- MySQL: 011_sys_users_provider_email
-- sys_users.provider_email and provider_seen_at, and the unique index on
-- (tenant_id, provider_email). See the psql migration.
--
-- MySQL has no ADD COLUMN IF NOT EXISTS and no CREATE INDEX IF NOT EXISTS, so
-- each change reads information_schema first and runs only when it is absent.
-- A unique index admits any number of rows holding NULL, so accounts without a
-- provider address never collide.

SET @lyeve_011_col = (SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND column_name = 'provider_email');
SET @lyeve_011_sql = IF(@lyeve_011_col = 0,
    'ALTER TABLE `sys_users` ADD COLUMN `provider_email` VARCHAR(320) NULL DEFAULT NULL',
    'SELECT 1');
PREPARE lyeve_011_stmt FROM @lyeve_011_sql;
EXECUTE lyeve_011_stmt;
DEALLOCATE PREPARE lyeve_011_stmt;

SET @lyeve_011_col = (SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND column_name = 'provider_seen_at');
SET @lyeve_011_sql = IF(@lyeve_011_col = 0,
    'ALTER TABLE `sys_users` ADD COLUMN `provider_seen_at` DATETIME(6) NULL DEFAULT NULL',
    'SELECT 1');
PREPARE lyeve_011_stmt FROM @lyeve_011_sql;
EXECUTE lyeve_011_stmt;
DEALLOCATE PREPARE lyeve_011_stmt;

SET @lyeve_011_idx = (SELECT COUNT(*) FROM information_schema.statistics
    WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND index_name = 'uq_sys_users_tenant_provider_email');
SET @lyeve_011_sql = IF(@lyeve_011_idx = 0,
    'CREATE UNIQUE INDEX `uq_sys_users_tenant_provider_email` ON `sys_users` (`tenant_id`, `provider_email`)',
    'SELECT 1');
PREPARE lyeve_011_stmt FROM @lyeve_011_sql;
EXECUTE lyeve_011_stmt;
DEALLOCATE PREPARE lyeve_011_stmt;
