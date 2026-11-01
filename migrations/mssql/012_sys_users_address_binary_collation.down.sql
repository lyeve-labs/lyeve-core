-- MSSQL: 012_sys_users_address_binary_collation, reversed. The columns go
-- back to the database default collation, which differs between installs.
--
-- The keys are created again under that collation, so the down migration
-- fails while two accounts hold addresses it compares as one, such as
-- straße@ and strasse@. Re-address or remove one of each pair first. A
-- failure leaves the columns and keys as they were.

BEGIN TRY
    BEGIN TRANSACTION;

    IF EXISTS (SELECT 1 FROM sys.key_constraints WHERE name = 'uq_sys_users_email' AND parent_object_id = OBJECT_ID('sys_users'))
        ALTER TABLE [sys_users] DROP CONSTRAINT [uq_sys_users_email];
    IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'uq_sys_users_tenant_provider_email' AND object_id = OBJECT_ID('sys_users'))
        DROP INDEX [uq_sys_users_tenant_provider_email] ON [sys_users];

    ALTER TABLE [sys_users] ALTER COLUMN [email] NVARCHAR(320) COLLATE DATABASE_DEFAULT NOT NULL;
    ALTER TABLE [sys_users] ALTER COLUMN [provider_email] NVARCHAR(320) COLLATE DATABASE_DEFAULT NULL;

    ALTER TABLE [sys_users] ADD CONSTRAINT [uq_sys_users_email] UNIQUE ([email]);
    EXEC('SET QUOTED_IDENTIFIER ON; CREATE UNIQUE INDEX [uq_sys_users_tenant_provider_email] ON [sys_users] ([tenant_id], [provider_email]) WHERE [provider_email] IS NOT NULL;');

    COMMIT TRANSACTION;
END TRY
BEGIN CATCH
    IF @@TRANCOUNT > 0
        ROLLBACK TRANSACTION;
    THROW;
END CATCH;
