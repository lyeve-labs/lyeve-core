-- MySQL: 011_sys_users_provider_email, reversed. Each step runs only when
-- what it removes is present.

SET @lyeve_011_idx = (SELECT COUNT(*) FROM information_schema.statistics
    WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND index_name = 'uq_sys_users_tenant_provider_email');
SET @lyeve_011_sql = IF(@lyeve_011_idx > 0,
    'DROP INDEX `uq_sys_users_tenant_provider_email` ON `sys_users`',
    'SELECT 1');
PREPARE lyeve_011_stmt FROM @lyeve_011_sql;
EXECUTE lyeve_011_stmt;
DEALLOCATE PREPARE lyeve_011_stmt;

SET @lyeve_011_col = (SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND column_name = 'provider_seen_at');
SET @lyeve_011_sql = IF(@lyeve_011_col > 0,
    'ALTER TABLE `sys_users` DROP COLUMN `provider_seen_at`',
    'SELECT 1');
PREPARE lyeve_011_stmt FROM @lyeve_011_sql;
EXECUTE lyeve_011_stmt;
DEALLOCATE PREPARE lyeve_011_stmt;

SET @lyeve_011_col = (SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND column_name = 'provider_email');
SET @lyeve_011_sql = IF(@lyeve_011_col > 0,
    'ALTER TABLE `sys_users` DROP COLUMN `provider_email`',
    'SELECT 1');
PREPARE lyeve_011_stmt FROM @lyeve_011_sql;
EXECUTE lyeve_011_stmt;
DEALLOCATE PREPARE lyeve_011_stmt;
