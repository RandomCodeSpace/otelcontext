CREATE TABLE promql_series (
 id VARCHAR(64) PRIMARY KEY,
 tenant_id VARCHAR(128) NOT NULL,
 name TEXT NOT NULL,
 labels TEXT NOT NULL,
 last_timestamp BIGINT NOT NULL
)
-- migrate:split
CREATE INDEX idx_promql_series_tenant_name ON promql_series (tenant_id, name)
-- migrate:split
CREATE INDEX idx_promql_series_last_timestamp ON promql_series (last_timestamp)
-- migrate:split
CREATE TABLE promql_samples (
 id BIGSERIAL PRIMARY KEY,
 series_id VARCHAR(64) NOT NULL,
 timestamp BIGINT NOT NULL,
 start_timestamp BIGINT NOT NULL,
 payload BYTEA NOT NULL
)
-- migrate:split
CREATE UNIQUE INDEX idx_promql_samples_series_time ON promql_samples (series_id, timestamp)
-- migrate:split
CREATE INDEX idx_promql_samples_timestamp ON promql_samples (timestamp)
