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

// BatchCreateSpans inserts multiple spans in batches.
func (r *Repository) BatchCreateSpans(spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	if err := r.db.CreateInBatches(spans, 500).Error; err != nil {
		return fmt.Errorf("failed to batch create spans: %w", err)
	}
	return nil
}

// BatchCreateTraces inserts traces, skipping duplicates.
func (r *Repository) BatchCreateTraces(traces []Trace) error {
	if len(traces) == 0 {
		return nil
	}
	if strings.ToLower(r.driver) == "mysql" {
		return r.db.Clauses(clause.Insert{Modifier: "IGNORE"}).Create(&traces).Error
	}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&traces).Error
}

// CreateTrace inserts a new trace, skipping if it already exists.
func (r *Repository) CreateTrace(trace Trace) error {
	if strings.ToLower(r.driver) == "mysql" {
		return r.db.Clauses(clause.Insert{Modifier: "IGNORE"}).Create(&trace).Error
	}
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&trace).Error
}

// GetTrace returns a trace by ID with its spans and logs, scoped to the tenant on ctx.
// The Preloaded Spans and Logs are additionally filtered so a trace ID collision
// across tenants cannot leak cross-tenant children.
func (r *Repository) GetTrace(ctx context.Context, traceID string) (*Trace, error) {
	tenant := TenantFromContext(ctx)
	var trace Trace
	if err := r.db.WithContext(ctx).
		Preload("Spans", "tenant_id = ?", tenant).
		Preload("Logs", "tenant_id = ?", tenant).
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

	base := r.db.WithContext(ctx).Model(&Trace{}).Where("tenant_id = ?", tenant)

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

const serviceMapSpanLimit = 500_000

// GetServiceMapMetrics computes topology metrics from spans scoped to the
// tenant on ctx.
func (r *Repository) GetServiceMapMetrics(ctx context.Context, start, end time.Time) (*ServiceMapMetrics, error) {
	tenant := TenantFromContext(ctx)
	var spans []Span
	query := r.db.WithContext(ctx).Model(&Span{}).Where("tenant_id = ?", tenant)

	if !start.IsZero() && !end.IsZero() {
		query = query.Where("start_time BETWEEN ? AND ?", start, end)
	}

	if err := query.Limit(serviceMapSpanLimit).Find(&spans).Error; err != nil {
		return nil, fmt.Errorf("failed to fetch spans: %w", err)
	}
	if len(spans) == serviceMapSpanLimit {
		slog.Warn("GetServiceMapMetrics: span query hit row limit, topology may be incomplete", "limit", serviceMapSpanLimit)
	}

	spanMap := make(map[string]Span)
	nodeStats := make(map[string]*ServiceMapNode)
	edgeStats := make(map[string]*ServiceMapEdge)

	for _, s := range spans {
		spanMap[s.SpanID] = s

		if s.ServiceName == "" {
			continue
		}

		if _, ok := nodeStats[s.ServiceName]; !ok {
			nodeStats[s.ServiceName] = &ServiceMapNode{Name: s.ServiceName}
		}
		ns := nodeStats[s.ServiceName]
		ns.TotalTraces++
		ns.AvgLatencyMs += float64(s.Duration)
	}

	nodes := make([]ServiceMapNode, 0)
	for _, ns := range nodeStats {
		if ns.TotalTraces > 0 {
			ns.AvgLatencyMs = ns.AvgLatencyMs / float64(ns.TotalTraces) / 1000.0
			ns.AvgLatencyMs = math.Round(ns.AvgLatencyMs*100) / 100
		}
		nodes = append(nodes, *ns)
	}

	for _, s := range spans {
		if s.ParentSpanID == "" || s.ParentSpanID == "0000000000000000" {
			continue
		}

		parent, ok := spanMap[s.ParentSpanID]
		if !ok {
			continue
		}

		source := parent.ServiceName
		target := s.ServiceName

		if source == "" || target == "" || source == target {
			continue
		}

		key := fmt.Sprintf("%s->%s", source, target)
		if _, ok := edgeStats[key]; !ok {
			edgeStats[key] = &ServiceMapEdge{Source: source, Target: target}
		}
		es := edgeStats[key]
		es.CallCount++
		es.AvgLatencyMs += float64(s.Duration)
	}

	edges := make([]ServiceMapEdge, 0)
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
	driver := strings.ToLower(r.driver)

	// Delete traces older than cutoff, then sweep any spans whose trace_id is no longer
	// present. Correlating via trace existence alone races with concurrent ingest:
	// TraceServer.Export inserts spans and traces separately, so a span whose parent
	// trace row has not yet committed would look orphaned and be wrongly deleted.
	// Constrain the sweep to old spans (start_time < cutoff) so fresh in-flight spans
	// are never candidates. Clock-skewed historical spans under a still-present trace
	// are still protected by the trace-existence subquery.
	deleteOrphanSpansSQL := "DELETE FROM spans WHERE start_time < ? AND trace_id NOT IN (SELECT trace_id FROM traces)"

	if driver == "sqlite" || driver == "" {
		// Unscoped() forces a hard DELETE so the orphan-span sweep below sees a
		// consistent traces table. Retention must reclaim disk, not soft-delete.
		result := r.db.WithContext(ctx).Unscoped().Where("timestamp < ?", olderThan).Delete(&Trace{})
		if result.Error != nil {
			return result.RowsAffected, result.Error
		}
		if err := r.db.WithContext(ctx).Exec(deleteOrphanSpansSQL, olderThan).Error; err != nil {
			return result.RowsAffected, fmt.Errorf("sweep orphan spans: %w", err)
		}
		return result.RowsAffected, nil
	}

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
