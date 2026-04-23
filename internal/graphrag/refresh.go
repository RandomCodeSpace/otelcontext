package graphrag

import (
	"context"
	"log/slog"
	"time"
)

// refreshLoop periodically rebuilds/merges from DB and prunes stale data.
func (g *GraphRAG) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(g.refreshEvery)
	defer ticker.Stop()

	// Initial rebuild on startup
	g.rebuildFromDB()

	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.rebuildFromDB()
			pruned := g.TraceStore.Prune()
			if pruned > 0 {
				slog.Debug("GraphRAG pruned expired traces/spans", "count", pruned)
			}
			g.pruneOldAnomalies()
		}
	}
}

// snapshotLoop takes periodic snapshots and prunes old ones.
func (g *GraphRAG) snapshotLoop(ctx context.Context) {
	ticker := time.NewTicker(g.snapshotEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.takeSnapshot()
			g.pruneOldSnapshots()
			g.persistDrainTemplates()
		}
	}
}

// persistDrainTemplates saves the current Drain template set to the DB.
// Called on each snapshot tick so a restart recovers the learned templates.
func (g *GraphRAG) persistDrainTemplates() {
	if g.repo == nil || g.repo.DB() == nil || g.drain == nil {
		return
	}
	tpls := g.drain.Templates()
	if len(tpls) == 0 {
		return
	}
	if err := SaveDrainTemplates(g.repo.DB(), tpls); err != nil {
		slog.Error("Failed to persist drain templates", "error", err)
		return
	}
	slog.Debug("Drain templates persisted", "count", len(tpls))
}

// rebuildFromDB loads recent span data from the DB and merges into the graph.
// This catches data from before callbacks started (e.g., restart recovery).
func (g *GraphRAG) rebuildFromDB() {
	since := time.Now().Add(-1 * time.Hour)

	// Load recent spans
	type spanRow struct {
		SpanID        string
		ParentSpanID  string
		ServiceName   string
		OperationName string
		Duration      int64 // microseconds
		TraceID       string
		Status        string
		StartTime     time.Time
	}

	var rows []spanRow
	err := g.repo.DB().
		Table("spans").
		Select("span_id, parent_span_id, service_name, operation_name, duration, trace_id, status, start_time").
		Where("start_time > ?", since).
		Order("start_time ASC").
		Limit(50000).
		Find(&rows).Error
	if err != nil {
		slog.Error("GraphRAG: failed to rebuild from DB", "error", err)
		return
	}

	if len(rows) == 0 {
		return
	}

	// Build spanID → service map for edge resolution
	spanService := make(map[string]string, len(rows))
	for _, r := range rows {
		spanService[r.SpanID] = r.ServiceName
	}

	for _, r := range rows {
		durationMs := float64(r.Duration) / 1000.0
		isError := r.Status == "STATUS_CODE_ERROR"

		g.ServiceStore.UpsertService(r.ServiceName, durationMs, isError, r.StartTime)
		if r.OperationName != "" {
			g.ServiceStore.UpsertOperation(r.ServiceName, r.OperationName, durationMs, isError, r.StartTime)
		}

		// Cross-service edges
		if r.ParentSpanID != "" {
			if parentSvc, ok := spanService[r.ParentSpanID]; ok && parentSvc != r.ServiceName {
				g.ServiceStore.UpsertCallEdge(parentSvc, r.ServiceName, durationMs, isError, r.StartTime)
			}
		}
	}

	slog.Debug("GraphRAG rebuilt from DB", "spans", len(rows), "services", len(g.ServiceStore.Services))
}
