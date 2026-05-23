-- Sys_device_logins, a sign-in started on a device and approved in a browser.
--
-- A command line tool asks for a sign-in and gets two codes: a device code it
-- keeps and polls with, stored here only as its SHA-256, and a short user code
-- a person types into the admin while signed in. Approving binds the person,
-- the tenant their session acts in and the token version it was signed under;
-- the next poll exchanges the row for an ordinary session, once, and only if
-- the account's sessions have not been ended since. Rows live ten minutes
-- and are deleted an hour after they expire. user_code is unique over every
-- row, which is stricter than unique while pending and needs no partial
-- index. requester_net is the address an open-request cap counts by: the
-- address itself for IPv4 and its /64 for IPv6, where one host holds the
-- whole prefix.

BEGIN;

CREATE TABLE IF NOT EXISTS sys_device_logins (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    device_code_hash TEXT        NOT NULL,
    user_code        TEXT        NOT NULL,
    client_name      TEXT        NOT NULL,
    requester_ip     TEXT        NOT NULL DEFAULT '',
    requester_net    TEXT        NOT NULL DEFAULT '',
    status           TEXT        NOT NULL DEFAULT 'pending',
    user_id          UUID        NULL,
    tenant_id        TEXT        NULL,
    token_version    INTEGER     NULL,
    poll_interval    INTEGER     NOT NULL DEFAULT 5,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at       TIMESTAMPTZ NOT NULL,
    approved_at      TIMESTAMPTZ NULL,
    last_polled_at   TIMESTAMPTZ NULL,
    CONSTRAINT uq_sys_device_logins_device_code UNIQUE (device_code_hash),
    CONSTRAINT uq_sys_device_logins_user_code UNIQUE (user_code)
);

CREATE INDEX IF NOT EXISTS idx_sys_device_logins_expires ON sys_device_logins (expires_at);
CREATE INDEX IF NOT EXISTS idx_sys_device_logins_requester ON sys_device_logins (requester_net, status);
CREATE INDEX IF NOT EXISTS idx_sys_device_logins_status ON sys_device_logins (status, expires_at);
CREATE INDEX IF NOT EXISTS idx_sys_device_logins_tenant ON sys_device_logins (tenant_id);

COMMIT;
