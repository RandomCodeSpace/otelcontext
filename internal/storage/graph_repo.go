package storage

import (
	"time"
)

// SpanGraphRow is the minimal projection needed by the in-memory service graph.
type SpanGraphRow struct {
	SpanID        string
	ParentSpanID  string
	ServiceName   string
	OperationName string
	DurationMs    float64
	IsError       bool
	Timestamp     time.Time
}

// GetSpansForGraph returns a lightweight projection of recent spans used to
// rebuild the in-memory service dependency graph.
//
// Duration is stored in microseconds; we convert to milliseconds here so the
// graph layer doesn't need to know the storage unit.
//
// The query is deliberately cross-tenant — the legacy in-memory graph is
// rebuilt from a background ticker with no request context, so there is no
// tenant to scope by. Trace identity is nevertheless (tenant_id, trace_id):
// the join carries tenant_id so a trace ID reused by two tenants cannot leak
// one tenant's error status onto the other tenant's spans.
func (r *Repository) GetSpansForGraph(since time.Time) ([]SpanGraphRow, error) {
	type raw struct {
		SpanID        string
		ParentSpanID  string
		ServiceName   string
		OperationName string
		Duration      int64
		TraceStatus   string
		StartTime     time.Time
	}

	var rows []raw
	err := r.db.
		Table("spans").
		Select("spans.span_id, spans.parent_span_id, spans.service_name, spans.operation_name, spans.duration, traces.status AS trace_status, spans.start_time").
		Joins("LEFT JOIN traces ON traces.trace_id = spans.trace_id AND traces.tenant_id = spans.tenant_id").
		Where("spans.start_time >= ?", since).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make([]SpanGraphRow, len(rows))
	for i, raw := range rows {
		out[i] = SpanGraphRow{
			SpanID:        raw.SpanID,
			ParentSpanID:  raw.ParentSpanID,
			ServiceName:   raw.ServiceName,
			OperationName: raw.OperationName,
			DurationMs:    float64(raw.Duration) / 1000.0, // µs → ms
			IsError:       raw.TraceStatus == StatusCodeError,
			Timestamp:     raw.StartTime,
		}
	}
	return out, nil
}
