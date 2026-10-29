-- Sys_users.erasure_pending_at, when an erasure locked the account.
--
-- The erasure disables the account as it signs the subject out, and
-- anonymizes it only once every other owner has erased. While this is set,
-- an administrator's enable and a directory's reactivation leave the account
-- disabled, so the person cannot sign back in and write rows behind an
-- eraser that already ran. Anonymization clears it. NULL when no erasure is
-- pending.

BEGIN;

ALTER TABLE sys_users ADD COLUMN IF NOT EXISTS erasure_pending_at TIMESTAMPTZ NULL;

COMMIT;
