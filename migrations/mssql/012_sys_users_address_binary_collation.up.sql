-- MSSQL: 012_sys_users_address_binary_collation
-- The account address columns compare by code point.
--
-- The database default collation compares straße@ and strasse@ as one value,
-- and an address spelled with a ligature as the one spelled without it, and
-- a case- and accent-insensitive default does the same for josé@ and jose@.
-- The unique keys refused the second address of each pair as a duplicate.
-- Under Latin1_General_100_BIN2 each is an address of its own, as on
-- PostgreSQL. Two addresses that differ only by case still name one account,
-- because the engine lowers every address before it stores or looks one up.
--
-- SQL Server refuses to change the collation of a column a constraint or an
-- index covers, so both keys are dropped, the columns altered with their
-- lengths and nullability unchanged, and the keys created again, in one
-- transaction. Equality pads trailing spaces under any collation. The
-- filtered index needs QUOTED_IDENTIFIER ON, and EXEC runs it as a batch of
-- its own with that setting, as migration 011 does.

BEGIN TRY
    BEGIN TRANSACTION;

    IF EXISTS (SELECT 1 FROM sys.key_constraints WHERE name = 'uq_sys_users_email' AND parent_object_id = OBJECT_ID('sys_users'))
        ALTER TABLE [sys_users] DROP CONSTRAINT [uq_sys_users_email];
    IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'uq_sys_users_tenant_provider_email' AND object_id = OBJECT_ID('sys_users'))
        DROP INDEX [uq_sys_users_tenant_provider_email] ON [sys_users];

    ALTER TABLE [sys_users] ALTER COLUMN [email] NVARCHAR(320) COLLATE Latin1_General_100_BIN2 NOT NULL;
    ALTER TABLE [sys_users] ALTER COLUMN [provider_email] NVARCHAR(320) COLLATE Latin1_General_100_BIN2 NULL;

    ALTER TABLE [sys_users] ADD CONSTRAINT [uq_sys_users_email] UNIQUE ([email]);
    EXEC('SET QUOTED_IDENTIFIER ON; CREATE UNIQUE INDEX [uq_sys_users_tenant_provider_email] ON [sys_users] ([tenant_id], [provider_email]) WHERE [provider_email] IS NOT NULL;');

    COMMIT TRANSACTION;
END TRY
BEGIN CATCH
    IF @@TRANCOUNT > 0
        ROLLBACK TRANSACTION;
    THROW;
END CATCH;
