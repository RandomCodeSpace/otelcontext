package storage

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/latency"
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
	Name              string              `json:"name"`
	TotalTraces       int64               `json:"total_traces"`
	ErrorCount        int64               `json:"error_count"`
	AvgLatencyMs      float64             `json:"avg_latency_ms"`
	P99LatencyMs      float64             `json:"p99_latency_ms,omitempty"`
	LatencyProvenance *latency.Provenance `json:"latency_provenance,omitempty"`

	// Additive host projection (#288), stamped by the topology provider;
	// never read from or written to the database.
	Kind      string   `json:"kind,omitempty" gorm:"-"`
	HostCount int      `json:"host_count,omitempty" gorm:"-"`
	Hosts     []string `json:"hosts,omitempty" gorm:"-"`
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

// traceConflictColumns is the conflict target for the traces uniqueIndex
// idx_traces_tenant_trace_id on (tenant_id, trace_id).
var traceConflictColumns = []clause.Column{{Name: "tenant_id"}, {Name: "trace_id"}}

// createTracesIdempotent runs the conflict-tolerant trace insert against an
// arbitrary *gorm.DB so the same logic is reused inside a transaction by
// BatchCreateAll.
//
// Trace status is UPGRADE-ONLY. A trace row exists once per (tenant, trace);
// whichever span arrives first seeds its timestamp/duration/service, and those
// are never rewritten. Status is the exception: an incoming STATUS_CODE_ERROR
// row must be able to flip an already-persisted UNSET/OK row to ERROR, because
// the root span (UNSET) frequently lands before the child span that failed.
// The reverse is forbidden — a persisted ERROR is never downgraded.
//
// This is expressed without a dialect-specific CASE/WHERE guard, which neither
// MySQL's ON DUPLICATE KEY UPDATE nor SQL Server's MERGE translation supports
// portably: the batch is split by status. Non-error rows keep the existing
// insert-or-ignore behaviour (so they cannot clobber an ERROR row), and error
// rows use DoUpdates{status: STATUS_CODE_ERROR} — a conflicting row is only
// ever assigned ERROR, so the update direction is upgrade-only by construction.
// Non-error rows are written first so an ERROR row later in the same batch wins.
func createTracesIdempotent(db *gorm.DB, driver string, traces []Trace) error {
	if len(traces) == 0 {
		return nil
	}
	healthy, errored := splitTracesByStatus(traces)
	if len(healthy) > 0 {
		if err := insertTracesIgnoringConflicts(db, driver, healthy); err != nil {
			return err
		}
	}
	if len(errored) > 0 {
		// DoUpdates carries the literal rather than a column reference so the
		// same clause works on every dialect (no excluded./VALUES()/MERGE alias).
		if err := db.Clauses(clause.OnConflict{
			Columns:   traceConflictColumns,
			DoUpdates: clause.Assignments(map[string]any{"status": StatusCodeError}),
		}).Create(&errored).Error; err != nil {
			return err
		}
	}
	return updateTraceTruncation(db, traces)
}

// updateTraceTruncation stamps the exemplar truncation metadata (#163) onto
// rows the inserts above may have left untouched.
//
// The insert paths are first-writer-wins on everything but status, so a trace
// whose truncation only becomes known on a later batch would otherwise keep the
// first batch's NULL forever — and a truncated trace is by definition one with
// enough spans to arrive across several batches. Truncation counts are
// cumulative and monotonic, so last-write-wins is the correct direction here.
//
// Cost is one UPDATE per truncated trace, which the exemplar budgets bound to a
// handful per service per window. Rows without a truncation claim (every
// legacy/shadow-mode row, and every exemplar retained whole) skip this entirely.
func updateTraceTruncation(db *gorm.DB, traces []Trace) error {
	for i := range traces {
		t := &traces[i]
		if t.Truncated == nil {
			continue
		}
		updates := map[string]any{"truncated": *t.Truncated}
		if t.RetainedSpanCount != nil {
			updates["retained_span_count"] = *t.RetainedSpanCount
		}
		if t.ObservedSpanCount != nil {
			updates["observed_span_count"] = *t.ObservedSpanCount
		}
		if err := db.Model(&Trace{}).
			Where("tenant_id = ? AND trace_id = ?", t.TenantID, t.TraceID).
			Updates(updates).Error; err != nil {
			return err
		}
	}
	return nil
}

// insertTracesIgnoringConflicts is the original first-writer-wins insert:
// MySQL takes INSERT IGNORE; SQLite/Postgres/SQL Server take
// ON CONFLICT DO NOTHING via the gorm clause helper.
func insertTracesIgnoringConflicts(db *gorm.DB, driver string, traces []Trace) error {
	if strings.ToLower(driver) == "mysql" {
		return db.Clauses(clause.Insert{Modifier: "IGNORE"}).Create(&traces).Error
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&traces).Error
}

// splitTracesByStatus partitions a batch into non-error and error rows,
// collapsing duplicates on (tenant_id, trace_id) with error-wins semantics.
// The in-batch dedup is required as well as useful: Postgres rejects an
// INSERT ... ON CONFLICT DO UPDATE that would touch the same row twice in one
// statement.
func splitTracesByStatus(traces []Trace) (healthy, errored []Trace) {
	idx := make(map[string]int, len(traces))
	deduped := make([]Trace, 0, len(traces))
	for _, t := range traces {
		key := t.TenantID + "\x00" + t.TraceID
		if i, seen := idx[key]; seen {
			if t.Status == StatusCodeError {
				deduped[i].Status = StatusCodeError
			}
			continue
		}
		idx[key] = len(deduped)
		deduped = append(deduped, t)
	}
	for _, t := range deduped {
		if t.Status == StatusCodeError {
			errored = append(errored, t)
		} else {
			healthy = append(healthy, t)
		}
	}
	return healthy, errored
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

// CreateTrace inserts a new trace, skipping if it already exists — except that
// an incoming STATUS_CODE_ERROR upgrades an existing non-error row's status
// (upgrade-only; see createTracesIdempotent for the full rationale).
// Uniqueness is per idx_traces_tenant_trace_id (tenant_id, trace_id), so the
// same trace_id across tenants is allowed.
func (r *Repository) CreateTrace(trace Trace) error {
	return createTracesIdempotent(r.db, r.driver, []Trace{trace})
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
// per row in Scan(), which dominated the cost of the old full-row load. It
// carries exactly the four columns the edge pass reads: the old projection
// also pulled status and start_time, and parsing a datetime string per row
// for a column nothing consumed was most of what remained (#290).
type serviceMapSpanRow struct {
	SpanID       string
	ParentSpanID string
	ServiceName  string
	Duration     int64
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

	// The edge pass streams the rows through database/sql directly: at a few
	// hundred thousand spans GORM's reflective row scan was a third of the
	// call. The nullable columns are coalesced in SQL so a plain string or
	// int64 destination is always valid.
	edgeQuery := r.db.WithContext(ctx).Model(&Span{}).
		Select("span_id, COALESCE(parent_span_id, ''), COALESCE(service_name, ''), COALESCE(duration, 0)").
		Where(sqlWhereTenantID, tenant)
	if !start.IsZero() && !end.IsZero() {
		edgeQuery = edgeQuery.Where("start_time BETWEEN ? AND ?", start, end)
	}
	rows, err := edgeQuery.Limit(serviceMapSpanLimit).Rows()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch spans: %w", err)
	}
	spans := make([]serviceMapSpanRow, 0, 1024)
	for rows.Next() {
		var s serviceMapSpanRow
		if err := rows.Scan(&s.SpanID, &s.ParentSpanID, &s.ServiceName, &s.Duration); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("failed to scan span: %w", err)
		}
		spans = append(spans, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("failed to fetch spans: %w", err)
	}
	_ = rows.Close()
	if len(spans) == serviceMapSpanLimit {
		slog.Warn("GetServiceMapMetrics: edge span query hit row limit, edge topology may be incomplete", "limit", serviceMapSpanLimit)
	}

	// Exact per-service p99 (#291), only while the range fits the row bound
	// the edge pass scans; above it every node keeps the average-multiplier
	// estimate, labelled as such. The durations are already in hand — the
	// edge pass reads every span in range — so the nearest rank is picked
	// from them here instead of by a second window-function query over the
	// same rows, which sorted the whole range a second time and on SQLite
	// cost more than the rest of the call put together (#290).
	var rangeSpans int64
	for _, nr := range nodeRows {
		rangeSpans += nr.SpanCount
	}
	p99ByService := map[string]int64{}
	if rangeSpans > 0 && rangeSpans <= int64(serviceMapSpanLimit) && len(spans) < serviceMapSpanLimit {
		p99ByService = serviceP99Durations(spans)
	}

	nodes := make([]ServiceMapNode, 0, len(nodeRows))
	for _, nr := range nodeRows {
		avgLatencyMs := math.Round(nr.AvgDuration/1000.0*100) / 100
		sampleCount := uint64(nr.SpanCount) // #nosec G115 -- grouped database counts cannot be negative.
		node := ServiceMapNode{
			Name:        nr.ServiceName,
			TotalTraces: nr.SpanCount,
			ErrorCount:  nr.ErrorCount,
			// AVG(duration) is microseconds; convert to ms and round to 2dp.
			AvgLatencyMs: avgLatencyMs,
		}
		if p99, ok := p99ByService[nr.ServiceName]; ok {
			node.P99LatencyMs = math.Round(float64(p99)/1000.0*100) / 100
			node.LatencyProvenance = &latency.Provenance{P99: &latency.Percentile{
				Status:      latency.StatusMeasured,
				Method:      latency.MethodOrderedRank,
				SampleCount: sampleCount,
				LowSample:   sampleCount < latency.LowSampleThreshold,
			}}
		} else {
			node.P99LatencyMs = avgLatencyMs * 2.5
			node.LatencyProvenance = &latency.Provenance{P99: &latency.Percentile{
				Status:         latency.StatusEstimated,
				Method:         latency.MethodAverageMultiplier,
				SampleCount:    sampleCount,
				LowSample:      sampleCount < latency.LowSampleThreshold,
				EstimateFactor: 2.5,
			}}
		}
		nodes = append(nodes, node)
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

// serviceP99Durations returns the nearest-rank p99 duration (microseconds)
// per service over the edge pass's rows: rank ceil(0.99·n) within each
// service's ascending durations, the same convention p99DurationForQuery
// applies to the dashboard. Spans with no service name carry no node and are
// skipped, matching the node aggregate's non-empty service-name predicate.
func serviceP99Durations(spans []serviceMapSpanRow) map[string]int64 {
	byService := make(map[string][]int64)
	for _, s := range spans {
		if s.ServiceName == "" {
			continue
		}
		byService[s.ServiceName] = append(byService[s.ServiceName], s.Duration)
	}
	out := make(map[string]int64, len(byService))
	for service, durations := range byService {
		slices.Sort(durations)
		n := len(durations)
		rank := (n*99 + 99) / 100 // ceil(0.99·n) in integer arithmetic
		out[service] = durations[rank-1]
	}
	return out
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
			batchedDeleteSQL(r.driver, "traces", "timestamp < ?"),
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
			batchedDeleteSQL(r.driver, "spans", "start_time < ? AND trace_id NOT IN (SELECT trace_id FROM traces)"),
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
