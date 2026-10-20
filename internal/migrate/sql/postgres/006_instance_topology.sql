CREATE TABLE topology_entities (
 id VARCHAR(64) PRIMARY KEY,
 tenant_id VARCHAR(128) NOT NULL,
 environment VARCHAR(255) NOT NULL,
 namespace VARCHAR(255) NOT NULL,
 service VARCHAR(255) NOT NULL,
 instance VARCHAR(255) NOT NULL,
 host VARCHAR(255) NOT NULL,
 workload VARCHAR(255) NOT NULL,
 identity VARCHAR(32) NOT NULL,
 source VARCHAR(64) NOT NULL,
 target VARCHAR(64) NOT NULL,
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
 entity_id VARCHAR(64) NOT NULL,
 window_start BIGINT NOT NULL,
 spans BIGINT NOT NULL,
 requests BIGINT NOT NULL,
 errors BIGINT NOT NULL,
 duration_micros BIGINT NOT NULL,
 PRIMARY KEY (entity_id, window_start)
)
-- migrate:split
CREATE INDEX idx_topology_buckets_window ON topology_buckets (window_start)
