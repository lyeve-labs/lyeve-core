-- MySQL: 010_device_logins
-- sys_device_logins, a sign-in started on a device and approved in a browser.
-- See the psql migration. Indexes are declared inline: MySQL has no
-- CREATE INDEX IF NOT EXISTS, so a separate statement is not re-runnable.

CREATE TABLE IF NOT EXISTS `sys_device_logins` (
    `id`               CHAR(36)     NOT NULL,
    `device_code_hash` VARCHAR(64)  NOT NULL,
    `user_code`        VARCHAR(16)  NOT NULL,
    `client_name`      VARCHAR(64)  NOT NULL,
    `requester_ip`     VARCHAR(64)  NOT NULL DEFAULT '',
    `requester_net`    VARCHAR(64)  NOT NULL DEFAULT '',
    `status`           VARCHAR(16)  NOT NULL DEFAULT 'pending',
    `user_id`          CHAR(36)     NULL,
    `tenant_id`        VARCHAR(255) NULL,
    `token_version`    INT          NULL,
    `poll_interval`    INT          NOT NULL DEFAULT 5,
    `created_at`       DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    `expires_at`       DATETIME(6)  NOT NULL,
    `approved_at`      DATETIME(6)  NULL,
    `last_polled_at`   DATETIME(6)  NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uq_sys_device_logins_device_code` (`device_code_hash`),
    UNIQUE KEY `uq_sys_device_logins_user_code` (`user_code`),
    KEY `idx_sys_device_logins_expires` (`expires_at`),
    KEY `idx_sys_device_logins_requester` (`requester_net`, `status`),
    KEY `idx_sys_device_logins_status` (`status`, `expires_at`),
    KEY `idx_sys_device_logins_tenant` (`tenant_id`)
) ENGINE=InnoDB;
