CREATE TABLE IF NOT EXISTS example_widget_tags (
    id BIGSERIAL PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    widget_id BIGINT NOT NULL,
    tag VARCHAR(64) NOT NULL
);
