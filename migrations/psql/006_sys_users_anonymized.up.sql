-- Add anonymized flag to sys_users for GDPR right-to-be-forgotten tracking.
-- An erasure marks the subject anonymized=true, so re-erasure is an idempotent
-- no-op and anonymized accounts are distinguishable.

BEGIN;

ALTER TABLE sys_users ADD COLUMN IF NOT EXISTS anonymized BOOLEAN NOT NULL DEFAULT false;

COMMIT;
