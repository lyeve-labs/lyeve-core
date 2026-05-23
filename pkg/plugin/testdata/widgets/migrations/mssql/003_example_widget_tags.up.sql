IF OBJECT_ID(N'example_widget_tags', N'U') IS NULL
CREATE TABLE example_widget_tags (
    id BIGINT IDENTITY(1,1) PRIMARY KEY,
    tenant_id NVARCHAR(64) NOT NULL,
    widget_id BIGINT NOT NULL,
    tag NVARCHAR(64) NOT NULL
);
