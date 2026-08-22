//go:build loadtest

package main

// Direct OTLP emission with ACK-latency measurement.
//
// The SDK-based producer in main.go cannot answer the question the #173
// release gate asks. Two reasons, both structural:
//
//   1. The SDK's BatchSpanProcessor exports asynchronously on its own
//      schedule, so the caller never observes the Export RPC round trip. In
//      aggregate mode that round trip IS the durable ACK — the server returns
//      only after the reduced deltas are in a committed transaction and
//      applied to the shards — so ACK p99 is exactly the client-side Export
//      latency and nothing else measures it.
//   2. The SDK producer sleeps for the simulated span duration inside its
//      emit loop, which caps a producer at roughly 1/mean(5..500ms) ~= 4
//      spans/s. 150 producers therefore top out near 600 points/s, not the
//      10,000 the gate requires.
//
// This file adds a second emission engine that talks the OTLP collector
// protobuf services directly, batches points per tick the way a real agent
// does, times every Export, and keeps per-phase latency samples for exact
// percentiles. It is selected with -direct (default on for the
// aggregate-acceptance profile).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	mrand "math/rand"

	"github.com/RandomCodeSpace/otelcontext/test/gate/gatecore"
)

// Measurement phases. Latency samples are bucketed by the phase in effect when
// the Export started, so the settle window never pollutes the sustained
// percentiles the gate is scored on.
const (
	phaseSettle    int32 = 0
	phaseSustained int32 = 1
	phaseBurst     int32 = 2
	phaseCount     int   = 3
)

var phaseNames = [phaseCount]string{"settle", "sustained", "burst"}

// currentPhase is read by every emitter on every tick.
var currentPhase atomic.Int32

// signalKind identifies which OTLP service an emitter drives.
type signalKind int

const (
	kindSpans signalKind = iota
	kindLogs
	kindMetrics
	kindCount int = 3
)

var signalNames = [kindCount]string{"spans", "logs", "metrics"}

// routes are the (method, route) pairs the synthetic services serve. They are
// derived from the existing operations pool so span names stay identical to
// the SDK producer's.
var routes = []struct {
	method string
	route  string
}{
	{"GET", "/api/items"},
	{"POST", "/api/orders"},
	{"GET", "/health"},
	{"GET", "/api/users"},
	{"POST", "/api/payments"},
}

// severityNumbers maps the existing severity strings onto OTLP severity
// numbers.
var severityNumbers = map[string]int32{
	"TRACE": 1,
	"DEBUG": 5,
	"INFO":  9,
	"WARN":  13,
	"ERROR": 17,
	"FATAL": 21,
}

// emitterResult is one emitter goroutine's contribution to the final report.
type emitterResult struct {
	kind signalKind
	// lat holds raw Export durations in nanoseconds, bucketed by phase.
	lat [phaseCount][]int64
	// reqOK, reqErr count Export calls by outcome, per phase.
	reqOK  [phaseCount]int64
	reqErr [phaseCount]int64
	// exhausted counts RESOURCE_EXHAUSTED refusals (the backpressure the
	// gate forbids at sustained load), unavailable counts UNAVAILABLE.
	exhausted   [phaseCount]int64
	unavailable [phaseCount]int64
	other       [phaseCount]int64
	// pointsSent counts points handed to Export; pointsAcked counts points in
	// Exports that returned nil.
	pointsSent  [phaseCount]int64
	pointsAcked [phaseCount]int64
	// firstErr is the first non-nil Export error, verbatim.
	firstErr string
}

// ackLedger is nil unless -ack-ledger was given. Every emitter records into
// it; gatecore.LedgerRecorder is safe for concurrent use.
var ackLedger *gatecore.LedgerRecorder

// live counters for the progress line.
var (
	liveAcked     [kindCount]atomic.Int64
	liveSent      [kindCount]atomic.Int64
	liveErrs      atomic.Int64
	liveExhausted atomic.Int64
	liveInflight  atomic.Int64
)

// directConfig is the emission engine's configuration.
type directConfig struct {
	endpoint   string
	tenantID   string
	insecure   bool
	services   int
	spanRate   float64 // per service, per second
	logRate    float64
	metricRate float64
	interval   time.Duration // batch tick
	burstMul   float64
	callTimout time.Duration
	// ledgerPath, when set, turns on the ACK ledger: a per-aggregate-window
	// record of contributions attempted versus contributions acknowledged,
	// fsynced every ledgerFlush so a copy predating a server crash always
	// exists on disk. The #202 recovery gate reads it; nothing else does.
	ledgerPath  string
	ledgerFlush time.Duration
}

// directEmitter drives one (service, signal) pair.
type directEmitter struct {
	svcIdx   int
	svcName  string
	instance string
	kind     signalKind
	rate     float64
	cfg      directConfig
	rng      *mrand.Rand

	traceClient  coltracepb.TraceServiceClient
	logClient    collogspb.LogsServiceClient
	metricClient colmetricspb.MetricsServiceClient

	res *resourcepb.Resource

	seq       int
	carry     float64
	startNano uint64 // metric start_time_unix_nano for cumulative points
	counter   int64  // cumulative request counter value
	resetCtr  int64  // cumulative counter that resets every ~5 minutes
	resetAt   time.Time

	out emitterResult
}

// newResource builds the per-service OTLP resource. service.instance.id is
// present so the aggregate engine derives a stable metric ProducerID (#166)
// rather than fragmenting cumulative baselines.
func newResource(svcName, instance string) *resourcepb.Resource {
	return &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			strAttr("service.name", svcName),
			strAttr("service.instance.id", instance),
		},
	}
}

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: v},
	}}
}

func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_IntValue{IntValue: v},
	}}
}

func randomID(n int) []byte {
	b := make([]byte, n)
	// crypto/rand is used only because it needs no seeding here; the IDs are
	// synthetic and carry no security meaning.
	_, _ = rand.Read(b)
	return b
}

// run is the emitter loop: one batch per tick, sized from the phase-adjusted
// rate, sent synchronously so the Export round trip is the measured ACK.
func (e *directEmitter) run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(e.cfg.interval)
	defer ticker.Stop()
	secs := e.cfg.interval.Seconds()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		ph := currentPhase.Load()
		mul := 1.0
		if ph == phaseBurst {
			mul = e.cfg.burstMul
		}
		e.carry += e.rate * mul * secs
		n := int(e.carry)
		if n <= 0 {
			continue
		}
		e.carry -= float64(n)

		var send func(context.Context) error
		// contrib is the batch's per-aggregate-window point counts, derived
		// from the timestamps the payload actually carries. The aggregate
		// engine picks a span's window from its START time, which is
		// backdated relative to this Export, so charging the whole batch to
		// the call's own window would misattribute spans at every boundary.
		var contrib gatecore.Contribution
		switch e.kind {
		case kindSpans:
			req, c := e.buildSpans(n)
			contrib = c
			send = func(c context.Context) error {
				_, err := e.traceClient.Export(c, req)
				return err
			}
		case kindLogs:
			req, c := e.buildLogs(n)
			contrib = c
			send = func(c context.Context) error {
				_, err := e.logClient.Export(c, req)
				return err
			}
		case kindMetrics:
			req, c := e.buildMetrics(n)
			contrib = c
			send = func(c context.Context) error {
				_, err := e.metricClient.Export(c, req)
				return err
			}
		}

		// The send context is deliberately NOT derived from ctx: run
		// teardown must not cancel an Export that is already in flight, or
		// the last tick of every emitter shows up as a spurious CANCELED
		// error and pollutes the error accounting the gate reads.
		callCtx := context.Background()
		var cancel context.CancelFunc
		if e.cfg.callTimout > 0 {
			callCtx, cancel = context.WithTimeout(callCtx, e.cfg.callTimout)
		}
		if e.cfg.tenantID != "" {
			callCtx = metadata.AppendToOutgoingContext(callCtx, "x-tenant-id", e.cfg.tenantID)
		}

		e.out.pointsSent[ph] += int64(n)
		liveSent[e.kind].Add(int64(n))
		liveInflight.Add(1)
		start := time.Now()
		// The attempt is recorded BEFORE the Export leaves, so a ledger
		// flushed mid-flight still bounds the crash interval from above.
		if ackLedger != nil {
			ackLedger.Attempt(contrib, signalNames[e.kind])
		}
		err := send(callCtx)
		elapsed := time.Since(start)
		liveInflight.Add(-1)
		if cancel != nil {
			cancel()
		}

		e.out.lat[ph] = append(e.out.lat[ph], elapsed.Nanoseconds())
		if err == nil {
			e.out.reqOK[ph]++
			e.out.pointsAcked[ph] += int64(n)
			liveAcked[e.kind].Add(int64(n))
			// The same Contribution the attempt used, so the two sides of
			// the bound can never land in different windows.
			if ackLedger != nil {
				ackLedger.Ack(contrib, signalNames[e.kind])
			}
			continue
		}

		e.out.reqErr[ph]++
		liveErrs.Add(1)
		if e.out.firstErr == "" {
			e.out.firstErr = err.Error()
		}
		switch grpcstatus.Code(err) {
		case codes.ResourceExhausted:
			e.out.exhausted[ph]++
			liveExhausted.Add(1)
		case codes.Unavailable:
			e.out.unavailable[ph]++
		default:
			e.out.other[ph]++
		}
	}
}

// buildSpans generates n spans grouped into short traces (a parent plus its
// children, matching the SDK producer's every-10th-span shape at batch scale).
func (e *directEmitter) buildSpans(n int) (*coltracepb.ExportTraceServiceRequest, gatecore.Contribution) {
	spans := make([]*tracepb.Span, 0, n)
	contrib := gatecore.Contribution{}
	var traceID, parentID []byte
	inTrace := 0
	now := time.Now()

	for i := 0; i < n; i++ {
		if inTrace == 0 {
			traceID = randomID(16)
			parentID = nil
			inTrace = 1 + e.rng.Intn(3) // 1-3 spans after the root
		}
		spanID := randomID(8)
		r := routes[e.seq%len(routes)]
		dur := e.randomDur()
		errored := isError(e.seq)
		end := now.Add(-time.Duration(e.rng.Intn(int(e.cfg.interval) + 1)))
		start := end.Add(-dur)

		status := &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
		httpStatus := int64(200)
		if errored {
			status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "simulated error"}
			httpStatus = 500
		}

		sp := &tracepb.Span{
			TraceId:           traceID,
			SpanId:            spanID,
			ParentSpanId:      parentID,
			Name:              r.method + " " + r.route,
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: uint64(start.UnixNano()), // #nosec G115 -- wall clock, always positive
			EndTimeUnixNano:   uint64(end.UnixNano()),   // #nosec G115 -- wall clock, always positive
			Status:            status,
			Attributes: []*commonpb.KeyValue{
				strAttr("http.request.method", r.method),
				strAttr("http.route", r.route),
				intAttr("http.response.status_code", httpStatus),
			},
		}
		// internal/aggregate windows a span by its START time.
		contrib.Add(start, gatecore.WindowSecs)
		spans = append(spans, sp)
		if parentID == nil {
			parentID = spanID
		}
		inTrace--
		e.seq++
	}

	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: e.res,
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: e.svcName},
				Spans: spans,
			}},
		}},
	}, contrib
}

func (e *directEmitter) randomDur() time.Duration {
	return time.Duration(5+e.rng.Intn(496)) * time.Millisecond
}

// buildLogs generates n log records using the existing template and severity
// mix helpers.
func (e *directEmitter) buildLogs(n int) (*collogspb.ExportLogsServiceRequest, gatecore.Contribution) {
	recs := make([]*logspb.LogRecord, 0, n)
	now := time.Now()
	contrib := gatecore.Contribution{}
	for i := 0; i < n; i++ {
		sev := pickSeverity(e.seq)
		body := e.generateLogBodyRNG(e.seq)
		recs = append(recs, &logspb.LogRecord{
			TimeUnixNano:   uint64(now.UnixNano()), // #nosec G115 -- wall clock, always positive
			SeverityNumber: logspb.SeverityNumber(severityNumbers[sev]),
			SeverityText:   sev,
			Body: &commonpb.AnyValue{
				Value: &commonpb.AnyValue_StringValue{StringValue: body},
			},
		})
		contrib.Add(now, gatecore.WindowSecs)
		e.seq++
	}
	return &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: e.res,
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope:      &commonpb.InstrumentationScope{Name: e.svcName},
				LogRecords: recs,
			}},
		}},
	}, contrib
}

// generateLogBodyRNG mirrors (*producer).generateLogBody against this
// emitter's RNG.
func (e *directEmitter) generateLogBodyRNG(seq int) string {
	template := logTemplates[seq%len(logTemplates)]
	var tokens []interface{}
	for i := 0; i < countPct(template); i++ {
		if i%2 == 0 {
			tokens = append(tokens, e.rng.Intn(10000)) // NOSONAR go:S2245 -- synthetic load data
		} else {
			tokens = append(tokens, fmt.Sprintf("id-%d", e.rng.Intn(1000))) // NOSONAR go:S2245 -- synthetic load data
		}
	}
	return fmt.Sprintf(template, tokens...)
}

func countPct(s string) int {
	c := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '%' {
			c++
		}
	}
	return c
}

// buildMetrics generates n data points spread across the three instruments the
// SDK producer publishes: a cumulative monotonic request counter, a queue-depth
// gauge, and a cumulative counter that resets every ~5 minutes.
func (e *directEmitter) buildMetrics(n int) (*colmetricspb.ExportMetricsServiceRequest, gatecore.Contribution) {
	wallNow := time.Now()
	now := uint64(wallNow.UnixNano()) // #nosec G115 -- wall clock, always positive
	var counterPts, gaugePts, resetPts []*metricspb.NumberDataPoint

	if time.Since(e.resetAt) > 5*time.Minute {
		e.resetCtr = 0
		e.resetAt = time.Now()
		e.startNano = now
	}

	for i := 0; i < n; i++ {
		switch e.seq % 3 {
		case 0:
			e.counter++
			counterPts = append(counterPts, &metricspb.NumberDataPoint{
				StartTimeUnixNano: e.startNano,
				TimeUnixNano:      now,
				Value:             &metricspb.NumberDataPoint_AsInt{AsInt: e.counter},
			})
		case 1:
			gaugePts = append(gaugePts, &metricspb.NumberDataPoint{
				TimeUnixNano: now,
				Value:        &metricspb.NumberDataPoint_AsInt{AsInt: int64(e.rng.Intn(100))},
			})
		default:
			e.resetCtr++
			resetPts = append(resetPts, &metricspb.NumberDataPoint{
				StartTimeUnixNano: e.startNano,
				TimeUnixNano:      now,
				Value:             &metricspb.NumberDataPoint_AsInt{AsInt: e.resetCtr},
			})
		}
		e.seq++
	}
	contrib := gatecore.Contribution{}
	for i := 0; i < n; i++ {
		contrib.Add(wallNow, gatecore.WindowSecs)
	}

	var ms []*metricspb.Metric
	cumulative := metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	if len(counterPts) > 0 {
		ms = append(ms, &metricspb.Metric{
			Name: "http.server.request.count",
			Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				DataPoints:             counterPts,
				AggregationTemporality: cumulative,
				IsMonotonic:            true,
			}},
		})
	}
	if len(gaugePts) > 0 {
		ms = append(ms, &metricspb.Metric{
			Name: "queue.depth",
			Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: gaugePts}},
		})
	}
	if len(resetPts) > 0 {
		ms = append(ms, &metricspb.Metric{
			Name: "custom.reset.counter",
			Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				DataPoints:             resetPts,
				AggregationTemporality: cumulative,
				IsMonotonic:            true,
			}},
		})
	}

	return &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: e.res,
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Scope:   &commonpb.InstrumentationScope{Name: e.svcName},
				Metrics: ms,
			}},
		}},
	}, contrib
}

// -------------------------------------------------------------------------
// Percentiles and the JSON report
// -------------------------------------------------------------------------

// phaseLatency is the latency summary of one (phase, signal) pair.
type phaseLatency struct {
	Samples int64   `json:"samples"`
	MinMs   float64 `json:"min_ms"`
	P50Ms   float64 `json:"p50_ms"`
	P90Ms   float64 `json:"p90_ms"`
	P95Ms   float64 `json:"p95_ms"`
	P99Ms   float64 `json:"p99_ms"`
	P999Ms  float64 `json:"p999_ms"`
	MaxMs   float64 `json:"max_ms"`
	MeanMs  float64 `json:"mean_ms"`
}

// phaseReport is one phase's full accounting.
type phaseReport struct {
	Phase        string                  `json:"phase"`
	DurationSec  float64                 `json:"duration_sec"`
	All          phaseLatency            `json:"ack_latency_all_signals"`
	BySignal     map[string]phaseLatency `json:"ack_latency_by_signal"`
	PointsSent   int64                   `json:"points_sent"`
	PointsAcked  int64                   `json:"points_acked"`
	PointsPerSec float64                 `json:"points_acked_per_sec"`
	RequestsOK   int64                   `json:"requests_ok"`
	RequestsErr  int64                   `json:"requests_err"`
	Exhausted    int64                   `json:"resource_exhausted"`
	Unavailable  int64                   `json:"unavailable"`
	OtherErrors  int64                   `json:"other_errors"`
}

// runReport is the whole run, written as JSON for the gate scoring.
type runReport struct {
	StartedAt string                 `json:"started_at"`
	EndedAt   string                 `json:"ended_at"`
	Config    map[string]interface{} `json:"config"`
	Phases    []phaseReport          `json:"phases"`
	FirstErr  string                 `json:"first_error,omitempty"`
}

// percentile returns the p-th percentile (0..1) of a sorted nanosecond slice,
// in milliseconds, using nearest-rank.
func percentile(sorted []int64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return float64(sorted[idx]) / 1e6
}

func summarize(samples []int64) phaseLatency {
	if len(samples) == 0 {
		return phaseLatency{}
	}
	s := make([]int64, len(samples))
	copy(s, samples)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum int64
	for _, v := range s {
		sum += v
	}
	return phaseLatency{
		Samples: int64(len(s)),
		MinMs:   float64(s[0]) / 1e6,
		P50Ms:   percentile(s, 0.50),
		P90Ms:   percentile(s, 0.90),
		P95Ms:   percentile(s, 0.95),
		P99Ms:   percentile(s, 0.99),
		P999Ms:  percentile(s, 0.999),
		MaxMs:   float64(s[len(s)-1]) / 1e6,
		MeanMs:  float64(sum) / float64(len(s)) / 1e6,
	}
}

// buildReport folds every emitter's result into the per-phase report.
func buildReport(results []emitterResult, phaseDur [phaseCount]time.Duration, cfg map[string]interface{}, started, ended time.Time) runReport {
	rep := runReport{
		StartedAt: started.UTC().Format(time.RFC3339Nano),
		EndedAt:   ended.UTC().Format(time.RFC3339Nano),
		Config:    cfg,
	}
	for ph := 0; ph < phaseCount; ph++ {
		var all []int64
		bySignal := make(map[string][]int64)
		pr := phaseReport{Phase: phaseNames[ph], DurationSec: phaseDur[ph].Seconds()}
		for i := range results {
			r := &results[i]
			all = append(all, r.lat[ph]...)
			name := signalNames[r.kind]
			bySignal[name] = append(bySignal[name], r.lat[ph]...)
			pr.PointsSent += r.pointsSent[ph]
			pr.PointsAcked += r.pointsAcked[ph]
			pr.RequestsOK += r.reqOK[ph]
			pr.RequestsErr += r.reqErr[ph]
			pr.Exhausted += r.exhausted[ph]
			pr.Unavailable += r.unavailable[ph]
			pr.OtherErrors += r.other[ph]
			if rep.FirstErr == "" && r.firstErr != "" {
				rep.FirstErr = r.firstErr
			}
		}
		pr.All = summarize(all)
		pr.BySignal = make(map[string]phaseLatency, len(bySignal))
		for k, v := range bySignal {
			pr.BySignal[k] = summarize(v)
		}
		if pr.DurationSec > 0 {
			pr.PointsPerSec = float64(pr.PointsAcked) / pr.DurationSec
		}
		rep.Phases = append(rep.Phases, pr)
	}
	return rep
}

func writeReport(path string, rep runReport) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

// -------------------------------------------------------------------------
// Engine entry point
// -------------------------------------------------------------------------

// runDirect drives the whole measured run: settle, sustained, burst. It returns
// the finished report.
func runDirect(ctx context.Context, cfg directConfig, settle, sustained, burst time.Duration, reportPath string) (runReport, error) {
	dialOpts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(16 << 20)),
	}
	if cfg.insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conns := make([]*grpc.ClientConn, cfg.services)
	emitters := make([]*directEmitter, 0, cfg.services*kindCount)
	for i := 0; i < cfg.services; i++ {
		cc, err := grpc.NewClient(cfg.endpoint, dialOpts...)
		if err != nil {
			return runReport{}, fmt.Errorf("service %d dial: %w", i, err)
		}
		conns[i] = cc
		svc := serviceName(i)
		inst := fmt.Sprintf("%s-instance-0", svc)
		res := newResource(svc, inst)
		now := uint64(time.Now().UnixNano()) // #nosec G115 -- wall clock, always positive
		for k, rate := range map[signalKind]float64{
			kindSpans:   cfg.spanRate,
			kindLogs:    cfg.logRate,
			kindMetrics: cfg.metricRate,
		} {
			if rate <= 0 {
				continue
			}
			e := &directEmitter{
				svcIdx:       i,
				svcName:      svc,
				instance:     inst,
				kind:         k,
				rate:         rate,
				cfg:          cfg,
				rng:          mrand.New(mrand.NewSource(time.Now().UnixNano() + int64(i)*13 + int64(k))), // NOSONAR go:S2245 -- synthetic load data
				traceClient:  coltracepb.NewTraceServiceClient(cc),
				logClient:    collogspb.NewLogsServiceClient(cc),
				metricClient: colmetricspb.NewMetricsServiceClient(cc),
				res:          res,
				startNano:    now,
				resetAt:      time.Now(),
			}
			e.out.kind = k
			emitters = append(emitters, e)
		}
	}
	defer func() {
		for _, cc := range conns {
			if cc != nil {
				_ = cc.Close()
			}
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if cfg.ledgerPath != "" {
		flush := cfg.ledgerFlush
		if flush <= 0 {
			flush = 2 * time.Second
		}
		ackLedger = gatecore.NewLedgerRecorder(gatecore.WindowSecs, flush, time.Now())
		go flushLedgerLoop(runCtx, cfg.ledgerPath, flush)
	}

	var wg sync.WaitGroup
	currentPhase.Store(phaseSettle)
	started := time.Now()

	// Stagger emitter start over the settle window so 450 goroutines do not
	// all fire their first Export in the same millisecond.
	stagger := time.Duration(0)
	if len(emitters) > 1 && settle > 0 {
		stagger = settle / 4 / time.Duration(len(emitters))
	}
	for _, e := range emitters {
		wg.Add(1)
		go e.run(runCtx, &wg)
		if stagger > 0 {
			time.Sleep(stagger)
		}
	}

	progressCtx, stopProgress := context.WithCancel(runCtx)
	go directProgress(progressCtx)

	var phaseDur [phaseCount]time.Duration

	remainingSettle := settle - time.Since(started)
	if remainingSettle > 0 {
		sleepCtx(runCtx, remainingSettle)
	}
	phaseDur[phaseSettle] = time.Since(started)

	fmt.Printf("=== phase: sustained (%s) ===\n", sustained)
	t := time.Now()
	currentPhase.Store(phaseSustained)
	sleepCtx(runCtx, sustained)
	phaseDur[phaseSustained] = time.Since(t)

	if burst > 0 {
		fmt.Printf("=== phase: burst %.1fx (%s) ===\n", cfg.burstMul, burst)
		t = time.Now()
		currentPhase.Store(phaseBurst)
		sleepCtx(runCtx, burst)
		phaseDur[phaseBurst] = time.Since(t)
	}

	stopProgress()
	cancel()
	wg.Wait()
	ended := time.Now()

	results := make([]emitterResult, 0, len(emitters))
	for _, e := range emitters {
		results = append(results, e.out)
	}

	cfgMap := map[string]interface{}{
		"endpoint":            cfg.endpoint,
		"services":            cfg.services,
		"span_rate_per_svc":   cfg.spanRate,
		"log_rate_per_svc":    cfg.logRate,
		"metric_rate_per_svc": cfg.metricRate,
		"batch_interval_ms":   cfg.interval.Milliseconds(),
		"burst_multiplier":    cfg.burstMul,
		"call_timeout_ms":     cfg.callTimout.Milliseconds(),
		"settle_sec":          settle.Seconds(),
		"sustained_sec":       sustained.Seconds(),
		"burst_sec":           burst.Seconds(),
	}
	if ackLedger != nil {
		if err := gatecore.WriteLedger(cfg.ledgerPath, ackLedger.Snapshot(time.Now(), true)); err != nil {
			return runReport{}, fmt.Errorf("write final ack ledger: %w", err)
		}
		fmt.Printf("ack ledger written to %s\n", cfg.ledgerPath)
	}

	rep := buildReport(results, phaseDur, cfgMap, started, ended)
	if reportPath != "" {
		if err := writeReport(reportPath, rep); err != nil {
			return rep, fmt.Errorf("write report: %w", err)
		}
	}
	printDirectSummary(rep)
	return rep, nil
}

// flushLedgerLoop fsyncs the ledger on an interval so a copy that predates a
// server kill -9 is always on the platter, whatever happens to this process.
func flushLedgerLoop(ctx context.Context, path string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := gatecore.WriteLedger(path, ackLedger.Snapshot(time.Now(), false)); err != nil {
				fmt.Printf("ack ledger flush failed: %v\n", err)
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func directProgress(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	start := time.Now()
	var prev int64
	prevT := start
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		var total int64
		for i := 0; i < kindCount; i++ {
			total += liveAcked[i].Load()
		}
		dt := now.Sub(prevT).Seconds()
		rate := float64(total-prev) / dt
		prev, prevT = total, now
		fmt.Printf("[T+%4.0fs][%s] acked spans=%d logs=%d metrics=%d | rate=%.0f pts/s | inflight=%d errors=%d exhausted=%d\n",
			now.Sub(start).Seconds(), phaseNames[currentPhase.Load()],
			liveAcked[kindSpans].Load(), liveAcked[kindLogs].Load(), liveAcked[kindMetrics].Load(),
			rate, liveInflight.Load(), liveErrs.Load(), liveExhausted.Load())
	}
}

func printDirectSummary(rep runReport) {
	fmt.Println("-------------------------------------------------------------")
	for _, p := range rep.Phases {
		if p.All.Samples == 0 {
			continue
		}
		fmt.Printf("phase=%-9s dur=%6.1fs  acked=%d pts (%.0f pts/s)  sent=%d\n",
			p.Phase, p.DurationSec, p.PointsAcked, p.PointsPerSec, p.PointsSent)
		fmt.Printf("  ACK latency (all signals): n=%d p50=%.1fms p90=%.1fms p99=%.1fms p99.9=%.1fms max=%.1fms\n",
			p.All.Samples, p.All.P50Ms, p.All.P90Ms, p.All.P99Ms, p.All.P999Ms, p.All.MaxMs)
		for _, name := range signalNames {
			if l, ok := p.BySignal[name]; ok && l.Samples > 0 {
				fmt.Printf("    %-8s n=%d p50=%.1fms p99=%.1fms max=%.1fms\n",
					name, l.Samples, l.P50Ms, l.P99Ms, l.MaxMs)
			}
		}
		fmt.Printf("  requests ok=%d err=%d resource_exhausted=%d unavailable=%d other=%d\n",
			p.RequestsOK, p.RequestsErr, p.Exhausted, p.Unavailable, p.OtherErrors)
	}
	if rep.FirstErr != "" {
		fmt.Printf("first error: %s\n", rep.FirstErr)
	}
	fmt.Println("-------------------------------------------------------------")
}
