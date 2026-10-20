package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
	"gorm.io/gorm"
)

// TraceReadLimits bounds association rows and payload bytes independently, so
// correlated logs cannot consume the span budget. Bytes bound stored payloads
// and decoded span payloads, excluding fixed-size fields and JSON framing.
type TraceReadLimits struct {
	Spans, Logs         int
	SpanBytes, LogBytes int
}

type BoundedTrace struct {
	Trace
	TotalSpans, TotalLogs         int64
	SpansTruncated, LogsTruncated bool
}

// GetTraceBounded is the bounded evidence read. GetTrace keeps its existing
// full-association contract for other callers. Log attributes and AI insights
// are not selected because trace evidence only needs the log summary fields.
func (r *Repository) GetTraceBounded(ctx context.Context, traceID string, limits TraceReadLimits) (*BoundedTrace, error) {
	if limits.Spans <= 0 || limits.Logs <= 0 || limits.SpanBytes <= 0 || limits.LogBytes <= 0 {
		return nil, fmt.Errorf("trace read limits must be positive")
	}
	tenant := TenantFromContext(ctx)
	var out BoundedTrace
	if err := r.db.WithContext(ctx).
		Select("id, tenant_id, trace_id, truncated, retained_span_count, observed_span_count").
		Where("tenant_id = ? AND trace_id = ?", tenant, traceID).First(&out.Trace).Error; err != nil {
		return nil, fmt.Errorf("failed to get trace: %w", err)
	}
	base := func(model any) *gorm.DB {
		return r.db.WithContext(ctx).Model(model).Where("tenant_id = ? AND trace_id = ?", tenant, traceID)
	}
	if err := base(&Span{}).Count(&out.TotalSpans).Error; err != nil {
		return nil, err
	}
	if err := r.withLogTraceIndex(base(&Log{}), traceID).Count(&out.TotalLogs).Error; err != nil {
		return nil, err
	}

	// Do not invoke CompressedText.Scan: its general-purpose decode budget is
	// much larger than this response's entire payload budget.
	var spans []struct {
		Span          `gorm:"embedded"`
		RawAttributes []byte `gorm:"column:raw_attributes"`
	}
	spanColumns := []string{"trace_id", "span_id", "parent_span_id", "operation_name", "service_name", "status", "attributes_json"}
	if err := r.boundedTraceRows(ctx, tenant, traceID, "spans", spanColumns,
		[]string{"start_time", "span_id", "id"}, limits.Spans, limits.SpanBytes).
		Select("spans.id, spans.trace_id, spans.span_id, spans.parent_span_id, spans.operation_name, spans.service_name, spans.status, spans.start_time, spans.end_time, spans.duration, spans.attributes_json AS raw_attributes").
		Find(&spans).Error; err != nil {
		return nil, err
	}

	remaining := limits.SpanBytes
	for _, row := range spans {
		sp := row.Span
		remaining -= len(sp.TraceID) + len(sp.SpanID) + len(sp.ParentSpanID) + len(sp.OperationName) + len(sp.ServiceName) + len(sp.Status)
	}
	var decoder *zstd.Decoder
	defer func() {
		if decoder != nil {
			decoder.Close()
		}
	}()
	// One scratch buffer is reused; the sum of retained decoded strings stays
	// within remaining, including legacy uncompressed attributes.
	scratch := make([]byte, 0, max(remaining, 0))
	for _, row := range spans {
		sp := row.Span
		attrs := row.RawAttributes
		if len(attrs) > 4 && string(attrs[:4]) == zstdMagic {
			if decoder == nil {
				var err error
				decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(8<<20), zstd.WithDecodeAllCapLimit(true))
				if err != nil {
					return nil, err
				}
			}
			var err error
			attrs, err = decoder.DecodeAll(attrs[4:], scratch[:0:max(remaining, 0)])
			if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
				attrs = nil
				out.SpansTruncated = true
			} else if err != nil {
				return nil, fmt.Errorf("failed to decode trace span attributes: %w", err)
			}
		}
		if len(attrs) > remaining {
			out.SpansTruncated = true
		} else {
			sp.AttributesJSON = CompressedText(attrs)
			remaining -= len(attrs)
		}
		out.Spans = append(out.Spans, sp)
	}
	logColumns := []string{"trace_id", "span_id", "severity", "service_name", "body"}
	if err := r.boundedTraceRows(ctx, tenant, traceID, "logs", logColumns,
		[]string{"timestamp", "id"}, limits.Logs, limits.LogBytes).
		Select("logs.id, logs.trace_id, logs.span_id, logs.timestamp, logs.severity, logs.service_name, logs.body").
		Find(&out.Logs).Error; err != nil {
		return nil, err
	}
	out.SpansTruncated = out.SpansTruncated || out.TotalSpans != int64(len(out.Spans))
	out.LogsTruncated = out.TotalLogs != int64(len(out.Logs))
	return &out, nil
}

// Select a stable prefix by row count, then by cumulative bytes, entirely in
// SQL before loading payloads. The candidate sort carries only keys and sizes.
// All identifiers here are fixed by the two callers above, never user input.
func (r *Repository) boundedTraceRows(ctx context.Context, tenant, traceID, table string, columns, order []string, limit, byteLimit int) *gorm.DB {
	sizes := make([]string, len(columns))
	for i, column := range columns {
		size := "LENGTH(CAST(" + column + " AS BLOB))"
		switch strings.ToLower(r.driver) {
		case "postgres", "postgresql", "mysql":
			size = "OCTET_LENGTH(" + column + ")"
		case "sqlserver", "mssql":
			// NVARCHAR uses UTF-16; twice its stored size also bounds UTF-8.
			size = "2 * CAST(DATALENGTH(" + column + ") AS BIGINT)"
		}
		sizes[i] = "COALESCE(" + size + ", 0)"
	}
	orderBy := strings.Join(order, ", ")
	candidates := r.db.WithContext(ctx).Table(table).
		Select(orderBy+", "+strings.Join(sizes, " + ")+" AS payload_bytes").
		Where("tenant_id = ? AND trace_id = ?", tenant, traceID).Order(orderBy).Limit(limit)
	if table == "logs" {
		candidates = r.withLogTraceIndex(candidates, traceID)
	}
	budget := r.db.Table("(?) AS candidates", candidates).
		Select(orderBy + ", SUM(payload_bytes) OVER (ORDER BY " + orderBy + " ROWS UNBOUNDED PRECEDING) AS total_bytes")
	qualifiedOrder := make([]string, len(order))
	for i, column := range order {
		qualifiedOrder[i] = table + "." + column
	}
	// Include the timestamp in the join: partitioned PostgreSQL log tables
	// use (id, timestamp), rather than id alone, as their primary key.
	join := "JOIN (?) AS trace_budget ON trace_budget.id = " + table + ".id AND trace_budget." + order[0] + " = " + table + "." + order[0]
	query := r.db.WithContext(ctx).Table(table).Joins(join, budget)
	if table == "logs" && (r.driver == "" || strings.EqualFold(r.driver, "sqlite")) {
		// CROSS JOIN keeps the bounded budget outermost, so each payload read
		// seeks by integer primary key instead of walking all tenant logs.
		query = r.db.WithContext(ctx).Table("(?) AS trace_budget", budget).
			Joins("CROSS JOIN logs ON trace_budget.id = logs.id AND trace_budget.timestamp = logs.timestamp")
	}
	return query.
		Where(table+".tenant_id = ? AND "+table+".trace_id = ? AND trace_budget.total_bytes <= ?", tenant, traceID, byteLimit).
		Order(strings.Join(qualifiedOrder, ", ")).Limit(limit)
}
