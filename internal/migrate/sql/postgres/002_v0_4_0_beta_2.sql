ALTER TABLE traces ADD COLUMN IF NOT EXISTS truncated boolean
-- migrate:split
ALTER TABLE traces ADD COLUMN IF NOT EXISTS retained_span_count bigint
-- migrate:split
ALTER TABLE traces ADD COLUMN IF NOT EXISTS observed_span_count bigint
-- migrate:split
DROP INDEX IF EXISTS idx_spans_status
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_spans_tenant_status_start ON spans (tenant_id, status, start_time)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_spans_start_time_brin ON spans USING BRIN (start_time)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_traces_timestamp_brin ON traces USING BRIN (timestamp)
-- migrate:split
UPDATE investigations SET tenant_id = 'default' WHERE tenant_id IS NULL OR tenant_id = ''
-- migrate:split
UPDATE drain_templates SET tenant_id = 'default' WHERE tenant_id IS NULL OR tenant_id = ''
