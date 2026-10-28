-- MySQL: 012_sys_users_address_binary_collation, reversed. The columns go
-- back to the engine's collation.
--
-- The unique indexes are rebuilt under the folding collation, so the down
-- migration fails while two accounts hold addresses it compares as one, such
-- as josé@ and jose@. Re-address or remove one of each pair first.

ALTER TABLE `sys_users`
    MODIFY `email` VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL,
    MODIFY `provider_email` VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NULL DEFAULT NULL;
