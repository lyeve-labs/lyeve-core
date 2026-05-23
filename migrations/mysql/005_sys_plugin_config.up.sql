-- Operator-set plugin configuration, written by PUT /api/admin/plugins/{name}/config.
-- Applied at boot: see SetPluginConfigOverlay. Environment variables still win.
CREATE TABLE IF NOT EXISTS sys_plugin_config (
    tenant_id   VARCHAR(255) NOT NULL DEFAULT '',
    plugin_name VARCHAR(255) NOT NULL,
    config      JSON         NOT NULL,
    updated_at  DATETIME(6)  NOT NULL DEFAULT NOW(6),
    updated_by  CHAR(36),
    PRIMARY KEY (tenant_id, plugin_name)
) ENGINE=InnoDB;
