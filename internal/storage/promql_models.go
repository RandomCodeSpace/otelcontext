package storage

// PromQLSeries preserves the complete, canonical label set independently of
// window aggregates. ID is the SHA-256 of tenant plus labels.
type PromQLSeries struct {
	ID            string `gorm:"primaryKey;size:64"`
	TenantID      string `gorm:"not null;size:128;index:idx_promql_series_tenant_name,priority:1"`
	Name          string `gorm:"not null;index:idx_promql_series_tenant_name,priority:2"`
	Labels        string `gorm:"not null"`
	LastTimestamp int64  `gorm:"not null;index:idx_promql_series_last_timestamp"`
}

func (PromQLSeries) TableName() string { return "promql_series" }

// Payload is a single Prometheus protobuf sample or native histogram. Binary
// encoding preserves NaN/staleness markers and infinities without SQL coercion.
type PromQLSample struct {
	ID             uint64 `gorm:"primaryKey;autoIncrement"`
	SeriesID       string `gorm:"not null;size:64;uniqueIndex:idx_promql_samples_series_time,priority:1"`
	Timestamp      int64  `gorm:"not null;uniqueIndex:idx_promql_samples_series_time,priority:2;index:idx_promql_samples_timestamp"`
	StartTimestamp int64  `gorm:"not null"`
	Payload        []byte `gorm:"not null"`
}

func (PromQLSample) TableName() string { return "promql_samples" }
