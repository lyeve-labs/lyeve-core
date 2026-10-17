-- MSSQL: 011_sys_users_provider_email
-- sys_users.provider_email and provider_seen_at, and the unique index on
-- (tenant_id, provider_email). See the psql migration.
--
-- SQL Server treats NULLs as equal in a unique index, so an unfiltered one
-- would admit a single account without a provider address per tenant. The
-- index is filtered to the rows that hold one.
--
-- A filtered index is created only under QUOTED_IDENTIFIER ON, and without it
-- the batch carries on with the uniqueness silently absent. The file is one
-- batch, compiled before any of it runs, so the index also cannot name a
-- column this batch is still adding. EXEC runs it as a batch of its own, with
-- the setting it needs.

IF COL_LENGTH('sys_users', 'provider_email') IS NULL
    ALTER TABLE [sys_users] ADD [provider_email] NVARCHAR(320) NULL;
IF COL_LENGTH('sys_users', 'provider_seen_at') IS NULL
    ALTER TABLE [sys_users] ADD [provider_seen_at] DATETIME2(7) NULL;

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'uq_sys_users_tenant_provider_email' AND object_id = OBJECT_ID('sys_users'))
    EXEC('SET QUOTED_IDENTIFIER ON; CREATE UNIQUE INDEX [uq_sys_users_tenant_provider_email] ON [sys_users] ([tenant_id], [provider_email]) WHERE [provider_email] IS NOT NULL;');
