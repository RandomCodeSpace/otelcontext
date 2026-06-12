package graphrag

import (
	"fmt"
	"testing"
	"time"
)

// TestAnomaliesSinceLimit proves the bounded variant caps the walk while the
// unlimited spellings (n<=0 and the legacy AnomaliesSince) return everything.
func TestAnomaliesSinceLimit(t *testing.T) {
	as := newAnomalyStore()
	now := time.Now()
	for i := 0; i < 10; i++ {
		as.AddAnomaly(AnomalyNode{
			ID:        fmt.Sprintf("anom-%d", i),
			Service:   "svc",
			Timestamp: now,
		})
	}
	since := now.Add(-time.Minute)

	if got := len(as.AnomaliesSinceLimit(since, 3)); got != 3 {
		t.Errorf("AnomaliesSinceLimit(3) returned %d, want 3", got)
	}
	if got := len(as.AnomaliesSinceLimit(since, 0)); got != 10 {
		t.Errorf("AnomaliesSinceLimit(0) returned %d, want 10 (unlimited)", got)
	}
	if got := len(as.AnomaliesSince(since)); got != 10 {
		t.Errorf("AnomaliesSince returned %d, want 10", got)
	}
}

// TestCorrelateWithRecent_BoundedWalk proves correlateWithRecent fans out at
// most maxCorrelationWalk PRECEDED_BY edges per detection, no matter how
// large the anomaly backlog in the ±30s window is.
func TestCorrelateWithRecent_BoundedWalk(t *testing.T) {
	stores := newTenantStores(time.Hour, 0)
	now := time.Now()

	for i := 0; i < maxCorrelationWalk+200; i++ {
		stores.anomalies.AddAnomaly(AnomalyNode{
			ID:        fmt.Sprintf("anom-%d", i),
			Service:   "svc",
			Timestamp: now,
		})
	}

	fresh := AnomalyNode{ID: "anom-fresh", Service: "svc", Timestamp: now}
	stores.anomalies.AddAnomaly(fresh)
	correlateWithRecent(stores, fresh)

	edges := 0
	for _, e := range stores.anomalies.Edges {
		if e.Type == EdgePrecededBy && e.FromID == "anom-fresh" {
			edges++
		}
	}
	if edges == 0 {
		t.Fatalf("no PRECEDED_BY edges minted — correlation broken")
	}
	if edges > maxCorrelationWalk {
		t.Fatalf("correlateWithRecent minted %d edges, want <= %d", edges, maxCorrelationWalk)
	}
}
