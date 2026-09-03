package storage

import (
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/compress"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// CompressedText is a string type that is transparently compressed using zstd before being stored in the database.
// It implements sql.Scanner and driver.Valuer for GORM.
type CompressedText string

const zstdMagic = "\x28\xb5\x2f\xfd" // Zstd magic number (little-endian)

// GormDBDataType returns the dialect-specific binary column type.
// Zstd-compressed bytes include non-text bytes (magic header 0x28 0xB5 0x2F 0xFD)
// so a TEXT column would corrupt data on Postgres; BYTEA is required.
func (CompressedText) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	switch db.Name() {
	case "postgres":
		return "bytea"
	case "mysql":
		return "longblob"
	case "sqlserver":
		return "varbinary(max)"
	default: // sqlite and others
		return "blob"
	}
}

func (ct CompressedText) Value() (driver.Value, error) {
	if ct == "" {
		// The column is binary on every dialect; a string parameter cannot be
		// bound to varbinary on SQL Server (error 257), so bind empty bytes.
		return []byte{}, nil
	}
	compressed := compress.Compress([]byte(ct))
	// Prepend magic header to identify compressed data
	return append([]byte(zstdMagic), compressed...), nil
}

func (ct *CompressedText) Scan(value any) error {
	if value == nil {
		*ct = ""
		return nil
	}

	bytes, ok := value.([]byte)
	if !ok {
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("failed to scan CompressedText: invalid type %T", value)
		}
		bytes = []byte(str)
	}

	if len(bytes) == 0 {
		*ct = ""
		return nil
	}

	// Check for zstd magic header
	if len(bytes) > 4 && string(bytes[:4]) == zstdMagic {
		decompressed, err := compress.Decompress(bytes[4:])
		if err != nil {
			return fmt.Errorf("failed to decompress zstd data: %w", err)
		}
		*ct = CompressedText(decompressed)
	} else {
		// Legacy uncompressed data
		*ct = CompressedText(bytes)
	}
	return nil
}

// DefaultTenantID is the tenant assigned when no X-Tenant-ID header or
// tenant.id resource attribute is present on an OTLP request.
const DefaultTenantID = "default"

// StatusCodeError is the OTLP status string persisted on spans and traces
// whose status code is STATUS_CODE_ERROR. Status columns hold the OTLP code
// name verbatim (STATUS_CODE_OK / STATUS_CODE_ERROR / STATUS_CODE_UNSET), so
// comparisons must use this constant rather than a bare "ERROR". Trace status
// is upgrade-only: a trace row may move UNSET/OK -> ERROR, never the reverse
// (see createTracesIdempotent).
const StatusCodeError = "STATUS_CODE_ERROR"

// Trace represents a complete distributed trace.
//
// Index strategy: single-column tenant_id is redundant — every tenant-scoped
// query joins tenant_id with another filter (timestamp, service_name). The
// leftmost column of a composite index satisfies single-column tenant lookups,
// so we only declare composites. TraceID uniqueness is scoped to (tenant_id,
// trace_id): distinct tenants may legitimately ingest identical trace_ids
// (RAN-21). The old standalone `uniqueIndex` on trace_id is dropped at
// migration time by dropLegacyTraceIDUniqueIndex.
type Trace struct {
	ID          uint    `gorm:"primaryKey" json:"id"`
	TenantID    string  `gorm:"size:64;default:'default';not null;index:idx_traces_tenant_ts,priority:1;index:idx_traces_tenant_service,priority:1;uniqueIndex:idx_traces_tenant_trace_id,priority:1" json:"tenant_id"`
	TraceID     string  `gorm:"size:32;not null;uniqueIndex:idx_traces_tenant_trace_id,priority:2" json:"trace_id"`
	ServiceName string  `gorm:"size:255;index:idx_traces_tenant_service,priority:2" json:"service_name"`
	Duration    int64   `gorm:"index" json:"duration"` // Microseconds
	DurationMs  float64 `gorm:"-" json:"duration_ms"`
	SpanCount   int     `gorm:"-" json:"span_count"`
	Operation   string  `gorm:"-" json:"operation"`
	Status      string  `gorm:"size:50" json:"status"`
	// Timestamp is both part of idx_traces_tenant_ts (composite) and retains a
	// standalone index so range scans on traces across all tenants (e.g.
	// retention sweeps) still use an index.
	Timestamp time.Time `gorm:"index;index:idx_traces_tenant_ts,priority:2" json:"timestamp"`
	// Exemplar truncation metadata (#163's complete-retained-trace contract,
	// written by the aggregate-mode exemplar policy in internal/ingest).
	// All three are nullable and stay NULL on every legacy/shadow-mode row and
	// on any exemplar retained whole — NULL means "no truncation claim was
	// made", which is deliberately distinct from truncated=false.
	//
	// When Truncated is true the trace was cut off by the per-trace span or
	// byte bound: RetainedSpanCount spans reached the DB out of
	// ObservedSpanCount seen. Causal-analysis tools must report partial
	// coverage for such a trace rather than fabricated certainty.
	Truncated         *bool          `gorm:"column:truncated" json:"truncated,omitempty"`
	RetainedSpanCount *int           `gorm:"column:retained_span_count" json:"retained_span_count,omitempty"`
	ObservedSpanCount *int           `gorm:"column:observed_span_count" json:"observed_span_count,omitempty"`
	Spans             []Span         `gorm:"foreignKey:TraceID;references:TraceID;constraint:false" json:"spans,omitempty"`
	Logs              []Log          `gorm:"foreignKey:TraceID;references:TraceID;constraint:false" json:"logs,omitempty"`
	CreatedAt         time.Time      `json:"-"`
	UpdatedAt         time.Time      `json:"-"`
	DeletedAt         gorm.DeletedAt `gorm:"index" json:"-"`
}

// Span represents a single operation within a trace.
//
// Idempotency: the composite uniqueIndex idx_spans_tenant_trace_span on
// (tenant_id, trace_id, span_id) ensures a span is written at most once
// per tenant. DLQ replay (or any duplicate ingest) collapses cleanly via
// OnConflict.DoNothing in BatchCreateAll/BatchCreateSpans rather than
// double-counting in downstream metrics or GraphRAG. The composite covers
// the legacy idx_spans_tenant_trace as a left-prefix; the legacy index
// is retained for query-plan stability across upgrades.
type Span struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	TenantID      string    `gorm:"size:64;default:'default';not null;index:idx_spans_tenant_trace,priority:1;index:idx_spans_tenant_service_start,priority:1;index:idx_spans_tenant_status_start,priority:1;uniqueIndex:idx_spans_tenant_trace_span,priority:1" json:"tenant_id"`
	TraceID       string    `gorm:"size:32;not null;index:idx_spans_tenant_trace,priority:2;uniqueIndex:idx_spans_tenant_trace_span,priority:2" json:"trace_id"`
	SpanID        string    `gorm:"size:16;not null;uniqueIndex:idx_spans_tenant_trace_span,priority:3" json:"span_id"`
	ParentSpanID  string    `gorm:"size:16" json:"parent_span_id"`
	OperationName string    `gorm:"size:255;index" json:"operation_name"`
	StartTime     time.Time `gorm:"index:idx_spans_tenant_service_start,priority:3;index:idx_spans_tenant_status_start,priority:3" json:"start_time"`
	EndTime       time.Time `json:"end_time"`
	Duration      int64     `json:"duration"`                                                                     // Microseconds
	ServiceName   string    `gorm:"size:255;index:idx_spans_tenant_service_start,priority:2" json:"service_name"` // Originating service
	// Status drives the GraphRAG error signal. The composite (tenant_id, status,
	// start_time) backs per-tenant error scans (status='STATUS_CODE_ERROR' over a
	// time window) far better than a bare low-cardinality status index, which no
	// query used for an indexed equality scan.
	Status         string         `gorm:"size:50;default:'STATUS_CODE_UNSET';index:idx_spans_tenant_status_start,priority:2" json:"status"` // OTLP status code (e.g. STATUS_CODE_ERROR)
	AttributesJSON CompressedText `json:"attributes_json"`                                                                                  // Compressed JSON string
}

// Log represents a log entry associated with a trace.
type Log struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	TenantID       string         `gorm:"size:64;default:'default';not null;index:idx_logs_tenant_ts,priority:1;index:idx_logs_tenant_service,priority:1;index:idx_logs_tenant_severity,priority:1" json:"tenant_id"`
	TraceID        string         `gorm:"index;size:32" json:"trace_id"`
	SpanID         string         `gorm:"size:16" json:"span_id"`
	Severity       string         `gorm:"size:50;index:idx_logs_tenant_severity,priority:2" json:"severity"`
	Body           string         `gorm:"type:text" json:"body"`
	ServiceName    string         `gorm:"size:255;index:idx_logs_tenant_service,priority:2" json:"service_name"`
	AttributesJSON CompressedText `json:"attributes_json"`
	AIInsight      CompressedText `json:"ai_insight"`                                                 // Populated by AI analysis
	Timestamp      time.Time      `gorm:"index;index:idx_logs_tenant_ts,priority:2" json:"timestamp"` // standalone index for global retention sweeps
}

// MetricBucket represents aggregated metric data over a time window (e.g., 10s).
type MetricBucket struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	TenantID       string         `gorm:"size:64;default:'default';not null;index:idx_metrics_tenant_name_bucket,priority:1;index:idx_metrics_tenant_service_bucket,priority:1" json:"tenant_id"`
	Name           string         `gorm:"size:255;not null;index:idx_metrics_tenant_name_bucket,priority:2" json:"name"`
	ServiceName    string         `gorm:"size:255;not null;index:idx_metrics_tenant_service_bucket,priority:2" json:"service_name"`
	TimeBucket     time.Time      `gorm:"not null;index;index:idx_metrics_tenant_name_bucket,priority:3;index:idx_metrics_tenant_service_bucket,priority:3" json:"time_bucket"`
	Min            float64        `json:"min"`
	Max            float64        `json:"max"`
	Sum            float64        `json:"sum"`
	Count          int64          `json:"count"`
	AttributesJSON CompressedText `json:"attributes_json"` // Grouped attributes
}

// ResourceRegistryEntry is one persisted row of the bounded resource registry
// (#279): which service ran on which host/workload, per tenant, and which
// signals it emitted. ID is the FNV-64a of (service_name, host, workload)
// bit-reinterpreted as int64, so the composite primary key stays short on
// every adapter. Signals is a bitmask (traces=1, logs=2, metrics=4).
type ResourceRegistryEntry struct {
	TenantID    string    `gorm:"primaryKey;size:64;default:'default';not null" json:"tenant_id"`
	ID          int64     `gorm:"primaryKey;autoIncrement:false" json:"id"`
	ServiceName string    `gorm:"size:255;not null" json:"service_name"`
	Host        string    `gorm:"size:255;not null" json:"host"`
	Workload    string    `gorm:"size:255;not null" json:"workload"`
	Kind        string    `gorm:"size:16;not null" json:"kind"`
	Signals     int64     `gorm:"not null" json:"signals"`
	LastSeen    time.Time `gorm:"index:idx_resource_registry_last_seen" json:"last_seen"`
}

// TableName overrides GORM's default table name.
func (ResourceRegistryEntry) TableName() string { return "resource_registry" }
