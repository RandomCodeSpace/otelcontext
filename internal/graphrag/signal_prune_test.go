package graphrag

import (
	"fmt"
	"testing"
	"time"
)

// TestSignalStore_Prune_DropsStaleMetricsKeepsFresh proves the TTL half of
// the SignalStore bound: metrics idle past the cutoff are removed together
// with their MEASURED_BY edge, while a fresh metric and its edge survive.
func TestSignalStore_Prune_DropsStaleMetricsKeepsFresh(t *testing.T) {
	ss := newSignalStore()
	now := time.Now()
	stale := now.Add(-48 * time.Hour)

	ss.UpsertMetric("cpu", "old-svc", 1.0, stale)
	ss.UpsertMetric("cpu", "new-svc", 1.0, now)

	if got := ss.Prune(now.Add(-24*time.Hour), 0); got != 1 {
		t.Fatalf("Prune removed %d metrics, want 1", got)
	}
	if _, ok := ss.Metrics["cpu|old-svc"]; ok {
		t.Errorf("stale metric survived Prune")
	}
	if _, ok := ss.Metrics["cpu|new-svc"]; !ok {
		t.Errorf("fresh metric was pruned")
	}
	if _, ok := ss.Edges[edgeKey(EdgeMeasuredBy, "cpu|old-svc", "old-svc")]; ok {
		t.Errorf("stale metric's MEASURED_BY edge survived Prune")
	}
	if _, ok := ss.Edges[edgeKey(EdgeMeasuredBy, "cpu|new-svc", "new-svc")]; !ok {
		t.Errorf("fresh metric's MEASURED_BY edge was swept")
	}
}

// TestSignalStore_Prune_CapEvictsOldestFirst proves the cap half: when the
// map still exceeds maxMetrics after the TTL pass, the oldest-LastSeen
// entries go first and their edges go with them.
func TestSignalStore_Prune_CapEvictsOldestFirst(t *testing.T) {
	ss := newSignalStore()
	now := time.Now()

	// 5 fresh metrics with strictly ascending LastSeen.
	for i := 0; i < 5; i++ {
		ss.UpsertMetric(fmt.Sprintf("m%d", i), "svc", 1.0, now.Add(time.Duration(i)*time.Minute))
	}

	if got := ss.Prune(now.Add(-24*time.Hour), 2); got != 3 {
		t.Fatalf("Prune removed %d metrics, want 3", got)
	}
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("m%d|svc", i)
		if _, ok := ss.Metrics[key]; ok {
			t.Errorf("oldest metric %s survived cap eviction", key)
		}
		if _, ok := ss.Edges[edgeKey(EdgeMeasuredBy, key, "svc")]; ok {
			t.Errorf("cap-evicted metric %s left a dangling MEASURED_BY edge", key)
		}
	}
	for i := 3; i < 5; i++ {
		if _, ok := ss.Metrics[fmt.Sprintf("m%d|svc", i)]; !ok {
			t.Errorf("newest metric m%d was evicted — cap must drop oldest first", i)
		}
	}
}

// TestSignalStore_Prune_SweepsStaleLogClusterEdgesOnly proves LogClusters
// themselves are untouched (they are Drain-bounded upstream) while their
// stale EMITTED_BY / LOGGED_DURING edges are swept.
func TestSignalStore_Prune_SweepsStaleLogClusterEdgesOnly(t *testing.T) {
	ss := newSignalStore()
	now := time.Now()
	stale := now.Add(-48 * time.Hour)

	ss.UpsertLogCluster("c-old", "tmpl", "ERROR", "svc", stale)
	ss.AddLoggedDuringEdge("c-old", "span-old", stale)
	ss.UpsertLogCluster("c-new", "tmpl", "ERROR", "svc2", now)

	ss.Prune(now.Add(-24*time.Hour), 0)

	if len(ss.LogClusters) != 2 {
		t.Fatalf("LogClusters len = %d, want 2 — Prune must not touch clusters", len(ss.LogClusters))
	}
	if _, ok := ss.Edges[edgeKey(EdgeEmittedBy, "c-old", "svc")]; ok {
		t.Errorf("stale EMITTED_BY edge survived")
	}
	if _, ok := ss.Edges[edgeKey(EdgeLoggedDuring, "c-old", "span-old")]; ok {
		t.Errorf("stale LOGGED_DURING edge survived")
	}
	if _, ok := ss.Edges[edgeKey(EdgeEmittedBy, "c-new", "svc2")]; !ok {
		t.Errorf("fresh EMITTED_BY edge was swept")
	}
}

// TestSignalStore_UpsertRefreshesEdgeTimestamps proves the upsert paths
// refresh edge UpdatedAt on every hit — without this, a continuously-active
// metric or log cluster would lose its correlation edge 24h after creation
// even though the node itself stays fresh.
func TestSignalStore_UpsertRefreshesEdgeTimestamps(t *testing.T) {
	ss := newSignalStore()
	now := time.Now()
	stale := now.Add(-48 * time.Hour)

	// Created stale, then re-upserted fresh: edges must carry the fresh time.
	ss.UpsertMetric("cpu", "svc", 1.0, stale)
	ss.UpsertMetric("cpu", "svc", 2.0, now)
	ss.UpsertLogCluster("c1", "tmpl", "ERROR", "svc", stale)
	ss.UpsertLogCluster("c1", "tmpl", "ERROR", "svc", now)
	ss.AddLoggedDuringEdge("c1", "span-1", stale)
	ss.AddLoggedDuringEdge("c1", "span-1", now)

	ss.Prune(now.Add(-24*time.Hour), 0)

	for _, ek := range []string{
		edgeKey(EdgeMeasuredBy, "cpu|svc", "svc"),
		edgeKey(EdgeEmittedBy, "c1", "svc"),
		edgeKey(EdgeLoggedDuring, "c1", "span-1"),
	} {
		if _, ok := ss.Edges[ek]; !ok {
			t.Errorf("edge %s swept despite a fresh upsert — UpdatedAt not refreshed", ek)
		}
	}
}
