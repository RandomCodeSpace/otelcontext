package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Opt in with OTELCONTEXT_TEST_PROMETHEUS_BINARY=/absolute/path/to/prometheus.
// Uses a disposable TSDB and loopback listener; never contacts a live instance.
// Reference checked with the official Prometheus 3.15.0 Linux amd64 binary.
func TestRealPrometheusOTLPAndPromQL(t *testing.T) {
	binary := os.Getenv("OTELCONTEXT_TEST_PROMETHEUS_BINARY")
	if binary == "" {
		t.Skip("set OTELCONTEXT_TEST_PROMETHEUS_BINARY for native OTLP/PromQL proof")
	}
	version, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("version: %v %s", err, version)
	}
	t.Log(strings.TrimSpace(string(version)))
	dir := t.TempDir()
	config := filepath.Join(dir, "prometheus.yml")
	if err = os.WriteFile(config, []byte("global:\n  scrape_interval: 1m\notlp:\n  translation_strategy: NoTranslation\n  promote_resource_attributes: [service.name, service.instance.id, host.id, host.name]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	baseURL := "http://" + address
	var process *exec.Cmd
	var log *os.File
	stop := func() {
		if process != nil {
			_ = process.Process.Signal(os.Interrupt)
			_ = process.Wait()
			process = nil
			_ = log.Close()
		}
	}
	t.Cleanup(stop)
	start := func() {
		var err error
		log, err = os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		process = exec.Command(binary, "--config.file="+config, "--storage.tsdb.path="+filepath.Join(dir, "data"), "--storage.tsdb.retention.time=7d", "--web.listen-address="+address, "--web.enable-otlp-receiver", "--query.max-samples=100000", "--query.timeout=2s")
		process.Stdout = log
		process.Stderr = log
		if err = process.Start(); err != nil {
			t.Fatal(err)
		}
		h := &http.Client{Timeout: 100 * time.Millisecond}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := h.Get(baseURL + "/-/ready")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "server.log"))
		t.Fatalf("Prometheus did not become ready: %s", data)
	}
	start()
	c, err := NewClient(Config{URL: baseURL, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	attr := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	point := func(at time.Time, v float64) *metricspb.NumberDataPoint {
		return &metricspb.NumberDataPoint{TimeUnixNano: uint64(at.UnixNano()), StartTimeUnixNano: uint64(at.Add(-time.Hour).UnixNano()), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v}}
	}
	export := func(metrics ...*metricspb.Metric) {
		resp, err := c.Export(ctx, &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{attr("service.name", "checkout"), attr("service.instance.id", "one"), attr("host.id", "i-123"), attr("host.name", "node-a")}}, ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: metrics}}}}})
		if err != nil || resp.GetPartialSuccess().GetRejectedDataPoints() != 0 {
			t.Fatalf("OTLP export: response=%v err=%v", resp, err)
		}
	}
	end := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	for day := 0; day < 7; day++ {
		at := end.Add(time.Duration(day-6) * 24 * time.Hour)
		// Finish the gauge just before the counter fixture: target_info is
		// shared by instruments on this resource and must also stay ordered.
		if day == 6 {
			at = at.Add(-4 * time.Minute)
		}
		export(&metricspb.Metric{Name: "system.cpu.utilization", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{point(at, float64(day+1)/10)}}}})
	}
	for i, v := range []float64{10, 20, 5, 15} {
		at := end.Add(time.Duration(i-3) * time.Minute)
		sum := float64(2 * (i + 1))
		export(&metricspb.Metric{Name: "fixture.requests", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, IsMonotonic: true, DataPoints: []*metricspb.NumberDataPoint{point(at, v)}}}},
			&metricspb.Metric{Name: "fixture.latency", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricspb.HistogramDataPoint{{TimeUnixNano: uint64(at.UnixNano()), StartTimeUnixNano: uint64(end.Add(-time.Hour).UnixNano()), Count: uint64(2 * (i + 1)), Sum: &sum, ExplicitBounds: []float64{1, 2}, BucketCounts: []uint64{uint64(i + 1), uint64(i + 1), 0}}}}}})
	}
	queryValue := func(expression string, at time.Time) float64 {
		body, err := c.Query(ctx, url.Values{"query": {expression}, "time": {at.Format(time.RFC3339Nano)}}, false)
		if err != nil {
			t.Fatalf("%s: %v %s", expression, err, body)
		}
		var envelope struct {
			Data struct {
				Result []struct {
					Value [2]json.RawMessage `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &envelope) != nil || len(envelope.Data.Result) != 1 {
			t.Fatalf("%s: %s", expression, body)
		}
		var value string
		_ = json.Unmarshal(envelope.Data.Result[0].Value[1], &value)
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatal(err)
		}
		return number
	}
	for _, tc := range []struct {
		expr string
		want float64
	}{
		{`sum by ("host.id") ({__name__="system.cpu.utilization","host.name"="node-a"})`, 0.7},
		{`avg by ("service.name") ({__name__="system.cpu.utilization"})`, 0.7},
		{`rate({__name__="fixture.requests"}[4m])`, 25.0 / 180},
		{`increase({__name__="fixture.requests"}[4m])`, 25.0 * 240 / 180},
		{`histogram_quantile(0.5, rate({__name__="fixture.latency_bucket"}[4m]))`, 1},
	} {
		got := queryValue(tc.expr, end)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("%s = %g want %g", tc.expr, got, tc.want)
		}
		t.Logf("%s = %.12g", tc.expr, got)
	}
	params := url.Values{"query": {`{__name__="system.cpu.utilization"}`}, "start": {end.Add(-6 * 24 * time.Hour).Format(time.RFC3339Nano)}, "end": {end.Format(time.RFC3339Nano)}, "step": {"24h"}}
	before, err := c.Query(ctx, params, true)
	if err != nil {
		t.Fatal(err)
	}
	var history struct {
		Data struct {
			Result []struct {
				Values []json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(before, &history) != nil || len(history.Data.Result) != 1 || len(history.Data.Result[0].Values) != 7 {
		t.Fatalf("seven daily samples missing: %s", before)
	}
	stop()
	start()
	after, err := c.Query(ctx, params, true)
	if err != nil || string(before) != string(after) {
		t.Fatalf("restart changed history: err=%v before=%s after=%s", err, before, after)
	}
	names, err := c.MetricNames(ctx, url.Values{"start": params["start"], "end": params["end"], "match[]": {`{"host.id"="i-123"}`}})
	if err != nil || !strings.Contains(string(names), "system.cpu.utilization") {
		t.Fatalf("retained discovery: %s %v", names, err)
	}
	stale, err := c.Query(ctx, url.Values{"query": {`{__name__="system.cpu.utilization"}`}, "time": {end.Add(6 * time.Minute).Format(time.RFC3339Nano)}}, false)
	if err != nil || !strings.Contains(string(stale), `"result":[]`) {
		t.Fatalf("staleness: %s %v", stale, err)
	}
	t.Log(fmt.Sprintf("seven daily samples survived restart; discovery and staleness verified at %s", end.Format(time.RFC3339)))
}
