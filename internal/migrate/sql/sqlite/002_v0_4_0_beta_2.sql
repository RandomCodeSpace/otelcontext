ALTER TABLE traces ADD COLUMN truncated numeric
-- migrate:split
ALTER TABLE traces ADD COLUMN retained_span_count integer
-- migrate:split
ALTER TABLE traces ADD COLUMN observed_span_count integer
-- migrate:split
DROP INDEX IF EXISTS idx_spans_status
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_spans_tenant_status_start ON spans (tenant_id, status, start_time)
-- migrate:split
UPDATE investigations SET tenant_id = 'default' WHERE tenant_id IS NULL OR tenant_id = ''
-- migrate:split
UPDATE drain_templates SET tenant_id = 'default' WHERE tenant_id IS NULL OR tenant_id = ''
