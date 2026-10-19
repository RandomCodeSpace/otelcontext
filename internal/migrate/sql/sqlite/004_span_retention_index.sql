-- Global age-based retention cannot use a tenant-prefixed span index.
CREATE INDEX IF NOT EXISTS idx_spans_start_time ON spans (start_time)
