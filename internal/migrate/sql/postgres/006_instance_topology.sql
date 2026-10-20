CREATE TABLE topology_entities (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL,
 environment TEXT NOT NULL,
 namespace TEXT NOT NULL,
 service TEXT NOT NULL,
 instance TEXT NOT NULL,
 host TEXT NOT NULL,
 workload TEXT NOT NULL,
 identity TEXT NOT NULL,
 source TEXT NOT NULL,
 target TEXT NOT NULL,
 signals BIGINT NOT NULL,
 first_seen BIGINT NOT NULL,
 last_seen BIGINT NOT NULL
)
-- migrate:split
CREATE INDEX idx_topology_entities_tenant ON topology_entities (tenant_id, environment)
-- migrate:split
CREATE INDEX idx_topology_entities_last_seen ON topology_entities (last_seen)
-- migrate:split
CREATE TABLE topology_buckets (
 entity_id TEXT NOT NULL,
 window_start BIGINT NOT NULL,
 spans BIGINT NOT NULL,
 requests BIGINT NOT NULL,
 errors BIGINT NOT NULL,
 duration_micros BIGINT NOT NULL,
 PRIMARY KEY (entity_id, window_start)
)
-- migrate:split
CREATE INDEX idx_topology_buckets_window ON topology_buckets (window_start)
