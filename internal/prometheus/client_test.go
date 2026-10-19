package prometheus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func testClient(t *testing.T, h http.HandlerFunc, cfg Config) *Client {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	cfg.URL = server.URL
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestQueryPreservesEnvelopeAndParameters(t *testing.T) {
	for _, rangeQuery := range []bool{false, true} {
		for _, status := range []int{200, 400, 422, 503} {
			t.Run(http.StatusText(status)+strconvBool(rangeQuery), func(t *testing.T) {
				body := `{"status":"success","data":{"resultType":"vector","result":[]},"warnings":["partial"],"infos":["info"]}`
				if status != 200 {
					body = `{"status":"error","errorType":"execution","error":"query failed","data":{"partial":true},"warnings":["partial"]}`
				}
				params := url.Values{"query": {`sum by ("host.id") (rate({__name__="requests"}[5m]))`}, "timeout": {"2s"}, "start": {"0"}, "end": {"20"}, "step": {"5s"}}
				c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					path := "/api/v1/query"
					if rangeQuery {
						path += "_range"
					}
					if r.URL.Path != path || r.Method != "POST" {
						t.Errorf("request %s %s", r.Method, r.URL.Path)
					}
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					if r.Form.Encode() != params.Encode() {
						t.Errorf("params = %v", r.Form)
					}
					w.WriteHeader(status)
					_, _ = io.WriteString(w, body)
				}, Config{})
				got, err := c.Query(context.Background(), params, rangeQuery)
				if string(got) != body {
					t.Fatalf("envelope changed: %s", got)
				}
				if status == 200 && err != nil {
					t.Fatal(err)
				}
				if status != 200 {
					code, raw := ErrorResponse(err)
					if code != status || string(raw) != body {
						t.Fatalf("error changed: %d %s", code, raw)
					}
				}
			})
		}
	}
}

func strconvBool(v bool) string {
	if v {
		return "Range"
	}
	return "Instant"
}

func TestQueryTenantIsolation(t *testing.T) {
	var calls atomic.Int64
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"scalar","result":[1,"2"]}}`)
	}, Config{TenantID: "one"})
	for _, ctx := range []context.Context{context.Background(), storage.WithTenantContext(context.Background(), "two"), storage.WithTenantContext(context.Background(), " one "), authn.WithPrincipal(storage.WithTenantContext(context.Background(), "one"), authn.Principal{Kind: authn.KindTenant, Tenant: "two"})} {
		_, err := c.Query(ctx, url.Values{"query": {"1+1"}}, false)
		status, _ := ErrorResponse(err)
		if status != 501 {
			t.Fatalf("cross-tenant query: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("cross-tenant request reached backend")
	}
	_, err := c.Query(storage.WithTenantContext(context.Background(), "one"), url.Values{"query": {"1+1"}}, false)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("bound tenant: calls=%d err=%v", calls.Load(), err)
	}
}

func TestQueryBoundsAndRedirects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		params url.Values
		body   string
		status int
		want   int
	}{
		{"query bytes", Config{MaxQueryBytes: 4}, url.Values{"query": {"up"}}, "", 200, 413},
		{"range", Config{MaxQueryRange: time.Second}, url.Values{"start": {"0"}, "end": {"2"}}, "", 200, 400},
		{"response", Config{MaxResponseBytes: 8}, url.Values{}, `{"status":"success"}`, 200, 502},
		{"invalid JSON", Config{}, url.Values{}, `not json`, 200, 502},
		{"redirect", Config{}, url.Values{}, ``, 302, 502},
		{"series", Config{MaxSeries: 1}, url.Values{}, `{"status":"success","data":{"resultType":"vector","result":[{},{}]}}`, 200, 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://127.0.0.1:1/secret")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}, tc.cfg)
			_, err := c.Query(context.Background(), tc.params, true)
			status, _ := ErrorResponse(err)
			if err == nil || status != tc.want {
				t.Fatalf("status=%d err=%v", status, err)
			}
		})
	}
}

func TestQueryConcurrencyTimeoutAndCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { started <- struct{}{}; <-r.Context().Done() }, Config{Timeout: 100 * time.Millisecond, MaxConcurrent: 1})
	done := make(chan error, 1)
	go func() { _, err := c.Query(context.Background(), url.Values{}, false); done <- err }()
	<-started
	_, err := c.Query(context.Background(), url.Values{}, false)
	if err == nil || !strings.Contains(err.Error(), "concurrency") {
		t.Fatalf("concurrency not bounded: %v", err)
	}
	if err = <-done; err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Query(ctx, url.Values{}, false)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestClientConfigValidation(t *testing.T) {
	for _, raw := range []string{"", "//localhost", "file:///tmp/prometheus", "http://user:pass@localhost", "http://localhost?target=x", "http://localhost#x"} {
		if _, err := NewClient(Config{URL: raw}); err == nil {
			t.Errorf("accepted URL %q", raw)
		}
	}
	if _, err := NewClient(Config{URL: "http://localhost", TenantID: "bad\nvalue"}); err == nil {
		t.Fatal("accepted invalid tenant")
	}
	if _, err := NewClient(Config{URL: "http://localhost", Timeout: -time.Second}); err == nil {
		t.Fatal("accepted negative limit")
	}
	if _, err := NewClient(Config{URL: "http://localhost", MaxIngestBytes: -1}); err == nil {
		t.Fatal("accepted negative ingest byte limit")
	}
	c, err := NewClient(Config{URL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.cfg.MaxIngestBytes != 128<<20 {
		t.Fatalf("default ingest byte limit=%d, want 128 MiB", c.cfg.MaxIngestBytes)
	}
}

func TestErrorResponseIsStructured(t *testing.T) {
	status, body := ErrorResponse(NewError(413, "bad_data", "oversized"))
	var envelope map[string]any
	if status != 413 || json.Unmarshal(body, &envelope) != nil || envelope["status"] != "error" || envelope["errorType"] != "bad_data" {
		t.Fatalf("bad envelope: %d %s", status, body)
	}
}

func TestIngestAdmissionSupports200ExportersWithoutQueryStarvation(t *testing.T) {
	release := make(chan struct{})
	var active, maximum atomic.Int64
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/-/ready" {
			if r.Method != "GET" {
				t.Errorf("readiness method=%s", r.Method)
			}
			return
		}
		if r.URL.Path == "/api/v1/query" {
			<-release
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		<-release
	}, Config{MaxConcurrent: 1, Timeout: 5 * time.Second})
	queryDone := make(chan error, 1)
	go func() { _, err := c.Query(context.Background(), url.Values{}, false); queryDone <- err }()
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Export(context.Background(), ingestAdmissionRequest())
			errs <- err
		}()
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(c.ingestWaiters) < 184 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(c.ingestWaiters) != 184 {
		close(release)
		wg.Wait()
		t.Fatalf("waiters=%d, want 184", len(c.ingestWaiters))
	}
	if err := c.Ready(context.Background()); err != nil {
		close(release)
		wg.Wait()
		t.Fatalf("readiness starved: %v", err)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("synchronized exporter refused: %v", err)
		}
	}
	if err := <-queryDone; err != nil {
		t.Fatal(err)
	}
	assertIngestReleased(t, c)
	if maximum.Load() > 16 {
		t.Fatalf("active backend writes=%d", maximum.Load())
	}
}

func TestIngestWaiterBoundAndCancellation(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { started <- struct{}{}; <-release }, Config{MaxIngestConcurrent: 1, MaxIngestWaiters: 1})
	first := make(chan error, 1)
	go func() {
		_, err := c.Export(context.Background(), ingestAdmissionRequest())
		first <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { _, err := c.Export(ctx, ingestAdmissionRequest()); second <- err }()
	deadline := time.Now().Add(time.Second)
	for len(c.ingestWaiters) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, err := c.Export(context.Background(), ingestAdmissionRequest())
	if err == nil || !strings.Contains(err.Error(), "waiter limit") {
		t.Errorf("unbounded waiter: %v", err)
	}
	cancel()
	if err = <-second; err == nil {
		t.Error("queued export ignored cancellation")
	}
	close(release)
	if err = <-first; err != nil {
		t.Fatal(err)
	}
	assertIngestReleased(t, c)
}

func TestExportRejectsDeltaBeforeBackend(t *testing.T) {
	var calls atomic.Int64
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, Config{})
	for _, metric := range []*metricspb.Metric{
		{Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA}}},
		{Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA}}},
		{Data: &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA}}},
	} {
		_, err := c.Export(context.Background(), &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{metric}}}}}})
		if err == nil || !strings.Contains(err.Error(), "cumulative OTLP export required") {
			t.Fatalf("delta export=%v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("delta request reached backend")
	}
}

func TestReadyReportsBackendOutage(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }, Config{})
	if err := c.Ready(context.Background()); err == nil {
		t.Fatal("unready backend reported ready")
	}
}

func ingestAdmissionRequest() *colmetricspb.ExportMetricsServiceRequest {
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: strings.Repeat("m", 1024),
			Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
				Value: &metricspb.NumberDataPoint_AsInt{AsInt: 1},
			}}}},
		}}}},
	}}}
}

func assertIngestReleased(t *testing.T, c *Client) {
	t.Helper()
	if len(c.ingestWaiters) != 0 || len(c.ingestSlots) != 0 {
		t.Error("ingest admission slot leaked")
	}
	if !c.ingestBytes.TryAcquire(c.cfg.MaxIngestBytes) {
		t.Fatal("ingest byte reservation leaked")
	}
	c.ingestBytes.Release(c.cfg.MaxIngestBytes)
}

func waitForIngestWaiters(t *testing.T, c *Client, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(c.ingestWaiters) != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(c.ingestWaiters); got != want {
		t.Fatalf("waiters=%d, want %d", got, want)
	}
}

func TestIngestByteBudgetConcurrentPressureAndCancellation(t *testing.T) {
	req := ingestAdmissionRequest()
	size := int64(proto.Size(req))
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	started := make(chan struct{}, 1)
	var calls atomic.Int64
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			started <- struct{}{}
		}
		<-release
	}, Config{MaxIngestConcurrent: 1, MaxIngestWaiters: 64, MaxIngestBytes: 2 * size, Timeout: 5 * time.Second})
	t.Cleanup(unblock)
	first := make(chan error, 1)
	go func() { _, err := c.Export(context.Background(), req); first <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first export did not reach backend")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 32)
	for i := 0; i < cap(done); i++ {
		go func() { _, err := c.Export(ctx, ingestAdmissionRequest()); done <- err }()
	}
	// One active and one queued request fill the byte budget, although the
	// count limit has room for every caller. All other callers must fail now.
	for i := 0; i < cap(done)-1; i++ {
		select {
		case err := <-done:
			status, _ := ErrorResponse(err)
			if err == nil || status != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "byte limit") {
				t.Fatalf("byte pressure must be retryable: status=%d err=%v", status, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("byte overflow waited for an ingest slot")
		}
	}
	waitForIngestWaiters(t, c, 1)
	if calls.Load() != 1 {
		t.Fatalf("backend calls=%d, want 1", calls.Load())
	}
	// Invalid UTF-8 would fail Marshal. A byte rejection instead proves the
	// reservation precedes serialization as well as the queue wait.
	invalid := ingestAdmissionRequest()
	invalid.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Name = "\xff"
	if _, err := c.Export(context.Background(), invalid); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("serialized before byte admission: %v", err)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("queued export ignored cancellation")
	}
	waitForIngestWaiters(t, c, 0)
	if !c.ingestBytes.TryAcquire(size) {
		t.Fatal("canceled waiter retained bytes")
	}
	c.ingestBytes.Release(size)
	if c.ingestBytes.TryAcquire(size + 1) {
		c.ingestBytes.Release(size + 1)
		t.Fatal("active export lost its reservation")
	}

	// The canceled waiter's reservation can immediately admit another export.
	retry := make(chan error, 1)
	go func() { _, err := c.Export(context.Background(), req); retry <- err }()
	waitForIngestWaiters(t, c, 1)
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-retry; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("backend calls=%d, want 2", calls.Load())
	}
	assertIngestReleased(t, c)
}

func TestIngestByteBudgetReleaseOnExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{
		{"success", 0},
		{"backend error", http.StatusServiceUnavailable},
		{"invalid response", http.StatusBadGateway},
		{"response too large", http.StatusBadGateway},
		{"marshal error", http.StatusBadGateway},
		{"request too large", http.StatusRequestEntityTooLarge},
		{"byte budget too small", http.StatusServiceUnavailable},
		{"already canceled", http.StatusServiceUnavailable},
		{"active cancellation", http.StatusServiceUnavailable},
		{"active timeout", http.StatusServiceUnavailable},
		{"queued timeout", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := ingestAdmissionRequest()
			cfg := Config{MaxIngestBytes: int64(proto.Size(req)), MaxResponseBytes: 16, MaxIngestConcurrent: 1}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch tc.name {
			case "marshal error":
				req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Name = "\xff"
			case "request too large":
				cfg.MaxRequestBytes = cfg.MaxIngestBytes - 1
			case "byte budget too small":
				cfg.MaxIngestBytes--
			case "already canceled":
				cancel()
			case "active timeout", "queued timeout":
				cfg.Timeout = 50 * time.Millisecond
			}
			var calls atomic.Int64
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				switch tc.name {
				case "backend error":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "invalid response":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, "invalid")
				case "response too large":
					_, _ = io.WriteString(w, strings.Repeat("x", 17))
				case "active cancellation":
					cancel()
					<-r.Context().Done()
				case "active timeout":
					<-r.Context().Done()
				}
			}, cfg)
			if tc.name == "queued timeout" {
				// Occupy the write slot without consuming the byte budget.
				c.ingestSlots <- struct{}{}
			}
			_, err := c.Export(ctx, req)
			if tc.name == "queued timeout" {
				<-c.ingestSlots
				if calls.Load() != 0 {
					t.Error("timed-out waiter reached backend")
				}
			}
			if tc.want == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else if status, _ := ErrorResponse(err); err == nil || status != tc.want {
				t.Fatalf("status=%d, want %d; err=%v", status, tc.want, err)
			}
			assertIngestReleased(t, c)
		})
	}
}
