CREATE TABLE IF NOT EXISTS resource_registry (
    tenant_id text NOT NULL DEFAULT 'default',
    id integer NOT NULL,
    service_name text NOT NULL,
    host text NOT NULL,
    workload text NOT NULL,
    kind text NOT NULL,
    signals integer NOT NULL,
    last_seen datetime,
    PRIMARY KEY (tenant_id, id)
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_resource_registry_last_seen ON resource_registry (last_seen)
