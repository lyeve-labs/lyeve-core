-- Drop the device sign-in table.

BEGIN;

DROP INDEX IF EXISTS idx_sys_device_logins_tenant;
DROP INDEX IF EXISTS idx_sys_device_logins_status;
DROP INDEX IF EXISTS idx_sys_device_logins_requester;
DROP INDEX IF EXISTS idx_sys_device_logins_expires;
DROP TABLE IF EXISTS sys_device_logins;

COMMIT;
