-- Operator-set plugin configuration, written by PUT /api/admin/plugins/{name}/config.
-- Applied at boot: see SetPluginConfigOverlay. Environment variables still win.
IF OBJECT_ID(N'sys_plugin_config', N'U') IS NULL
CREATE TABLE sys_plugin_config (
    tenant_id   NVARCHAR(255)    NOT NULL DEFAULT '',
    plugin_name NVARCHAR(255)    NOT NULL,
    config      NVARCHAR(MAX)    NOT NULL,
    updated_at  DATETIME2(7)     NOT NULL DEFAULT SYSUTCDATETIME(),
    updated_by  UNIQUEIDENTIFIER,
    CONSTRAINT pk_sys_plugin_config PRIMARY KEY (tenant_id, plugin_name)
);
