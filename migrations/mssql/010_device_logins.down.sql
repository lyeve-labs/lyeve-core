-- MSSQL: 010_device_logins
-- Drop the device sign-in table.

IF OBJECT_ID(N'sys_device_logins', N'U') IS NOT NULL
    DROP TABLE sys_device_logins;
