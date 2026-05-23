-- MSSQL: 001_init
-- The engine's own tables. Content type definitions belong to the schema engine.
-- Requires SQL Server 2016+ for JSON support (NVARCHAR(MAX) + JSON functions).

IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'sys_users')
CREATE TABLE sys_users (
    id            UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID() PRIMARY KEY,
    email         NVARCHAR(320)    NOT NULL,
    password_hash NVARCHAR(MAX)    NOT NULL,
    roles         NVARCHAR(MAX)    NOT NULL DEFAULT '["editor"]',
    created_at    DATETIME2        NOT NULL DEFAULT GETUTCDATE(),
    updated_at    DATETIME2        NOT NULL DEFAULT GETUTCDATE(),
    CONSTRAINT uq_sys_users_email UNIQUE (email)
);
