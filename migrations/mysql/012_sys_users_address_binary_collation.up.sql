-- MySQL: 012_sys_users_address_binary_collation
-- The account address columns compare by byte.
--
-- Under the database default utf8mb4_unicode_ci, josé@ and jose@ are one
-- value, and so are straße@ and strasse@, and an address spelled with a
-- ligature and one spelled without it. The unique keys refused the second
-- address of each pair as a duplicate. Under utf8mb4_bin each is an address
-- of its own, as on PostgreSQL. Two addresses that differ only by case still
-- name one account, because the engine lowers every address before it stores
-- or looks one up.
--
-- utf8mb4_bin pads trailing spaces as the old collation did. The lengths and
-- nullability are unchanged. MySQL copies the table to rebuild both unique
-- indexes, and writes to sys_users wait until the copy ends.

ALTER TABLE `sys_users`
    MODIFY `email` VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    MODIFY `provider_email` VARCHAR(320) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL DEFAULT NULL;
