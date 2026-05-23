IF OBJECT_ID(N'example_widget_parts', N'U') IS NULL
CREATE TABLE example_widget_parts (
    id BIGINT IDENTITY(1,1) PRIMARY KEY,
    tenant_id NVARCHAR(64) NOT NULL,
    widget_id BIGINT NOT NULL,
    label NVARCHAR(255) NOT NULL
);
