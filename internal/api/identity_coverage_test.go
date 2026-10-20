package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func TestAPIDisclosesIdentityOverflowWithoutChangingTrafficArray(t *testing.T) {
	s, _, _ := aggregateTestServer(t, false)
	e, err := aggregate.NewEngine(aggregate.EngineConfig{Mode: aggregate.ModeAggregate, Registrar: aggregate.NewMemRegistrar(&aggregate.MemRegistrarOptions{Limits: map[aggregate.Kind]int{aggregate.KindOperation: 1}})})
	if err != nil {
		t.Fatal(err)
	}
	s.SetAggregateEngine(e)
	now := time.Now()
	for _, name := range []string{"known", "overflow"} {
		r := e.NewReducer(now)
		r.ReduceSpan(aggregate.SpanInput{Tenant: "default", Service: "svc", SpanName: name, Timestamp: now, Root: true, DurationMicros: 100})
		if _, err := e.ApplyReducerErr(r); err != nil {
			t.Fatal(err)
		}
	}
	_, body := getJSON(t, s.handleGetDashboardStats, "/api/metrics/dashboard", "default")
	if body["coverage"] != "limited" || body["coverage_note"] != aggregate.LimitedCoverageNote || body["spans"] != float64(2) {
		t.Fatalf("dashboard lost coverage/count contract: %+v", body)
	}
	coverage, ok := body["identity_coverage"].(map[string]any)
	if !ok || coverage["trace_points"] != float64(1) {
		t.Fatalf("coverage=%v", body["identity_coverage"])
	}
	req := httptest.NewRequest("GET", "/api/metrics/traffic", nil)
	req = req.WithContext(storage.WithTenantContext(req.Context(), "default"))
	w := httptest.NewRecorder()
	s.handleGetTrafficMetrics(w, req)
	var points []storage.TrafficPoint
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &points) != nil {
		t.Fatalf("traffic is no longer array: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get(aggregate.CoverageHeader) != "limited" || w.Header().Get("OtelContext-Coverage-Note") != aggregate.LimitedCoverageNote {
		t.Fatal(w.Header())
	}
	var identity aggregate.IdentityCoverage
	if err := json.Unmarshal([]byte(w.Header().Get("OtelContext-Identity-Coverage")), &identity); err != nil || identity.TracePoints != 1 {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
}
