-- MySQL: 012_sys_users_address_binary_collation, reversed. The columns go
-- back to the collation of the sys_users table, which they took when it was
-- created. It can differ from the database default, which a later
-- ALTER DATABASE changes without touching the table, so the table's own is
-- read. The collation names its character set.
--
-- The unique indexes are rebuilt under the folding collation, so the down
-- migration fails while two accounts hold addresses it compares as one, such
-- as josé@ and jose@. Re-address or remove one of each pair first.

SET @lyeve_012_collation = (SELECT table_collation FROM information_schema.tables
    WHERE table_schema = DATABASE() AND table_name = 'sys_users');
SET @lyeve_012_sql = CONCAT(
    'ALTER TABLE `sys_users` ',
    'MODIFY `email` VARCHAR(320) COLLATE ', @lyeve_012_collation, ' NOT NULL, ',
    'MODIFY `provider_email` VARCHAR(320) COLLATE ', @lyeve_012_collation, ' NULL DEFAULT NULL');
PREPARE lyeve_012_stmt FROM @lyeve_012_sql;
EXECUTE lyeve_012_stmt;
DEALLOCATE PREPARE lyeve_012_stmt;
