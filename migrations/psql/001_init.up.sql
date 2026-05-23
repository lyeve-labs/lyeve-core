-- The engine's own tables. Content type definitions are not stored here: the
-- registered schema engine creates its own tables.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- CMS users (admin panel auth)
CREATE TABLE IF NOT EXISTS sys_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    roles         TEXT[] NOT NULL DEFAULT '{"editor"}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
