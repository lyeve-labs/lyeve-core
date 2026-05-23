-- Add token_version column to sys_users
-- Embedded in every JWT (tv claim); bumped on logout to invalidate all
-- tokens issued under the earlier value. When claims.tv < token_version, the
-- token is rejected immediately. Default 1 so a token with no tv claim
-- is always rejected by the middleware (0 < 1).

BEGIN;

ALTER TABLE sys_users ADD COLUMN IF NOT EXISTS token_version INT NOT NULL DEFAULT 1;

COMMIT;
