package storage

import (
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/compress"
	"gorm.io/gorm"
)

// CompressedText is a string type that is transparently compressed using zstd before being stored in the database.
// It implements sql.Scanner and driver.Valuer for GORM.
type CompressedText string

const zstdMagic = "\x28\xb5\x2f\xfd" // Zstd magic number (little-endian)

func (ct CompressedText) Value() (driver.Value, error) {
	if ct == "" {
		return "", nil
	}
	compressed := compress.Compress([]byte(ct))
	// Prepend magic header to identify compressed data
	return append([]byte(zstdMagic), compressed...), nil
}

func (ct *CompressedText) Scan(value interface{}) error {
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

// Trace represents a complete distributed trace.
type Trace struct {
	ID          uint           `gorm:"primaryKey" json:"id"`
	TraceID     string         `gorm:"uniqueIndex;size:32;not null" json:"trace_id"`
	ServiceName string         `gorm:"size:255;index" json:"service_name"`
	Duration    int64          `gorm:"index" json:"duration"` // Microseconds
	DurationMs  float64        `gorm:"-" json:"duration_ms"`
	SpanCount   int            `gorm:"-" json:"span_count"`
	Operation   string         `gorm:"-" json:"operation"`
	Status      string         `gorm:"size:50" json:"status"`
	Timestamp   time.Time      `gorm:"index" json:"timestamp"`
	Spans       []Span         `gorm:"foreignKey:TraceID;references:TraceID;constraint:false" json:"spans,omitempty"`
	Logs        []Log          `gorm:"foreignKey:TraceID;references:TraceID;constraint:false" json:"logs,omitempty"`
	CreatedAt   time.Time      `json:"-"`
	UpdatedAt   time.Time      `json:"-"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

// Span represents a single operation within a trace.
type Span struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	TraceID        string         `gorm:"index;size:32;not null" json:"trace_id"`
	SpanID         string         `gorm:"size:16;not null" json:"span_id"`
	ParentSpanID   string         `gorm:"size:16" json:"parent_span_id"`
	OperationName  string         `gorm:"size:255;index" json:"operation_name"`
	StartTime      time.Time      `json:"start_time"`
	EndTime        time.Time      `json:"end_time"`
	Duration       int64          `json:"duration"`                           // Microseconds
	ServiceName    string         `gorm:"size:255;index" json:"service_name"` // Originating service
	AttributesJSON CompressedText `gorm:"type:blob" json:"attributes_json"`   // Compressed JSON string
}

// Log represents a log entry associated with a trace.
type Log struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	TraceID        string         `gorm:"index;size:32" json:"trace_id"`
	SpanID         string         `gorm:"size:16" json:"span_id"`
	Severity       string         `gorm:"size:50;index" json:"severity"`
	Body           CompressedText `gorm:"type:blob" json:"body"`
	ServiceName    string         `gorm:"size:255;index" json:"service_name"`
	AttributesJSON CompressedText `gorm:"type:blob" json:"attributes_json"`
	AIInsight      CompressedText `gorm:"type:blob" json:"ai_insight"` // Populated by AI analysis
	Timestamp      time.Time      `gorm:"index" json:"timestamp"`
}

// MetricBucket represents aggregated metric data over a time window (e.g., 10s).
type MetricBucket struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	Name           string         `gorm:"size:255;index;not null" json:"name"`
	ServiceName    string         `gorm:"size:255;index;not null" json:"service_name"`
	TimeBucket     time.Time      `gorm:"index;not null" json:"time_bucket"`
	Min            float64        `json:"min"`
	Max            float64        `json:"max"`
	Sum            float64        `json:"sum"`
	Count          int64          `json:"count"`
	AttributesJSON CompressedText `gorm:"type:blob" json:"attributes_json"` // Grouped attributes
}

