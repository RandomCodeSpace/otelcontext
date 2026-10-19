package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/api/views"
	"github.com/RandomCodeSpace/otelcontext/internal/prometheus"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func prometheusAPIServer(t *testing.T, h http.HandlerFunc, maxQueryBytes int) *Server {
	t.Helper()
	backend := httptest.NewServer(h)
	t.Cleanup(backend.Close)
	c, err := prometheus.NewClient(prometheus.Config{URL: backend.URL, MaxQueryBytes: maxQueryBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	s := &Server{}
	s.SetPrometheusClient(c)
	return s
}

func TestPrometheusHTTPGetPostAndErrors(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		for _, rangeQuery := range []bool{false, true} {
			t.Run(method+map[bool]string{true: "Range", false: "Instant"}[rangeQuery], func(t *testing.T) {
				want := `{"status":"error","errorType":"execution","error":"bad expression","warnings":["partial"]}`
				s := prometheusAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
					_ = r.ParseForm()
					if r.Form.Get("query") != "sum(up)" || r.Form.Get("timeout") != "1s" {
						t.Errorf("parameters lost: %v", r.Form)
					}
					path := "/api/v1/query"
					if rangeQuery {
						path += "_range"
					}
					if r.URL.Path != path {
						t.Errorf("path=%s", r.URL.Path)
					}
					w.WriteHeader(422)
					_, _ = io.WriteString(w, want)
				}, 0)
				params := url.Values{"query": {"sum(up)"}, "timeout": {"1s"}, "start": {"0"}, "end": {"10"}, "step": {"1"}}.Encode()
				var req *http.Request
				target := "/api/v1/query"
				if rangeQuery {
					target += "_range"
				}
				if method == "GET" {
					req = httptest.NewRequest(method, target+"?"+params, nil)
				} else {
					req = httptest.NewRequest(method, target, strings.NewReader(params))
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				}
				w := httptest.NewRecorder()
				mux := http.NewServeMux()
				s.RegisterRoutes(mux)
				mux.ServeHTTP(w, req)
				if w.Code != 422 || w.Body.String() != want {
					t.Fatalf("response=%d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestPrometheusHTTPBoundaries(t *testing.T) {
	var calls atomic.Int64
	s := prometheusAPIServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, 24)
	for _, tc := range []struct {
		name, method, target, body, contentType, tenant string
		want                                            int
	}{
		{"other tenant", "GET", "/api/v1/query?query=up", "", "", "other", 501},
		{"large URL", "GET", "/api/v1/query?query=" + strings.Repeat("x", 32), "", "", "", 413},
		{"large body", "POST", "/api/v1/query", "query=" + strings.Repeat("x", 32), "application/x-www-form-urlencoded", "", 413},
		{"malformed URL", "GET", "/api/v1/query?query=%zz", "", "", "", 400},
		{"wrong content type", "POST", "/api/v1/query", `{"query":"up"}`, "application/json", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			if tc.tenant != "" {
				r = r.WithContext(storage.WithTenantContext(r.Context(), tc.tenant))
			}
			w := httptest.NewRecorder()
			s.handlePrometheusQuery(w, r)
			if w.Code != tc.want || !strings.Contains(w.Body.String(), `"status":"error"`) {
				t.Fatalf("response=%d %s", w.Code, w.Body.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached backend")
	}
	w := httptest.NewRecorder()
	(&Server{}).handlePrometheusQuery(w, httptest.NewRequest("GET", "/api/v1/query?query=up", nil))
	if w.Code != 503 {
		t.Fatalf("disabled status=%d", w.Code)
	}
}

func TestPrometheusRetainedHostMetricsAndDiscovery(t *testing.T) {
	start := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Second)
	end := start.Add(7 * 24 * time.Hour)
	s := prometheusAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("start") != start.Format(time.RFC3339Nano) || r.Form.Get("end") != end.Format(time.RFC3339Nano) {
			t.Errorf("range=%v", r.Form)
		}
		if r.URL.Path == "/api/v1/label/__name__/values" {
			if len(r.Form["match[]"]) != 3 {
				t.Errorf("host selectors=%v", r.Form)
			}
			_, _ = io.WriteString(w, `{"status":"success","data":["system.cpu.utilization","system.memory.usage","system.filesystem.utilization"]}`)
			return
		}
		query := r.Form.Get("query")
		if !strings.Contains(query, `__name__="system.cpu.utilization"`) || !strings.Contains(query, `"host.name"="node-a"`) || !strings.Contains(query, `"host.name"="","host.id"="node-a"`) {
			t.Errorf("host query=%s", query)
		}
		body := map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{map[string]any{
			"metric": map[string]string{"__name__": "system.cpu.utilization", "host.id": "i-123", "host.name": "node-a", "cpu": "cpu0"},
			"values": []any{[]any{start.Unix(), "0.42"}, []any{end.Unix(), "0.5"}},
		}}}}
		_ = json.NewEncoder(w).Encode(body)
	}, 0)
	params := url.Values{"name": {"system.cpu.utilization"}, "service_name": {"host/node-a"}, "start": {start.Format(time.RFC3339Nano)}, "end": {end.Format(time.RFC3339Nano)}}
	w, buckets := getArray[views.MetricBucket](t, s.handleGetMetricBuckets, "/api/metrics?"+params.Encode(), "default")
	if len(buckets) != 2 || buckets[0].ServiceName != "host/node-a" || buckets[0].Sum != 0.42 || buckets[0].Count != 1 || !buckets[0].TimeBucket.Equal(start) || !strings.Contains(buckets[0].AttributesJSON, `"host.name":"node-a"`) {
		t.Fatalf("buckets=%+v", buckets)
	}
	if w.Header().Get("OtelContext-Metric-Source") != "prometheus" || w.Header().Get("OtelContext-Data-Coverage") != "sampled" {
		t.Fatalf("headers=%v", w.Header())
	}
	_, names := getArray[string](t, s.handleGetMetricNames, "/api/metadata/metrics?"+params.Encode(), "default")
	if len(names) != 3 {
		t.Fatalf("names=%v", names)
	}
}

func TestPrometheusOtherTenantKeepsAggregateMetrics(t *testing.T) {
	s, _, engine := aggregateTestServer(t, true)
	seedAggregateMetric(t, engine, "other", "checkout", "queue.depth", 7)
	var calls atomic.Int64
	p := prometheusAPIServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, 0)
	s.SetPrometheusClient(p.prometheusClient)
	start, end := metricWindowRange()
	_, buckets := getArray[views.MetricBucket](t, s.handleGetMetricBuckets, "/api/metrics?name=queue.depth&start="+start+"&end="+end, "other")
	if len(buckets) != 1 || buckets[0].Sum != 7 || calls.Load() != 0 {
		t.Fatalf("fallback buckets=%v backend=%d", buckets, calls.Load())
	}
}

func TestPrometheusQuotedMetricSelector(t *testing.T) {
	name := `cpu"} or up or {__name__="`
	selectors := metricSelectors(name, `host/id"}`)
	if len(selectors) != 3 || !strings.Contains(selectors[0], `cpu\"} or up or {__name__=\"`) {
		t.Fatalf("unescaped matcher: %v", selectors)
	}
	if !strings.Contains(selectors[2], `"host.name"=""`) {
		t.Fatalf("host-id fallback must require absent host.name: %s", selectors[2])
	}
}

func TestPrometheusQueryCancellationContext(t *testing.T) {
	s := prometheusAPIServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("canceled request reached backend") }, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	s.handlePrometheusQuery(w, httptest.NewRequest("GET", "/api/v1/query?query=up", nil).WithContext(ctx))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "canceled") {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
}
