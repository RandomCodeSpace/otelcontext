package ingest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/prometheus"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/tsdb"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestPrometheusDeltaHTTPIsBadRequestWithoutForwarding(t *testing.T) {
	s := NewMetricsServer(nil, nil, nil, aggTestConfig())
	attachPrometheus(t, s, "default", true, func(http.ResponseWriter, *http.Request) { t.Error("unsupported delta was forwarded") })
	req := metricsRequest(&metricspb.Metric{Name: "delta", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
		AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
		IsMonotonic:            true,
		DataPoints:             []*metricspb.NumberDataPoint{{TimeUnixNano: uint64(time.Now().UnixNano()), Value: &metricspb.NumberDataPoint_AsInt{AsInt: 1}}},
	}}})
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	NewHTTPHandler(nil, nil, s).handleMetrics(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("delta HTTP status=%d body=%s", w.Code, w.Body.String())
	}
}

func attachPrometheus(t *testing.T, s *MetricsServer, tenant string, required bool, h http.HandlerFunc) {
	t.Helper()
	backend := httptest.NewServer(h)
	t.Cleanup(backend.Close)
	c, err := prometheus.NewClient(prometheus.Config{URL: backend.URL, TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if err = s.SetPrometheusClient(c, required); err != nil {
		t.Fatal(err)
	}
}

func TestPrometheusForwardsOriginalOTLP(t *testing.T) {
	now := time.Now()
	rm := hostmetricsResource(now, stringAttr("host.id", "i-123"), stringAttr("host.name", "node-a"), stringAttr("service.name", "checkout"))
	rm.SchemaUrl = "https://opentelemetry.io/schemas/1.27.0"
	rm.ScopeMetrics[0].Scope = &commonpb.InstrumentationScope{Name: "test-scope", Version: "1.2.3"}
	rm.ScopeMetrics[0].Metrics = append(rm.ScopeMetrics[0].Metrics, histogramMetric("latency", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, now, []uint64{1, 2, 3, 4}), expHistogramMetric("size", 2, now, []uint64{1, 2}))
	rm.ScopeMetrics[0].Metrics[0].Unit = "s"
	rm.ScopeMetrics[0].Metrics[3].GetExponentialHistogram().AggregationTemporality = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{rm}}
	s := NewMetricsServer(nil, nil, nil, aggTestConfig())
	var calls atomic.Int64
	attachPrometheus(t, s, "default", true, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/otlp/v1/metrics" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Errorf("wrong OTLP endpoint: %s", r.URL.Path)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var got colmetricspb.ExportMetricsServiceRequest
		if err = proto.Unmarshal(data, &got); err != nil || !proto.Equal(req, &got) {
			t.Errorf("OTLP changed: %v\n%v", err, &got)
		}
	})
	resp, err := s.Export(context.Background(), req)
	if err != nil || resp.PartialSuccess != nil || calls.Load() != 1 {
		t.Fatalf("export calls=%d resp=%v err=%v", calls.Load(), resp, err)
	}
}

func TestPrometheusRequiredFailureDoesNotAccountRetry(t *testing.T) {
	now := time.Now()
	s := metricsExportServer(t, now)
	var callbacks int
	s.SetMetricCallback(func(tsdb.RawMetric) { callbacks++ })
	var calls atomic.Int64
	attachPrometheus(t, s, "default", true, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
		}
	})
	req := metricsRequest(gaugeMetricPoints("queue.depth", 1, now))
	if _, err := s.Export(context.Background(), req); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("required failure=%v", err)
	}
	if callbacks != 0 || len(s.aggregateEngine.TopologySnapshot("default").Metrics) != 0 {
		t.Fatal("failed backend write changed local state")
	}
	if _, err := s.Export(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	snap := s.aggregateEngine.TopologySnapshot("default")
	if callbacks != 1 || len(snap.Metrics) != 1 {
		t.Fatalf("retry callbacks=%d metrics=%v", callbacks, snap.Metrics)
	}
	var count uint64
	for _, win := range snap.Metrics[0].Windows {
		count += win.ValueCount
	}
	if count != 1 {
		t.Fatalf("retry double counted: %d", count)
	}
}

func TestPrometheusMixedTenantAndContextPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		expected int
	}{
		{"resource tenants", context.Background(), 1},
		{"context", storage.WithTenantContext(context.Background(), "other"), 0},
		{"grpc metadata", metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-tenant-id", "other")), 0},
		{"bound principal", authn.WithPrincipal(storage.WithTenantContext(context.Background(), "default"), authn.Principal{Kind: authn.KindTenant, Tenant: "other"}), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := aggTestConfig()
			cfg.OTLPTrustResourceTenant = true
			s := NewMetricsServer(nil, nil, nil, cfg)
			var local int
			s.SetMetricCallback(func(tsdb.RawMetric) { local++ })
			now := time.Now()
			req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{
				hostmetricsResource(now, stringAttr("tenant.id", "default"), stringAttr("host.name", "one")),
				hostmetricsResource(now, stringAttr("tenant.id", "other"), stringAttr("host.name", "two")),
			}}
			var calls atomic.Int64
			attachPrometheus(t, s, "default", true, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				data, _ := io.ReadAll(r.Body)
				var got colmetricspb.ExportMetricsServiceRequest
				if err := proto.Unmarshal(data, &got); err != nil {
					t.Error(err)
				}
				if len(got.ResourceMetrics) != 1 || !proto.Equal(got.ResourceMetrics[0], req.ResourceMetrics[0]) {
					t.Errorf("cross-tenant export: %v", &got)
				}
			})
			if _, err := s.Export(tc.ctx, req); err != nil {
				t.Fatal(err)
			}
			if int(calls.Load()) != tc.expected || local != 4 {
				t.Fatalf("backend=%d local=%d", calls.Load(), local)
			}
		})
	}
}

func TestPrometheusOptionalFailureAndPartialSuccess(t *testing.T) {
	t.Run("optional failure", func(t *testing.T) {
		s := NewMetricsServer(nil, nil, nil, aggTestConfig())
		var local int
		s.SetMetricCallback(func(tsdb.RawMetric) { local++ })
		attachPrometheus(t, s, "default", false, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
		resp, err := s.Export(context.Background(), metricsRequest(gaugeMetricPoints("cpu", 1, time.Now())))
		if err != nil || local != 1 || !strings.Contains(resp.GetPartialSuccess().GetErrorMessage(), "forwarding failed") {
			t.Fatalf("resp=%v local=%d err=%v", resp, local, err)
		}
	})
	t.Run("backend partial", func(t *testing.T) {
		s := NewMetricsServer(nil, nil, nil, aggTestConfig())
		var local int
		s.SetMetricCallback(func(tsdb.RawMetric) { local++ })
		attachPrometheus(t, s, "default", true, func(w http.ResponseWriter, r *http.Request) {
			data, _ := proto.Marshal(&colmetricspb.ExportMetricsServiceResponse{PartialSuccess: &colmetricspb.ExportMetricsPartialSuccess{RejectedDataPoints: 1, ErrorMessage: "bad point"}})
			_, _ = w.Write(data)
		})
		resp, err := s.Export(context.Background(), metricsRequest(gaugeMetricPoints("cpu", 2, time.Now())))
		if err != nil || local != 0 || resp.GetPartialSuccess().GetRejectedDataPoints() != 1 {
			t.Fatalf("resp=%v local=%d err=%v", resp, local, err)
		}
	})
	t.Run("projection rejection does not reject stored histogram", func(t *testing.T) {
		now := time.Now()
		s := metricsExportServer(t, now)
		attachPrometheus(t, s, "default", true, func(w http.ResponseWriter, r *http.Request) {})
		resp, err := s.Export(context.Background(), metricsRequest(histogramMetric("latency", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, now, []uint64{1, 2, 3, 4})))
		if err != nil || resp.GetPartialSuccess().GetRejectedDataPoints() != 0 || !strings.Contains(resp.GetPartialSuccess().GetErrorMessage(), "projection") {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
	})
}

func TestPrometheusRequiredFailureHTTPIsRetryable(t *testing.T) {
	s := NewMetricsServer(nil, nil, nil, aggTestConfig())
	attachPrometheus(t, s, "default", true, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	body, _ := proto.Marshal(metricsRequest(gaugeMetricPoints("cpu", 1, time.Now())))
	r := httptest.NewRequest("POST", "/v1/metrics", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	NewHTTPHandler(nil, nil, s).handleMetrics(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("non-retryable HTTP failure: %d %s", w.Code, w.Body.String())
	}
}
