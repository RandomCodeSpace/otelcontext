package storage

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TracesResponse represents the response for the traces endpoint with pagination
type TracesResponse struct {
	Traces []Trace `json:"traces"`
	Total  int64   `json:"total"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}

// ServiceMapNode represents a single service node on the service map.
type ServiceMapNode struct {
	Name         string  `json:"name"`
	TotalTraces  int64   `json:"total_traces"`
	ErrorCount   int64   `json:"error_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
}

// ServiceMapEdge represents a connection between two services.
type ServiceMapEdge struct {
	Source       string  `json:"source"`
	Target       string  `json:"target"`
	CallCount    int64   `json:"call_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
}

// ServiceMapMetrics holds the complete service topology with metrics.
type ServiceMapMetrics struct {
	Nodes []ServiceMapNode `json:"nodes"`
	Edges []ServiceMapEdge `json:"edges"`
}

// BatchCreateSpans inserts multiple spans, skipping duplicates.
// Duplicate is defined per the composite uniqueIndex idx_spans_tenant_trace_span
// on (tenant_id, trace_id, span_id): a (tenant, trace, span) clash is silently
// absorbed so DLQ replays (or any duplicate ingest) collapse to a no-op rather
// than double-inserting.
func (r *Repository) BatchCreateSpans(spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	if err := createSpansIdempotent(r.db, r.driver, spans); err != nil {
		return fmt.Errorf("failed to batch create spans: %w", err)
	}
	return nil
}

// createSpansIdempotent runs the conflict-tolerant span insert against an
// arbitrary *gorm.DB so the same logic is reused inside a transaction by
// BatchCreateAll. MySQL takes INSERT IGNORE; SQLite/Postgres/SQL Server take
// ON CONFLICT DO NOTHING via the gorm clause helper.
func createSpansIdempotent(db *gorm.DB, driver string, spans []Span) error {
	if strings.ToLower(driver) == "mysql" {
		return db.Clauses(clause.Insert{Modifier: "IGNORE"}).CreateInBatches(spans, 500).Error
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(spans, 500).Error
}

// BatchCreateTraces inserts traces, skipping duplicates.
// Duplicate is defined per the composite uniqueIndex idx_traces_tenant_trace_id
// on (tenant_id, trace_id): a trace_id clash within the same tenant is ignored,
// while the same trace_id under a different tenant inserts cleanly.
func (r *Repository) BatchCreateTraces(traces []Trace) error {
	if len(traces) == 0 {
		return nil
	}
	return createTracesIdempotent(r.db, r.driver, traces)
}

// createTracesIdempotent runs the conflict-tolerant trace insert against an
// arbitrary *gorm.DB so the same logic is reused inside a transaction by
// BatchCreateAll. MySQL takes INSERT IGNORE; SQLite/Postgres take
// ON CONFLICT DO NOTHING via the gorm clause helper.
func createTracesIdempotent(db *gorm.DB, driver string, traces []Trace) error {
	if strings.ToLower(driver) == "mysql" {
		return db.Clauses(clause.Insert{Modifier: "IGNORE"}).Create(&traces).Error
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&traces).Error
}

// BatchCreateAll persists traces, spans, and logs in a single DB transaction.
// The async ingest pipeline uses this path so a failure (or panic) mid-batch
// rolls back any partial commit, preventing orphan FK rows from a worker that
// crashed between BatchCreateTraces and BatchCreateSpans.
//
// Idempotency: traces and spans both collapse duplicates silently —
//   - traces via idx_traces_tenant_trace_id on (tenant_id, trace_id)
//   - spans  via idx_spans_tenant_trace_span on (tenant_id, trace_id, span_id)
//
// so a DLQ replay of an already-persisted batch is a safe no-op for those
// signals. Logs do not yet have a unique key (OTLP logs lack a stable
// identifier) and a replay can still produce duplicate log rows; that is a
// separate idempotency concern out of scope for this method.
func (r *Repository) BatchCreateAll(traces []Trace, spans []Span, logs []Log) error {
	if len(traces) == 0 && len(spans) == 0 && len(logs) == 0 {
		return nil
	}
	return r.db.Transaction(func(tx *gorm.DB) error {
		if len(traces) > 0 {
			if err := createTracesIdempotent(tx, r.driver, traces); err != nil {
				return fmt.Errorf("BatchCreateAll: traces: %w", err)
			}
		}
		if len(spans) > 0 {
			if err := createSpansIdempotent(tx, r.driver, spans); err != nil {
				return fmt.Errorf("BatchCreateAll: spans: %w", err)
			}
		}
		if len(logs) > 0 {
			if err := tx.CreateInBatches(logs, 500).Error; err != nil {
				return fmt.Errorf("BatchCreateAll: logs: %w", err)
			}
		}
		return nil
	})
}

// CreateTrace inserts a new trace, skipping if it already exists.
// Uniqueness is per idx_traces_tenant_trace_id (tenant_id, trace_id), so the
// same trace_id across tenants is allowed.
func (r *Repository) CreateTrace(trace Trace) error {
	if strings.ToLower(r.driver) == "mysql" {
		return r.db.Clauses(clause.Insert{Modifier: "IGNORE"}).Create(&trace).Error
	}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&trace).Error
}

// GetTrace returns a trace by ID with its spans and logs, scoped to the tenant on ctx.
// Trace uniqueness is composite (tenant_id, trace_id), so the same trace_id can
// legitimately exist in multiple tenants; the Preloaded Spans and Logs are
// filtered by tenant_id as defense-in-depth against cross-tenant child leakage.
func (r *Repository) GetTrace(ctx context.Context, traceID string) (*Trace, error) {
	tenant := TenantFromContext(ctx)
	var trace Trace
	if err := r.db.WithContext(ctx).
		Preload("Spans", sqlWhereTenantID, tenant).
		Preload("Logs", sqlWhereTenantID, tenant).
		Where("tenant_id = ? AND trace_id = ?", tenant, traceID).
		First(&trace).Error; err != nil {
		return nil, fmt.Errorf("failed to get trace: %w", err)
	}
	return &trace, nil
}

// spanSummary is a lightweight struct used to enrich trace list items.
type spanSummary struct {
	TraceID       string
	SpanCount     int
	OperationName string
}

// GetTracesFiltered retrieves traces with filtering and pagination, scoped to
// the tenant on ctx. Spans are NOT eagerly loaded — a single batch summary query
// is used instead.
func (r *Repository) GetTracesFiltered(ctx context.Context, start, end time.Time, serviceNames []string, status, search string, limit, offset int, sortBy, orderBy string) (*TracesResponse, error) {
	tenant := TenantFromContext(ctx)
	var traces []Trace
	var total int64

	base := r.db.WithContext(ctx).Model(&Trace{}).Where(sqlWhereTenantID, tenant)

	if !start.IsZero() && !end.IsZero() {
		base = base.Where("timestamp BETWEEN ? AND ?", start, end)
	}
	if len(serviceNames) > 0 {
		base = base.Where("service_name IN ?", serviceNames)
	}
	op := r.likeOp()
	if status != "" {
		base = base.Where(fmt.Sprintf("status %s ?", op), "%"+status+"%")
	}
	if search != "" {
		base = base.Where(fmt.Sprintf("trace_id %s ?", op), "%"+search+"%")
	}

	orderClause := "timestamp DESC"
	if sortBy != "" {
		direction := "ASC"
		if strings.ToLower(orderBy) == "desc" {
			direction = "DESC"
		}
		validSorts := map[string]string{
			"timestamp":    "timestamp",
			"duration":     "duration",
			"service_name": "service_name",
			"status":       "status",
			"trace_id":     "trace_id",
		}
		if field, ok := validSorts[sortBy]; ok {
			orderClause = fmt.Sprintf("%s %s", field, direction)
		}
	}

	// Run COUNT and SELECT in parallel using independent sessions.
	var g errgroup.Group
	g.Go(func() error {
		return base.Session(&gorm.Session{}).Count(&total).Error
	})
	g.Go(func() error {
		return base.Session(&gorm.Session{}).Order(orderClause).Limit(limit).Offset(offset).Find(&traces).Error
	})
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("failed to fetch traces: %w", err)
	}

	// Enrich traces with span summary via a single batch query (no N+1, no full span load).
	if len(traces) > 0 {
		traceIDs := make([]string, len(traces))
		for i, t := range traces {
			traceIDs[i] = t.TraceID
		}

		var summaries []spanSummary
		r.db.WithContext(ctx).Raw(
			`SELECT trace_id, COUNT(*) as span_count, MIN(operation_name) as operation_name
			 FROM spans WHERE tenant_id = ? AND trace_id IN ? GROUP BY trace_id`, tenant, traceIDs,
		).Scan(&summaries)

		sm := make(map[string]spanSummary, len(summaries))
		for _, s := range summaries {
			sm[s.TraceID] = s
		}

		for i := range traces {
			s := sm[traces[i].TraceID]
			traces[i].SpanCount = s.SpanCount
			traces[i].DurationMs = float64(traces[i].Duration) / 1000.0
			if s.OperationName != "" {
				traces[i].Operation = s.OperationName
			} else {
				traces[i].Operation = "Unknown"
			}
		}
	}

	return &TracesResponse{
		Traces: traces,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	}, nil
}

// serviceMapSpanLimit caps the edge-pass span scan. A var (not const) so the
// row-limit warning path is testable without seeding 500k rows.
var serviceMapSpanLimit = 500_000

// serviceMapNodeRow receives the per-service GROUP BY aggregate used to build
// ServiceMapNode entries without loading span rows into Go.
type serviceMapNodeRow struct {
	ServiceName string
	SpanCount   int64
	AvgDuration float64
	ErrorCount  int64
}

// serviceMapSpanRow is the narrow projection scanned for the edge pass. It is
// deliberately NOT Span: AttributesJSON (CompressedText) zstd-decompresses
// per row in Scan(), which dominated the cost of the old full-row load.
type serviceMapSpanRow struct {
	SpanID       string
	ParentSpanID string
	ServiceName  string
	Duration     int64
	Status       string
	StartTime    time.Time
}

// GetServiceMapMetrics computes topology metrics from spans scoped to the
// tenant on ctx.
//
// Node stats come from a single portable GROUP BY aggregate so the database
// does the heavy lifting. Edge stats still need the parent→child resolution
// via span_id done in Go, but over the narrow serviceMapSpanRow projection so
// the compressed attributes column is never scanned. `duration * 1.0` keeps
// AVG in floating point on every dialect (SQL Server truncates AVG(bigint)).
func (r *Repository) GetServiceMapMetrics(ctx context.Context, start, end time.Time) (*ServiceMapMetrics, error) {
	tenant := TenantFromContext(ctx)

	nodeQuery := r.db.WithContext(ctx).Model(&Span{}).
		Select("service_name, COUNT(*) as span_count, AVG(duration * 1.0) as avg_duration, "+
			"SUM(CASE WHEN status = 'STATUS_CODE_ERROR' THEN 1 ELSE 0 END) as error_count").
		Where(sqlWhereTenantID, tenant).
		Where("service_name <> ''")
	if !start.IsZero() && !end.IsZero() {
		nodeQuery = nodeQuery.Where("start_time BETWEEN ? AND ?", start, end)
	}
	var nodeRows []serviceMapNodeRow
	if err := nodeQuery.Group("service_name").Scan(&nodeRows).Error; err != nil {
		return nil, fmt.Errorf("failed to aggregate service map nodes: %w", err)
	}

	nodes := make([]ServiceMapNode, 0, len(nodeRows))
	for _, nr := range nodeRows {
		nodes = append(nodes, ServiceMapNode{
			Name:        nr.ServiceName,
			TotalTraces: nr.SpanCount,
			ErrorCount:  nr.ErrorCount,
			// AVG(duration) is microseconds; convert to ms and round to 2dp.
			AvgLatencyMs: math.Round(nr.AvgDuration/1000.0*100) / 100,
		})
	}

	edgeQuery := r.db.WithContext(ctx).Model(&Span{}).
		Select("span_id, parent_span_id, service_name, duration, status, start_time").
		Where(sqlWhereTenantID, tenant)
	if !start.IsZero() && !end.IsZero() {
		edgeQuery = edgeQuery.Where("start_time BETWEEN ? AND ?", start, end)
	}
	var spans []serviceMapSpanRow
	if err := edgeQuery.Limit(serviceMapSpanLimit).Find(&spans).Error; err != nil {
		return nil, fmt.Errorf("failed to fetch spans: %w", err)
	}
	if len(spans) == serviceMapSpanLimit {
		slog.Warn("GetServiceMapMetrics: edge span query hit row limit, edge topology may be incomplete", "limit", serviceMapSpanLimit)
	}

	// Parent resolution map (span_id → service name) is built over ALL rows,
	// including empty service names, matching the old full-span map exactly.
	serviceBySpanID := make(map[string]string, len(spans))
	for _, s := range spans {
		serviceBySpanID[s.SpanID] = s.ServiceName
	}

	edgeStats := make(map[string]*ServiceMapEdge)
	for _, s := range spans {
		if s.ParentSpanID == "" || s.ParentSpanID == "0000000000000000" {
			continue
		}

		source, ok := serviceBySpanID[s.ParentSpanID]
		if !ok {
			continue
		}
		target := s.ServiceName

		if source == "" || target == "" || source == target {
			continue
		}

		key := source + "->" + target
		if _, ok := edgeStats[key]; !ok {
			edgeStats[key] = &ServiceMapEdge{Source: source, Target: target}
		}
		es := edgeStats[key]
		es.CallCount++
		es.AvgLatencyMs += float64(s.Duration)
	}

	edges := make([]ServiceMapEdge, 0, len(edgeStats))
	for _, es := range edgeStats {
		if es.CallCount > 0 {
			es.AvgLatencyMs = es.AvgLatencyMs / float64(es.CallCount) / 1000.0
			es.AvgLatencyMs = math.Round(es.AvgLatencyMs*100) / 100
		}
		edges = append(edges, *es)
	}

	return &ServiceMapMetrics{
		Nodes: nodes,
		Edges: edges,
	}, nil
}

// PurgeTraces deletes traces older than the given timestamp in a single statement.
// Uses Unscoped() for a hard DELETE (Trace has a soft-delete column that would
// otherwise leave rows present and block storage reclamation).
func (r *Repository) PurgeTraces(olderThan time.Time) (int64, error) {
	result := r.db.Unscoped().Where("timestamp < ?", olderThan).Delete(&Trace{})
	if result.Error != nil {
		return 0, fmt.Errorf("failed to purge traces: %w", result.Error)
	}
	slog.Info("Traces purged", "count", result.RowsAffected, "cutoff", olderThan)
	return result.RowsAffected, nil
}

// PurgeTracesBatched deletes traces (and their spans) in bounded chunks.
// On SQLite it falls through to a single-statement delete.
//
// Tenant scope: this is a SYSTEM-WIDE retention operation and intentionally
// does NOT filter by tenant. Rows are deleted across every tenant. Never
// expose this on a tenant-scoped API surface.
func (r *Repository) PurgeTracesBatched(ctx context.Context, olderThan time.Time, batchSize int, sleep time.Duration) (int64, error) {
	if batchSize <= 0 {
		batchSize = 10_000
	}

	// Delete traces older than cutoff, then sweep any spans whose trace_id is no longer
	// present. Correlating via trace existence alone races with concurrent ingest:
	// TraceServer.Export inserts spans and traces separately, so a span whose parent
	// trace row has not yet committed would look orphaned and be wrongly deleted.
	// Constrain the sweep to old spans (start_time < cutoff) so fresh in-flight spans
	// are never candidates. Clock-skewed historical spans under a still-present trace
	// are still protected by the trace-existence subquery.
	//
	// Both the trace purge and the orphan-span sweep run in bounded LIMIT batches
	// with a yield between batches for EVERY driver — including SQLite. The raw-SQL
	// DELETEs are hard deletes (they bypass GORM soft-delete), so the orphan sweep
	// still sees a consistent traces table. SQLite previously ran these two DELETEs
	// UNBATCHED, holding the single writer lock for the whole multi-GB purge and
	// stalling ingest into a 429 storm; batching releases the lock between chunks.

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		result := r.db.WithContext(ctx).Exec(
			"DELETE FROM traces WHERE id IN (SELECT id FROM traces WHERE timestamp < ? ORDER BY id LIMIT ?)",
			olderThan, batchSize,
		)
		if result.Error != nil {
			return total, fmt.Errorf("batched purge traces: %w", result.Error)
		}
		total += result.RowsAffected
		if result.RowsAffected < int64(batchSize) {
			break
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(sleep):
		}
	}

	// Sweep orphaned spans in batches. The NOT IN subquery is evaluated per batch, which is
	// O(spans × traces) worst case — acceptable because we bound the scan with LIMIT and the
	// trace set shrinks on each pass.
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		result := r.db.WithContext(ctx).Exec(
			"DELETE FROM spans WHERE id IN (SELECT id FROM spans WHERE start_time < ? AND trace_id NOT IN (SELECT trace_id FROM traces) ORDER BY id LIMIT ?)",
			olderThan, batchSize,
		)
		if result.Error != nil {
			return total, fmt.Errorf("sweep orphan spans: %w", result.Error)
		}
		if result.RowsAffected < int64(batchSize) {
			break
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(sleep):
		}
	}

	return total, nil
}
