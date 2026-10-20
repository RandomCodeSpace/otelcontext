package prometheus

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"gorm.io/gorm"
)

func localDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	t.Setenv("LOG_FTS_ENABLED", "false")
	db, err := storage.NewDatabase("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := migrate.Up(context.Background(), db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	return db
}
func localFor(t *testing.T, db *gorm.DB, tenant string) *Local {
	t.Helper()
	l, err := NewLocal(db, Config{TenantID: tenant}, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func attribute(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}
func number(t time.Time, value float64) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{TimeUnixNano: uint64(t.UnixNano()), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: value}}
}
func gauge(name string, points ...*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: points}}}
}
func localRequest(host string, metrics ...*metricspb.Metric) *colmetricspb.ExportMetricsServiceRequest {
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		attribute("service.name", "checkout"), attribute("host.name", host), attribute("service.instance.id", host),
	}}, ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: metrics}}}}}
}
func localExport(t *testing.T, l *Local, req *colmetricspb.ExportMetricsServiceRequest) {
	t.Helper()
	if _, err := l.Export(storage.WithTenantContext(context.Background(), l.TenantID()), req); err != nil {
		t.Fatal(err)
	}
}
func localQuery(t *testing.T, l *Local, expr string, at time.Time) map[string]any {
	t.Helper()
	body, err := l.Query(storage.WithTenantContext(context.Background(), l.TenantID()), url.Values{"query": {expr}, "time": {at.Format(time.RFC3339Nano)}}, false)
	if err != nil {
		t.Fatalf("query %s: %v", expr, err)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result["data"].(map[string]any)
}
func localValue(t *testing.T, l *Local, expr string, at time.Time) float64 {
	t.Helper()
	data := localQuery(t, l, expr, at)
	result := data["result"].([]any)
	if len(result) != 1 {
		t.Fatalf("expected one result: %#v", data)
	}
	text := result[0].(map[string]any)["value"].([]any)[1].(string)
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLocalPromQLPersistenceIsolationAndLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.db")
	db := localDB(t, path)
	alpha, beta := localFor(t, db, "alpha"), localFor(t, db, "beta")
	now := time.Now().Truncate(time.Second)
	localExport(t, alpha, localRequest("a", gauge("system.cpu.utilization", number(now, .25))))
	localExport(t, alpha, localRequest("b", gauge("system.cpu.utilization", number(now, .75))))
	localExport(t, beta, localRequest("a", gauge("system.cpu.utilization", number(now, 99))))
	expr := `sum({__name__="system.cpu.utilization"})`
	if v := localValue(t, alpha, expr, now); v != 1 {
		t.Fatalf("tenant sum=%v", v)
	}
	if v := localValue(t, alpha, `{__name__="system.cpu.utilization","host.name"="a"}`, now); v != .25 {
		t.Fatal(v)
	}
	if v := localValue(t, alpha, `{__name__="system.cpu.utilization","service.name"="checkout","service.instance.id"="b"}`, now); v != .75 {
		t.Fatalf("same-service instance selection=%v", v)
	}
	if v := localValue(t, beta, expr, now); v != 99 {
		t.Fatal(v)
	}
	if _, err := alpha.Query(storage.WithTenantContext(context.Background(), "beta"), url.Values{"query": {expr}}, false); err == nil {
		t.Fatal("cross-tenant query allowed")
	}
	if _, err := alpha.Export(storage.WithTenantContext(context.Background(), "beta"), localRequest("a", gauge("x", number(now, 1)))); err == nil {
		t.Fatal("cross-tenant export allowed")
	}
	conn, _ := db.DB()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.NewDatabase("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	conn, _ = reopened.DB()
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := migrate.RequireExact(context.Background(), reopened, "sqlite"); err != nil {
		t.Fatal(err)
	}
	again := localFor(t, reopened, "alpha")
	if v := localValue(t, again, expr, now); v != 1 {
		t.Fatalf("restart sum=%v", v)
	}
	names, err := again.MetricNames(storage.WithTenantContext(context.Background(), "alpha"), url.Values{"start": {now.Add(-time.Minute).Format(time.RFC3339Nano)}, "end": {now.Format(time.RFC3339Nano)}, "match[]": {`{"system.cpu.utilization","host.name"="a"}`}})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct{ Data []string }
	if err := json.Unmarshal(names, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data) != 1 || envelope.Data[0] != "system.cpu.utilization" {
		t.Fatalf("names=%s", names)
	}
}

func TestLocalPromQLCumulativeStartTimeReset(t *testing.T) {
	l := localFor(t, localDB(t, filepath.Join(t.TempDir(), "main.db")), "default")
	now := time.Now().Truncate(time.Second)
	before, after := number(now.Add(-time.Minute), 10), number(now, 20)
	before.StartTimeUnixNano = uint64(now.Add(-10 * time.Minute).UnixNano())
	after.StartTimeUnixNano = uint64(now.Add(-30 * time.Second).UnixNano())
	counter := &metricspb.Metric{Name: "counter_total", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
		IsMonotonic:            true,
		AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
		DataPoints:             []*metricspb.NumberDataPoint{before, after},
	}}}
	localExport(t, l, localRequest("a", counter))
	if got := localValue(t, l, "resets(counter_total[3m])", now); got != 1 {
		t.Fatalf("counter start-time reset missed: got %v, want 1", got)
	}
}
func TestLocalPromQLCounterResetRangeAndStaleness(t *testing.T) {
	l := localFor(t, localDB(t, filepath.Join(t.TempDir(), "main.db")), "default")
	now := time.Now().Truncate(time.Second)
	counter := &metricspb.Metric{Name: "requests_total", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{IsMonotonic: true, AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricspb.NumberDataPoint{
		number(now.Add(-120*time.Second), 100), number(now.Add(-60*time.Second), 120), number(now, 5),
	}}}}
	req := localRequest("a", counter)
	localExport(t, l, req)
	localExport(t, l, req) // Retry must not double count.
	if v := localValue(t, l, "resets(requests_total[3m])", now); v != 1 {
		t.Fatal(v)
	}
	if v := localValue(t, l, "rate(requests_total[3m])", now); math.Abs(v-25.0/120) > 1e-12 {
		t.Fatalf("rate=%v", v)
	}
	data, err := l.Query(context.Background(), url.Values{"query": {"requests_total"}, "start": {now.Add(-120 * time.Second).Format(time.RFC3339Nano)}, "end": {now.Format(time.RFC3339Nano)}, "step": {"60"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct{ Result []struct{ Values [][]any } }
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Result) != 1 || len(envelope.Data.Result[0].Values) != 3 {
		t.Fatalf("range=%s", data)
	}
	stale := number(now.Add(time.Second), 0)
	stale.Flags = 1
	localExport(t, l, localRequest("a", gauge("temperature", number(now, 42), stale)))
	if v := localValue(t, l, "temperature", now); v != 42 {
		t.Fatal(v)
	}
	if got := localQuery(t, l, "temperature", now.Add(time.Second))["result"].([]any); len(got) != 0 {
		t.Fatalf("stale sample returned: %#v", got)
	}
}

func TestLocalPromQLHistograms(t *testing.T) {
	l := localFor(t, localDB(t, filepath.Join(t.TempDir(), "main.db")), "default")
	now := time.Now().Truncate(time.Second)
	sum := 17.0
	classic := &metricspb.Metric{Name: "latency", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricspb.HistogramDataPoint{{
		TimeUnixNano: uint64(now.UnixNano()), Count: 10, Sum: &sum, ExplicitBounds: []float64{1, 2}, BucketCounts: []uint64{2, 3, 5},
	}}}}}
	exp := &metricspb.Metric{Name: "native_latency", Data: &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricspb.ExponentialHistogramDataPoint{{
		TimeUnixNano: uint64(now.UnixNano()), Count: 5, Sum: &sum, Scale: 0, Positive: &metricspb.ExponentialHistogramDataPoint_Buckets{Offset: 0, BucketCounts: []uint64{2, 3}},
	}}}}}
	localExport(t, l, localRequest("a", classic, exp))
	for expr, want := range map[string]float64{`latency_count`: 10, `latency_sum`: 17, `latency_bucket{le="+Inf"}`: 10, `histogram_quantile(0.5,latency_bucket)`: 2, `histogram_count(native_latency)`: 5, `histogram_sum(native_latency)`: 17} {
		if v := localValue(t, l, expr, now); v != want {
			t.Fatalf("%s=%v want %v", expr, v, want)
		}
	}
}

func TestLocalPromQLRejectsAtomicallyAndBoundsQueries(t *testing.T) {
	db := localDB(t, filepath.Join(t.TempDir(), "main.db"))
	l := localFor(t, db, "default")
	now := time.Now().Truncate(time.Second)
	localExport(t, l, localRequest("a", gauge("g", number(now, 1))))
	bad := localRequest("a", gauge("new", number(now, 9)), gauge("g", number(now, 2)))
	if _, err := l.Export(context.Background(), bad); err == nil {
		t.Fatal("conflicting retry accepted")
	}
	if got := localQuery(t, l, "new", now)["result"].([]any); len(got) != 0 {
		t.Fatal("partial transaction committed")
	}
	delta := &metricspb.Metric{Name: "d", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{IsMonotonic: true, AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, DataPoints: []*metricspb.NumberDataPoint{number(now, 1)}}}}
	if _, err := l.Export(context.Background(), localRequest("a", gauge("new", number(now, 9)), delta)); err == nil {
		t.Fatal("delta accepted")
	}
	if got := localQuery(t, l, "new", now)["result"].([]any); len(got) != 0 {
		t.Fatal("conversion partially committed")
	}
	for _, params := range []url.Values{
		{"query": {"g"}, "start": {"0"}, "end": {"99999999"}, "step": {"1"}},
		{"query": {"g"}, "start": {"1"}, "end": {"2"}, "step": {"NaN"}},
		{"query": {"g"}, "start": {"1"}, "end": {"100"}, "step": {"0.001"}},
	} {
		if _, err := l.Query(context.Background(), params, true); err == nil {
			t.Fatalf("invalid bounds accepted: %v", params)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Query(ctx, url.Values{"query": {"g"}}, false); err == nil {
		t.Fatal("canceled query succeeded")
	}
	narrow, err := NewLocal(db, Config{MaxSeries: 1}, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	localExport(t, l, localRequest("b", gauge("g", number(now, 2))))
	if _, err := narrow.Query(context.Background(), url.Values{"query": {"sum(g)"}, "time": {now.Format(time.RFC3339Nano)}}, false); err == nil {
		t.Fatal("input series limit bypassed by aggregation")
	}
}
func TestLocalPromQLRetention(t *testing.T) {
	db := localDB(t, filepath.Join(t.TempDir(), "main.db"))
	l := localFor(t, db, "default")
	now := time.Now().Truncate(time.Second)
	l.now = func() time.Time { return now }
	localExport(t, l, localRequest("a", gauge("g", number(now.Add(-6*24*time.Hour), 1), number(now, 2)), gauge("expired", number(now.Add(-6*24*time.Hour), 9))))
	l.now = func() time.Time { return now.Add(2 * 24 * time.Hour) }
	if got := localQuery(t, l, "expired", now.Add(-6*24*time.Hour))["result"].([]any); len(got) != 0 {
		t.Fatal("expired history returned before purge")
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	if n, err := repo.PurgePromQLBatched(context.Background(), l.now().Add(-7*24*time.Hour), 1, 0); err != nil || n != 3 {
		t.Fatalf("purge rows=%d err=%v", n, err)
	}
	if v := localValue(t, l, "g", now); v != 2 {
		t.Fatal(v)
	}
	var rows int64
	db.Model(&storage.PromQLSeries{}).Count(&rows)
	if rows != 1 {
		t.Fatalf("series=%d", rows)
	}
}

func TestLocalPromQLReadinessDeadlineAndPayloadBounds(t *testing.T) {
	db := localDB(t, filepath.Join(t.TempDir(), "main.db"))
	l := localFor(t, db, "default")
	now := time.Now().Truncate(time.Second)
	localExport(t, l, localRequest("a", gauge("g", number(now, 1))))
	if err := l.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	held, err := conn.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err = l.Query(ctx, url.Values{"query": {"g"}}, false)
	cancel()
	_ = held.Close()
	if err == nil {
		t.Fatal("query ignored deadline waiting for the DB connection")
	}
	tiny, err := NewLocal(db, Config{MaxResponseBytes: 16}, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := tiny.Query(context.Background(), url.Values{"query": {"g"}}, false); err == nil || body != nil {
		t.Fatal("response size cap did not fail closed")
	}
	// Reject an oversized on-disk payload before protobuf decoding. This also
	// proves corruption cannot produce a plausible partial sum.
	if err := db.Exec("UPDATE promql_samples SET payload = zeroblob(?)", localMaxSampleBytes+1).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := l.Query(context.Background(), url.Values{"query": {"sum(g)"}}, false); err == nil {
		t.Fatal("oversized payload accepted")
	}
	if err := db.Exec("DROP TABLE promql_samples").Error; err != nil {
		t.Fatal(err)
	}
	if err := l.Ready(context.Background()); err == nil {
		t.Fatal("readiness ignored missing sample table")
	}
}
