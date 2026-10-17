-- sys_users.provider_email and provider_seen_at: an account a tenant's
-- identity provider or sign-up form creates belongs to that tenant alone.
--
-- Such an account stores a key in email that no other row can hold, so the
-- install-wide unique address no longer decides whether a tenant may create
-- it. The address the provider asserted goes in provider_email, and the
-- account is found by its tenant plus that address. The unique index below is
-- what keeps two first sign-ins at the same moment from creating two
-- accounts: the second insert fails and the caller reads the winner back.
--
-- provider_seen_at is when the tenant's provider last signed the account in.
-- Password login, reset and magic link refuse an account whose stamp is older
-- than its tenant's window.
--
-- provider_email is typed like email, so a lookup parameter never converts
-- the column. No row is rewritten: existing accounts keep their address in
-- email and are found the way they always were.

BEGIN;

ALTER TABLE sys_users ADD COLUMN IF NOT EXISTS provider_email TEXT NULL;
ALTER TABLE sys_users ADD COLUMN IF NOT EXISTS provider_seen_at TIMESTAMPTZ NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_sys_users_tenant_provider_email
    ON sys_users (tenant_id, provider_email);

COMMIT;
