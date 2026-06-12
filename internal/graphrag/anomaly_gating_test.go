package graphrag

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/tsdb"
)

// feedErrorSpan pushes one ERROR span through processSpan synchronously so
// the default tenant's ServiceStore error rate trips the spike detector.
func feedErrorSpan(g *GraphRAG, spanID string) {
	g.processSpan(&spanEvent{
		Span: storage.Span{
			TraceID:       "trace-gate",
			SpanID:        spanID,
			OperationName: "/op",
			ServiceName:   "orders",
			StartTime:     time.Now(),
		},
		TraceID: "trace-gate",
		Status:  "STATUS_CODE_ERROR",
		Tenant:  storage.DefaultTenantID,
	})
}

// deleteAnomaly removes one anomaly node so a later scan provably re-creates
// it (or provably does not, when the tenant is gated).
func deleteAnomaly(stores *tenantStores, id string) {
	stores.anomalies.mu.Lock()
	delete(stores.anomalies.Anomalies, id)
	stores.anomalies.mu.Unlock()
}

// TestDetectAnomalies_SkipsTenantsWithoutNewEvents proves the scan gate:
// a tenant with no span/log/metric processed since the previous tick is
// skipped entirely, and resumes being scanned as soon as an event lands.
func TestDetectAnomalies_SkipsTenantsWithoutNewEvents(t *testing.T) {
	g := New(nil, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		feedErrorSpan(g, fmt.Sprintf("s-%d", i))
	}

	// Scan 1 (first ever): always runs, detects the error spike.
	g.detectAnomalies(ctx)
	stores := g.storesForTenant(storage.DefaultTenantID)
	if _, ok := stores.anomalies.Anomalies["anom_orders_err"]; !ok {
		t.Fatalf("scan 1 did not detect the error spike")
	}

	// Scan 2: no events since scan 1 — the tenant must be skipped, so a
	// deleted anomaly stays gone.
	deleteAnomaly(stores, "anom_orders_err")
	g.detectAnomalies(ctx)
	if _, ok := stores.anomalies.Anomalies["anom_orders_err"]; ok {
		t.Fatalf("scan 2 re-created the anomaly — idle tenant was not skipped")
	}

	// Scan 3: a fresh event re-arms the tenant and detection resumes.
	feedErrorSpan(g, "s-rearm")
	g.detectAnomalies(ctx)
	if _, ok := stores.anomalies.Anomalies["anom_orders_err"]; !ok {
		t.Fatalf("scan 3 skipped an active tenant — gate stuck")
	}
}

// TestProcessEvents_StampLastEventAt proves all three event paths arm the
// anomaly-scan gate.
func TestProcessEvents_StampLastEventAt(t *testing.T) {
	g := New(nil, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	cases := []struct {
		name   string
		tenant string
		fire   func(tenant string)
	}{
		{"span", "tenant-span", func(tn string) {
			g.processSpan(&spanEvent{
				Span:    storage.Span{TraceID: "t", SpanID: "s", ServiceName: "svc", StartTime: time.Now()},
				TraceID: "t", Status: "STATUS_CODE_UNSET", Tenant: tn,
			})
		}},
		{"log", "tenant-log", func(tn string) {
			g.processLog(&logEvent{
				Log:    storage.Log{ServiceName: "svc", Body: "boom failed id=1", Severity: "ERROR", Timestamp: time.Now()},
				Tenant: tn,
			})
		}},
		{"metric", "tenant-metric", func(tn string) {
			g.processMetric(&metricEvent{
				Metric: tsdb.RawMetric{Name: "cpu", ServiceName: "svc", Value: 1.0, Timestamp: time.Now()},
				Tenant: tn,
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fire(tc.tenant)
			st := g.storesForTenant(tc.tenant)
			if st.lastEventAt.Load() == 0 {
				t.Fatalf("%s event did not stamp lastEventAt", tc.name)
			}
		})
	}
}
