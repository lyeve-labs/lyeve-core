-- Operator-set plugin configuration, written by PUT /api/admin/plugins/{name}/config.
-- Applied at boot: see SetPluginConfigOverlay. Environment variables still win.
CREATE TABLE IF NOT EXISTS sys_plugin_config (
    tenant_id   TEXT        NOT NULL DEFAULT '',
    plugin_name TEXT        NOT NULL,
    config      JSONB       NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by  UUID,
    PRIMARY KEY (tenant_id, plugin_name)
);
