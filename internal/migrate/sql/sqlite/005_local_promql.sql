CREATE TABLE promql_series (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL,
 name TEXT NOT NULL,
 labels TEXT NOT NULL,
 last_timestamp INTEGER NOT NULL
)
-- migrate:split
CREATE INDEX idx_promql_series_tenant_name ON promql_series (tenant_id, name)
-- migrate:split
CREATE INDEX idx_promql_series_last_timestamp ON promql_series (last_timestamp)
-- migrate:split
CREATE TABLE promql_samples (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 series_id TEXT NOT NULL,
 timestamp INTEGER NOT NULL,
 start_timestamp INTEGER NOT NULL,
 payload BLOB NOT NULL
)
-- migrate:split
CREATE UNIQUE INDEX idx_promql_samples_series_time ON promql_samples (series_id, timestamp)
-- migrate:split
CREATE INDEX idx_promql_samples_timestamp ON promql_samples (timestamp)
