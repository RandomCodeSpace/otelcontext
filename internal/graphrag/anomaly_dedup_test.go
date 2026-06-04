package graphrag

import (
	"testing"
	"time"
)

// TestAnomalyDedupBoundsStore proves the root-cause fix for the soak finding:
// stable per-(service,type) anomaly IDs make repeated detection ticks UPSERT a
// single evolving node instead of minting a new one each tick. With the old
// UnixNano-suffixed IDs, 100 ticks over 2 services produced 200 nodes and an
// O(N²) PRECEDED_BY edge explosion (which grew AnomalyStore to ~1.3 GB in a
// 15-min soak). Under the fix, node and edge counts stay bounded regardless of
// how many ticks fire.
func TestAnomalyDedupBoundsStore(t *testing.T) {
	stores := newTenantStores(time.Hour)
	base := time.Unix(1_700_000_000, 0)

	const ticks = 100
	for i := range ticks {
		ts := base.Add(time.Duration(i) * 10 * time.Second)
		for _, svc := range []string{"checkout", "payments"} {
			a := AnomalyNode{
				ID:        "anom_" + svc + "_err", // stable ID, mirrors detectAnomaliesForTenant
				Type:      AnomalyErrorSpike,
				Service:   svc,
				Timestamp: ts,
			}
			stores.anomalies.AddAnomaly(a)
			correlateWithRecent(stores, a)
		}
	}

	if got := len(stores.anomalies.Anomalies); got != 2 {
		t.Fatalf("expected 2 deduped anomaly nodes after %d ticks, got %d", ticks, got)
	}
	// TRIGGERED_BY: 2 (one per node). PRECEDED_BY: at most the 2-node mesh.
	// The point is it does NOT scale with tick count.
	if got := len(stores.anomalies.Edges); got > 8 {
		t.Fatalf("edge count must stay bounded under dedup, got %d after %d ticks", got, ticks)
	}
}
