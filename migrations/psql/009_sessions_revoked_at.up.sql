-- Sys_users.sessions_revoked_at, when the account's sessions last ended.
--
-- token_version ends the engine's own tokens, which carry the version they
-- were signed under. A trusted issuer's token carries no engine version, so
-- the version alone could never end it: the account is read fresh on every
-- request. The moment of the last bump lets an issuer token issued before it
-- be refused. Set wherever token_version moves; NULL until the first time.

BEGIN;

ALTER TABLE sys_users ADD COLUMN IF NOT EXISTS sessions_revoked_at TIMESTAMPTZ NULL;

COMMIT;
