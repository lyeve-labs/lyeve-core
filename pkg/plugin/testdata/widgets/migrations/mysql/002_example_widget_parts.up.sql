CREATE TABLE IF NOT EXISTS example_widget_parts (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    widget_id BIGINT NOT NULL,
    label VARCHAR(255) NOT NULL
);
