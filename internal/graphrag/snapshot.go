package graphrag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// GraphSnapshot is a periodic snapshot of the service topology persisted to DB.
//
// TenantID scopes the row to the tenant slice it was captured from. The
// composite (tenant_id, created_at) index supports the
// "most recent snapshot at-or-before T for tenant X" lookup that
// GetGraphSnapshot runs on every read.
type GraphSnapshot struct {
	TenantID       string          `gorm:"size:64;default:'default';not null;index:idx_graph_snapshots_tenant_created,priority:1" json:"tenant_id"`
	ID             string          `gorm:"primaryKey;size:64" json:"id"`
	CreatedAt      time.Time       `gorm:"index:idx_graph_snapshots_tenant_created,priority:2" json:"created_at"`
	Nodes          json.RawMessage `gorm:"type:text" json:"nodes"`
	Edges          json.RawMessage `gorm:"type:text" json:"edges"`
	ServiceCount   int             `json:"service_count"`
	TotalCalls     int64           `json:"total_calls"`
	AvgHealthScore float64         `json:"avg_health_score"`
}

// TableName overrides GORM's default table name.
func (GraphSnapshot) TableName() string {
	return "graph_snapshots"
}

// snapshotNode is a lightweight node representation for snapshots.
type snapshotNode struct {
	ID          string  `json:"id"`
	Type        string  `json:"type"`
	Name        string  `json:"name"`
	HealthScore float64 `json:"health_score"`
	ErrorRate   float64 `json:"error_rate"`
	AvgLatency  float64 `json:"avg_latency_ms"`
}

// snapshotEdge is a lightweight edge representation for snapshots.
type snapshotEdge struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	Type      string  `json:"type"`
	Weight    float64 `json:"weight"`
	CallCount int64   `json:"call_count"`
	ErrorRate float64 `json:"error_rate"`
}

// takeSnapshot captures each tenant's current service topology and persists
// one row per tenant per tick. See the note on GraphSnapshot regarding the
// upcoming tenant_id column in Subtask B.
func (g *GraphRAG) takeSnapshot(ctx context.Context) {
	for tenant, stores := range g.snapshotTenants() {
		tctx := storage.WithTenantContext(ctx, tenant)
		g.takeSnapshotForTenant(tctx, tenant, stores)
	}
}

func (g *GraphRAG) takeSnapshotForTenant(_ context.Context, tenant string, stores *tenantStores) {
	services := stores.service.AllServices()
	edges := stores.service.AllEdges()

	if len(services) == 0 {
		return
	}

	var nodes []snapshotNode
	var totalCalls int64
	var totalHealth float64

	for _, svc := range services {
		nodes = append(nodes, snapshotNode{
			ID:          svc.ID,
			Type:        "service",
			Name:        svc.Name,
			HealthScore: svc.HealthScore,
			ErrorRate:   svc.ErrorRate,
			AvgLatency:  svc.AvgLatency,
		})
		totalCalls += svc.CallCount
		totalHealth += svc.HealthScore
	}

	// Also include operations for this tenant.
	stores.service.mu.RLock()
	for _, op := range stores.service.Operations {
		nodes = append(nodes, snapshotNode{
			ID:          op.ID,
			Type:        "operation",
			Name:        op.Operation,
			HealthScore: op.HealthScore,
			ErrorRate:   op.ErrorRate,
			AvgLatency:  op.AvgLatency,
		})
	}
	stores.service.mu.RUnlock()

	var snapEdges []snapshotEdge
	for _, e := range edges {
		snapEdges = append(snapEdges, snapshotEdge{
			From:      e.FromID,
			To:        e.ToID,
			Type:      string(e.Type),
			Weight:    e.Weight,
			CallCount: e.CallCount,
			ErrorRate: e.ErrorRate,
		})
	}

	nodesJSON, _ := json.Marshal(nodes)
	edgesJSON, _ := json.Marshal(snapEdges)

	snap := GraphSnapshot{
		TenantID:       tenant,
		ID:             fmt.Sprintf("snap_%s_%d", tenant, time.Now().UnixNano()),
		CreatedAt:      time.Now(),
		Nodes:          nodesJSON,
		Edges:          edgesJSON,
		ServiceCount:   len(services),
		TotalCalls:     totalCalls,
		AvgHealthScore: totalHealth / float64(len(services)),
	}

	if g.repo == nil || g.repo.DB() == nil {
		return
	}
	if err := g.repo.DB().Create(&snap).Error; err != nil {
		slog.Error("Failed to persist graph snapshot", "tenant", tenant, "error", err)
		return
	}

	slog.Debug("Graph snapshot persisted",
		"tenant", tenant,
		"services", len(services),
		"edges", len(snapEdges),
	)
}

// maxSnapshotRows is a row-count backstop on `graph_snapshots` to prevent
// unbounded disk growth when the write rate outruns the 7-day age prune.
// Steady state at 15-min cadence × 100 tenants is ~67k rows/week, so 100k
// gives ~50% headroom — high enough to never trigger under normal operation,
// low enough to bound disk if a misconfig or tenant explosion runs the
// snapshotter hot.
const maxSnapshotRows = 100_000

// pruneOldSnapshots removes snapshots older than 7 days, then enforces a
// row-count backstop in case the by-age prune isn't keeping up.
func (g *GraphRAG) pruneOldSnapshots() {
	if g.repo == nil || g.repo.DB() == nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -7)
	result := g.repo.DB().Where("created_at < ?", cutoff).Delete(&GraphSnapshot{})
	if result.Error != nil {
		slog.Error("Failed to prune old snapshots", "error", result.Error)
	} else if result.RowsAffected > 0 {
		slog.Info("Pruned old graph snapshots", "count", result.RowsAffected)
	}

	var count int64
	if err := g.repo.DB().Model(&GraphSnapshot{}).Count(&count).Error; err != nil {
		slog.Error("Failed to count snapshots for row-cap prune", "error", err)
		return
	}
	if count <= maxSnapshotRows {
		return
	}
	excess := count - maxSnapshotRows
	// Subquery selects the N oldest IDs, then deletes that set. Portable
	// across SQLite and Postgres; avoids a multi-statement transaction.
	sub := g.repo.DB().Model(&GraphSnapshot{}).Select("id").Order("created_at ASC").Limit(int(excess))
	if err := g.repo.DB().Where("id IN (?)", sub).Delete(&GraphSnapshot{}).Error; err != nil {
		slog.Error("Failed to row-cap prune snapshots", "error", err)
		return
	}
	slog.Warn("graphrag: row-cap pruned snapshots (write rate exceeded by-age prune)",
		"deleted", excess,
		"cap", maxSnapshotRows,
	)
}

// GetGraphSnapshot retrieves the snapshot closest to the requested time,
// scoped to the tenant carried by ctx. The composite (tenant_id, created_at)
// index supports the descending lookup.
func (g *GraphRAG) GetGraphSnapshot(ctx context.Context, at time.Time) (*GraphSnapshot, error) {
	tenant := storage.TenantFromContext(ctx)
	var snap GraphSnapshot
	err := g.repo.DB().
		Where("tenant_id = ? AND created_at <= ?", tenant, at).
		Order("created_at DESC").
		First(&snap).Error
	if err != nil {
		return nil, err
	}
	return &snap, nil
}
