-- Support bounded retention ordered by age without sorting all expired spans.
CREATE INDEX IF NOT EXISTS idx_spans_start_time ON spans (start_time)
