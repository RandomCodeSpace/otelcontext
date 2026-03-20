package graphrag

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// GraphSnapshot is a periodic snapshot of the service topology persisted to DB.
type GraphSnapshot struct {
	ID             string          `gorm:"primaryKey;size:64" json:"id"`
	CreatedAt      time.Time       `json:"created_at"`
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

// takeSnapshot captures the current service topology and persists it.
func (g *GraphRAG) takeSnapshot() {
	services := g.ServiceStore.AllServices()
	edges := g.ServiceStore.AllEdges()

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

	// Also include operations
	g.ServiceStore.mu.RLock()
	for _, op := range g.ServiceStore.Operations {
		nodes = append(nodes, snapshotNode{
			ID:          op.ID,
			Type:        "operation",
			Name:        op.Operation,
			HealthScore: op.HealthScore,
			ErrorRate:   op.ErrorRate,
			AvgLatency:  op.AvgLatency,
		})
	}
	g.ServiceStore.mu.RUnlock()

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
		ID:             fmt.Sprintf("snap_%d", time.Now().UnixNano()),
		CreatedAt:      time.Now(),
		Nodes:          nodesJSON,
		Edges:          edgesJSON,
		ServiceCount:   len(services),
		TotalCalls:     totalCalls,
		AvgHealthScore: totalHealth / float64(len(services)),
	}

	if err := g.repo.DB().Create(&snap).Error; err != nil {
		slog.Error("Failed to persist graph snapshot", "error", err)
		return
	}

	slog.Debug("Graph snapshot persisted", "services", len(services), "edges", len(snapEdges))
}

// pruneOldSnapshots removes snapshots older than 7 days.
func (g *GraphRAG) pruneOldSnapshots() {
	cutoff := time.Now().AddDate(0, 0, -7)
	result := g.repo.DB().Where("created_at < ?", cutoff).Delete(&GraphSnapshot{})
	if result.Error != nil {
		slog.Error("Failed to prune old snapshots", "error", result.Error)
	} else if result.RowsAffected > 0 {
		slog.Info("Pruned old graph snapshots", "count", result.RowsAffected)
	}
}

// GetGraphSnapshot retrieves the snapshot closest to the requested time.
func (g *GraphRAG) GetGraphSnapshot(at time.Time) (*GraphSnapshot, error) {
	var snap GraphSnapshot
	err := g.repo.DB().
		Where("created_at <= ?", at).
		Order("created_at DESC").
		First(&snap).Error
	if err != nil {
		return nil, err
	}
	return &snap, nil
}
