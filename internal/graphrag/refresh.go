package graphrag

import (
	"context"
	"log/slog"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

const (
	// signalRetention bounds MetricNodes and signal-store edges: anything
	// not refreshed within this window is swept on the refresh tick.
	signalRetention = 24 * time.Hour
	// maxMetricsPerTenant caps each tenant's SignalStore metric map; past
	// the cap the oldest-LastSeen series are evicted first.
	maxMetricsPerTenant = 2000
)

// refreshLoop periodically rebuilds/merges from DB and prunes stale data.
// Work is sharded per tenant: on each tick we snapshot the coordinator's
// tenant map, then rebuild and prune each slice under its own lock. Tenants
// are discovered from the spans table on first rebuild so historical data
// from tenants that have not yet ingested via callbacks is still loaded.
func (g *GraphRAG) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(g.refreshEvery)
	defer ticker.Stop()

	// Initial rebuild on startup.
	g.rebuildAllTenantsFromDB(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.rebuildAllTenantsFromDB(ctx)
			pruned := 0
			prunedMetrics := 0
			signalCutoff := time.Now().Add(-signalRetention)
			for _, stores := range g.snapshotTenants() {
				pruned += stores.traces.Prune()
				prunedMetrics += stores.signals.Prune(signalCutoff, maxMetricsPerTenant)
			}
			if pruned > 0 {
				slog.Debug("GraphRAG pruned expired traces/spans", "count", pruned)
			}
			if prunedMetrics > 0 {
				slog.Debug("GraphRAG pruned stale/over-cap metrics", "count", prunedMetrics)
			}
			g.pruneOldAnomalies()
			if evicted := g.evictIdleTenants(); evicted > 0 {
				slog.Info("GraphRAG evicted idle tenant stores", "count", evicted)
			}
			// Bound the investigation cooldown map. The 10m cutoff is 2×
			// the cooldown window (5m) — it retains entries through the
			// active suppression plus a grace period. This assumes the
			// refresh tick runs at least every 10 minutes; if RefreshEvery
			// grows larger, raise the cutoff in lockstep, otherwise a stuck
			// service could bypass the cooldown between prunes.
			if g.invCooldown != nil {
				g.invCooldown.prune(time.Now().Add(-10 * time.Minute))
			}
		}
	}
}

// evictIdleTenants drops tenant store slices whose lastAccess is older than
// tenantIdleTTL (GRAPHRAG_TENANT_IDLE_TTL, default 24h). The default tenant
// is never evicted — single-tenant installs route everything through it.
// Eviction is self-healing: a tenant that is actually active reappears
// within one refresh tick (rebuildAllTenantsFromDB re-discovers it from
// recent spans) or instantly on the next ingest event / query via
// storesForTenant. Returns the number of slices evicted.
func (g *GraphRAG) evictIdleTenants() int {
	if g.tenantIdleTTL <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-g.tenantIdleTTL).UnixNano()
	g.tenantsMu.Lock()
	evicted := 0
	for tenant, st := range g.tenants {
		if tenant == storage.DefaultTenantID {
			continue
		}
		if st.lastAccess.Load() < cutoff {
			delete(g.tenants, tenant)
			evicted++
		}
	}
	g.tenantsMu.Unlock()
	if evicted > 0 {
		g.tenantsEvicted.Add(int64(evicted))
		if g.metrics != nil && g.metrics.GraphRAGTenantsEvictedTotal != nil {
			g.metrics.GraphRAGTenantsEvictedTotal.Add(float64(evicted))
		}
	}
	return evicted
}

// snapshotLoop persists Drain templates on the configured cadence so a
// restart recovers the learned templates instead of rebuilding from scratch.
//
// Historically this loop also captured a periodic GraphSnapshot row into
// the `graph_snapshots` table and pruned aged-out snapshots; both were
// removed on 2026-05-24 alongside the get_graph_snapshot MCP tool. The
// `snapshotLoop` / `snapshotEvery` names are retained for wiring stability
// — callers still tune the persistence cadence via `Config.SnapshotEvery`.
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
	if err := SaveDrainTemplates(g.repo.DB(), storage.DefaultTenantID, tpls); err != nil {
		slog.Error("Failed to persist drain templates", "error", err)
		return
	}
	slog.Debug("Drain templates persisted", "count", len(tpls))
}

// rebuildAllTenantsFromDB rebuilds each known tenant's in-memory service
// topology from the spans table. Tenants are the union of already-present
// coordinator slices and the distinct tenant_id values observed in recent
// spans — this catches historical tenants that have not yet ingested via
// live callbacks since startup.
func (g *GraphRAG) rebuildAllTenantsFromDB(ctx context.Context) {
	if g.repo == nil || g.repo.DB() == nil {
		return
	}

	since := time.Now().Add(-1 * time.Hour)

	// Discover tenants that have recent spans. Missing tenant_id rows fall
	// back to DefaultTenantID so pre-multi-tenant data still rebuilds.
	var tenantIDs []string
	if err := g.repo.DB().
		Table("spans").
		Where("start_time > ?", since).
		Distinct("tenant_id").
		Pluck("tenant_id", &tenantIDs).Error; err != nil {
		slog.Error("GraphRAG: failed to enumerate tenants for rebuild", "error", err)
		return
	}

	seen := make(map[string]bool, len(tenantIDs))
	for _, t := range tenantIDs {
		if t == "" {
			t = storage.DefaultTenantID
		}
		seen[t] = true
	}
	// Always include tenants the coordinator already knows about so we refresh
	// live-ingested tenants even when no DB rows yet carry their ID.
	for t := range g.snapshotTenants() {
		seen[t] = true
	}

	for tenant := range seen {
		tctx := storage.WithTenantContext(ctx, tenant)
		g.rebuildFromDBForTenant(tctx, tenant, since)
	}
}

// rebuildFromDBForTenant loads recent span data for a single tenant and
// merges it into that tenant's slice of the graph. Catches data from before
// callbacks started (e.g., restart recovery).
func (g *GraphRAG) rebuildFromDBForTenant(_ context.Context, tenant string, since time.Time) {
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

	// NoTouch: a 60s bookkeeping rebuild must not refresh the idle-eviction
	// clock, or dormant tenants would never reach GRAPHRAG_TENANT_IDLE_TTL.
	stores := g.tenantStoresNoTouch(tenant)

	var rows []spanRow
	err := g.repo.DB().
		Table("spans").
		Select("span_id, parent_span_id, service_name, operation_name, duration, trace_id, status, start_time").
		Where("start_time > ? AND tenant_id = ?", since, tenant).
		Order("start_time ASC").
		Limit(50000).
		Find(&rows).Error
	if err != nil {
		slog.Error("GraphRAG: failed to rebuild from DB", "tenant", tenant, "error", err)
		return
	}

	if len(rows) == 0 {
		return
	}

	// Build spanID → service map for edge resolution.
	spanService := make(map[string]string, len(rows))
	for _, r := range rows {
		spanService[r.SpanID] = r.ServiceName
	}

	for _, r := range rows {
		durationMs := float64(r.Duration) / 1000.0
		isError := r.Status == "STATUS_CODE_ERROR"

		stores.service.UpsertService(r.ServiceName, durationMs, isError, r.StartTime)
		if r.OperationName != "" {
			stores.service.UpsertOperation(r.ServiceName, r.OperationName, durationMs, isError, r.StartTime)
		}

		// Cross-service edges.
		if r.ParentSpanID != "" {
			if parentSvc, ok := spanService[r.ParentSpanID]; ok && parentSvc != r.ServiceName {
				stores.service.UpsertCallEdge(parentSvc, r.ServiceName, durationMs, isError, r.StartTime)
			}
		}
	}

	slog.Debug("GraphRAG rebuilt from DB",
		"tenant", tenant,
		"spans", len(rows),
		"services", len(stores.service.Services),
	)
}
