package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/ingest"
	"github.com/RandomCodeSpace/otelcontext/internal/mcp"
	"github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/prometheus"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLocalPromQLIngestHTTPAndMCP(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "false")
	db, err := storage.NewDatabase("sqlite", filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := db.DB()
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := migrate.Up(context.Background(), db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	backend, err := prometheus.NewLocal(db, prometheus.Config{TenantID: "alpha"}, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	receiver := ingest.NewMetricsServer(nil, nil, nil, &config.Config{DefaultTenant: "alpha"})
	if err := receiver.SetPrometheusClient(backend, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}}}}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: "queue.depth", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
			TimeUnixNano: uint64(now.UnixNano()), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: 42},
		}}}}}}}},
	}}}
	if _, err := receiver.Export(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	server.SetPrometheusClient(backend)
	expr := `{__name__="queue.depth","service.name"="checkout"}`
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		params := url.Values{"query": {expr}, "time": {now.Format(time.RFC3339Nano)}}
		var r *http.Request
		if method == http.MethodGet {
			r = httptest.NewRequest(method, "/api/v1/query?"+params.Encode(), nil)
		} else {
			r = httptest.NewRequest(method, "/api/v1/query", strings.NewReader(params.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		r = r.WithContext(storage.WithTenantContext(r.Context(), "alpha"))
		w := httptest.NewRecorder()
		server.handlePrometheusQuery(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"42"`) {
			t.Fatalf("HTTP %s: %d %s", method, w.Code, w.Body.String())
		}
	}
	mcpServer := mcp.New("alpha", nil, nil, nil)
	mcpServer.SetPromQLQuerier(backend, backend.TenantID())
	for _, name := range []string{"promql_query", "promql_query_range"} {
		args := map[string]any{"query": expr, "time": now.Format(time.RFC3339Nano)}
		if name == "promql_query_range" {
			args = map[string]any{"query": expr, "start": now.Add(-time.Minute).Format(time.RFC3339Nano), "end": now.Format(time.RFC3339Nano), "step": "60s"}
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mcpServer.Handler().ServeHTTP(w, r)
		var rpc struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &rpc); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || rpc.Result.IsError || len(rpc.Result.Content) != 1 || !strings.Contains(rpc.Result.Content[0].Text, `"42"`) {
			t.Fatalf("MCP %s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// Required local storage must reject an export before local projections when
	// a commit is impossible. No separate Prometheus endpoint was ever configured.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Export(context.Background(), req); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("closed DB export: %v", err)
	}
}
