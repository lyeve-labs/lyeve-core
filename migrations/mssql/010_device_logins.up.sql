-- MSSQL: 010_device_logins
-- sys_device_logins, a sign-in started on a device and approved in a browser.
-- See the psql migration. The code columns are NVARCHAR because a Go string
-- parameter is NVARCHAR, and a VARCHAR column would be converted on every
-- lookup instead of seeking its index.

IF OBJECT_ID(N'sys_device_logins', N'U') IS NULL
CREATE TABLE sys_device_logins (
    id               UNIQUEIDENTIFIER NOT NULL CONSTRAINT df_sys_device_logins_id DEFAULT NEWID(),
    device_code_hash NVARCHAR(64)     NOT NULL,
    user_code        NVARCHAR(16)     NOT NULL,
    client_name      NVARCHAR(64)     NOT NULL,
    requester_ip     NVARCHAR(64)     NOT NULL CONSTRAINT df_sys_device_logins_ip DEFAULT '',
    requester_net    NVARCHAR(64)     NOT NULL CONSTRAINT df_sys_device_logins_net DEFAULT '',
    status           NVARCHAR(16)     NOT NULL CONSTRAINT df_sys_device_logins_status DEFAULT 'pending',
    user_id          UNIQUEIDENTIFIER NULL,
    tenant_id        NVARCHAR(255)    NULL,
    token_version    INT              NULL,
    poll_interval    INT              NOT NULL CONSTRAINT df_sys_device_logins_interval DEFAULT 5,
    created_at       DATETIME2(7)     NOT NULL CONSTRAINT df_sys_device_logins_created DEFAULT SYSUTCDATETIME(),
    expires_at       DATETIME2(7)     NOT NULL,
    approved_at      DATETIME2(7)     NULL,
    last_polled_at   DATETIME2(7)     NULL,
    CONSTRAINT pk_sys_device_logins PRIMARY KEY (id),
    CONSTRAINT uq_sys_device_logins_device_code UNIQUE (device_code_hash),
    CONSTRAINT uq_sys_device_logins_user_code UNIQUE (user_code)
);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_sys_device_logins_expires')
    CREATE INDEX idx_sys_device_logins_expires ON sys_device_logins (expires_at);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_sys_device_logins_requester')
    CREATE INDEX idx_sys_device_logins_requester ON sys_device_logins (requester_net, status);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_sys_device_logins_status')
    CREATE INDEX idx_sys_device_logins_status ON sys_device_logins (status, expires_at);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_sys_device_logins_tenant')
    CREATE INDEX idx_sys_device_logins_tenant ON sys_device_logins (tenant_id);
