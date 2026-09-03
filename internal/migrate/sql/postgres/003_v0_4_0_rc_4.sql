CREATE TABLE IF NOT EXISTS resource_registry (
    tenant_id varchar(64) NOT NULL DEFAULT 'default',
    id bigint NOT NULL,
    service_name varchar(255) NOT NULL,
    host varchar(255) NOT NULL,
    workload varchar(255) NOT NULL,
    kind varchar(16) NOT NULL,
    signals bigint NOT NULL,
    last_seen timestamptz,
    PRIMARY KEY (tenant_id, id)
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_resource_registry_last_seen ON resource_registry (last_seen)
