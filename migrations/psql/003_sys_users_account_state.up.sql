-- Add disabled and expires_at columns to sys_users
-- Account lockout: disabled flag for manual admin disable.
-- Account expiration: expires_at for time-limited accounts.

BEGIN;

ALTER TABLE sys_users ADD COLUMN IF NOT EXISTS disabled BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE sys_users ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

COMMIT;
