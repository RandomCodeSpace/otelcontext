-- Tenant-scoped span scans bounded by start_time seek one tenant's range.
CREATE INDEX IF NOT EXISTS idx_spans_tenant_start ON spans (tenant_id, start_time)
