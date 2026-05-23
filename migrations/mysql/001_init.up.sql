-- MySQL: 001_init
-- The engine's own tables. Content type definitions belong to the schema engine.
-- Requires MySQL 8.0+ for UUID() default expressions and JSON type.

CREATE TABLE IF NOT EXISTS sys_users (
    id            CHAR(36)     NOT NULL DEFAULT (UUID()) PRIMARY KEY,
    email         VARCHAR(320) NOT NULL,
    password_hash TEXT         NOT NULL,
    roles         JSON         NOT NULL DEFAULT (JSON_ARRAY('editor')),
    created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    CONSTRAINT uq_sys_users_email UNIQUE (email)
);
