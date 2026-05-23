IF OBJECT_ID(N'example_widgets', N'U') IS NULL
CREATE TABLE example_widgets (
    id BIGINT IDENTITY(1,1) PRIMARY KEY,
    tenant_id NVARCHAR(64) NOT NULL,
    name NVARCHAR(255) NOT NULL
);
