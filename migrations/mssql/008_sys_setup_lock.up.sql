-- MSSQL: 008_sys_setup_lock
-- sys_setup_lock, the row first-run setup locks. See the psql migration.

IF OBJECT_ID(N'sys_setup_lock', N'U') IS NULL
CREATE TABLE sys_setup_lock (
    id         INT NOT NULL PRIMARY KEY,
    claimed_at DATETIME2(6) NULL
);

IF NOT EXISTS (SELECT 1 FROM sys_setup_lock WHERE id = 1)
    INSERT INTO sys_setup_lock (id) VALUES (1);
