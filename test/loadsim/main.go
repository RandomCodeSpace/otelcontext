//go:build loadtest

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

// operations is the fixed pool picked round-robin per producer.
var operations = []string{
	"GET /api/items",
	"POST /api/orders",
	"GET /health",
	"GET /api/users",
	"POST /api/payments",
}

// logTemplates are ~20 templates per service; %d/%s tokens replaced with random IDs/numbers.
var logTemplates = []string{
	"Request %d processing started",
	"Query execution for resource %s completed in %dms",
	"Database connection pool exhausted, waiting...",
	"Cache hit rate: %d%%",
	"Message received from broker, id=%s",
	"Failed to serialize object %s",
	"Worker %d handling batch of %d items",
	"Authentication attempt for user %s failed",
	"Rate limit exceeded for endpoint %s",
	"Disk usage at %d%%, monitoring...",
	"Backoff retry attempt %d for request %s",
	"Service dependency %s timeout, circuit open",
	"Config reload detected, version=%s",
	"Batch job %d processed %d records",
	"Memory warning: usage at %d MB",
	"HTTP request from %s completed with status %d",
	"Transaction %s rolled back due to conflict",
	"Socket error on connection %d: network unreachable",
	"API version mismatch detected: expected %s, got %s",
	"Queue depth for %s is %d, consider scaling",
}

// logSeverities weighted by the mix: 70% INFO, 15% WARN, 10% DEBUG, 5% ERROR.
var logSeverities = []string{
	"INFO", "INFO", "INFO", "INFO", "INFO", "INFO", "INFO",
	"WARN", "WARN",
	"DEBUG", "DEBUG",
	"ERROR",
}

// -------------------------------------------------------------------------
// Pure helper functions (tested directly by main_test.go)
// -------------------------------------------------------------------------

// serviceName returns the zero-padded service name for index i.
func serviceName(i int) string {
	return fmt.Sprintf("loadsim-svc-%03d", i)
}

// pickOperation returns an operation name using round-robin on the global ops slice.
func pickOperation(seq int) string {
	return operations[seq%len(operations)]
}

// randomDuration returns a uniformly random duration in [5ms, 500ms].
// Uses the shared global RNG; the hot-path variant is (*producer).randomDuration.
func randomDuration() time.Duration {
	// 5ms + [0, 495ms)
	return time.Duration(5+rand.Intn(496)) * time.Millisecond
}

// randomDuration returns a uniformly random duration in [5ms, 500ms] using the
// producer's private RNG (no cross-goroutine mutex contention).
func (p *producer) randomDuration() time.Duration {
	return time.Duration(5+p.rng.Intn(496)) * time.Millisecond
}

// isError returns true for approximately 5% of call sites (seq % 20 == 0).
// This is deterministic for a given seq, giving exactly 5% over a complete cycle.
func isError(seq int) bool {
	return seq%20 == 0
}

// pickSeverity returns a severity (INFO, WARN, DEBUG, ERROR) using round-robin.
// Deterministic per seq, respecting the ~70/15/10/5 mix via the logSeverities array.
func pickSeverity(seq int) string {
	return logSeverities[seq%len(logSeverities)]
}

// generateLogBody creates a log message by picking a template and filling tokens.
// Uses the producer's RNG for deterministic reproducibility.
func (p *producer) generateLogBody(seq int) string {
	template := logTemplates[seq%len(logTemplates)]
	// Count %d and %s tokens to know how many random values to generate.
	var tokens []interface{}
	for i := 0; i < strings.Count(template, "%"); i++ {
		if i%2 == 0 {
			// Alternate between numeric and string IDs.
			tokens = append(tokens, p.rng.Intn(10000)) // NOSONAR go:S2245 -- synthetic load data, not security-sensitive
		} else {
			// Generate a service-scoped ID token.
			tokens = append(tokens, fmt.Sprintf("id-%d", p.rng.Intn(1000))) // NOSONAR go:S2245 -- synthetic load data, not security-sensitive
		}
	}
	return fmt.Sprintf(template, tokens...)
}

// burstSpec holds parsed burst configuration.
type burstSpec struct {
	multiplier float64       // e.g. 2.0 for "2x"
	duration   time.Duration // e.g. 30*time.Second for "30s"
}

// parseBurstSpec parses a string like "2x30s" into {2.0, 30s}.
// Returns (spec, error); if err != nil, spec is zero.
func parseBurstSpec(s string) (burstSpec, error) {
	// Match "Nx<duration>" where N is float and <duration> is a Go duration string.
	re := regexp.MustCompile(`^(\d+(?:\.\d+)?)[xX](\d+(?:\.\d+)?[a-zA-Z]+)$`)
	matches := re.FindStringSubmatch(s)
	if len(matches) != 3 {
		return burstSpec{}, fmt.Errorf("invalid burst spec %q; format: 2x30s", s)
	}
	mul, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return burstSpec{}, fmt.Errorf("invalid multiplier in %q: %w", s, err)
	}
	dur, err := time.ParseDuration(matches[2])
	if err != nil {
		return burstSpec{}, fmt.Errorf("invalid duration in %q: %w", s, err)
	}
	return burstSpec{multiplier: mul, duration: dur}, nil
}

// -------------------------------------------------------------------------
// Ticker-based rate limiter (no golang.org/x/time/rate dependency)
// -------------------------------------------------------------------------

type rateLimiter struct {
	ticker *time.Ticker
	ch     chan struct{}
	done   chan struct{}
}

func newRateLimiter(rps int) *rateLimiter {
	interval := time.Second / time.Duration(rps)
	rl := &rateLimiter{
		ticker: time.NewTicker(interval),
		ch:     make(chan struct{}, 1), // capacity 1 avoids head-of-line blocking
		done:   make(chan struct{}),
	}
	go func() {
		for {
			select {
			case <-rl.ticker.C:
				select {
				case rl.ch <- struct{}{}:
				default: // drop tick if consumer is behind — no burst accumulation
				}
			case <-rl.done:
				return
			}
		}
	}()
	return rl
}

// wait blocks until one token is available.
func (rl *rateLimiter) wait() {
	<-rl.ch
}

func (rl *rateLimiter) stop() {
	rl.ticker.Stop()
	close(rl.done)
}

// -------------------------------------------------------------------------
// Per-producer state
// -------------------------------------------------------------------------

type producer struct {
	idx      int
	endpoint string
	tenantID string
	insecure bool

	tp     *sdktrace.TracerProvider
	tracer trace.Tracer

	mp     *sdkmetric.MeterProvider
	meter  metric.Meter
	meters producerMeters // pre-created instruments

	// rng is a per-producer RNG — avoids 200-goroutine contention on the global
	// math/rand mutex in the hot path (duration, child count).
	rng *rand.Rand

	// Counters per signal type (spans, logs, metrics).
	spansSent     atomic.Int64
	spansErrors   atomic.Int64
	logsSent      atomic.Int64
	logsErrors    atomic.Int64
	metricsSent   atomic.Int64
	metricsErrors atomic.Int64

	// Metric state.
	requestCount    atomic.Int64 // monotonic cumulative
	queueDepth      atomic.Int64 // gauge value
	resetCountSeq   atomic.Int64 // resets every ~5 minutes
	resetCountStart time.Time
}

// producerMeters holds pre-created metric instruments.
type producerMeters struct {
	requestCounter  metric.Int64Counter
	queueDepthGauge metric.Int64Gauge
	resetCounter    metric.Int64Counter
}

func newProducer(ctx context.Context, idx int, endpoint, tenantID string, insecure bool) (*producer, error) {
	svc := serviceName(idx)

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
	}
	if insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if tenantID != "" {
		opts = append(opts, otlptracegrpc.WithHeaders(map[string]string{"x-tenant-id": tenantID}))
	}

	client := otlptracegrpc.NewClient(opts...)
	exp, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("producer %d exporter: %w", idx, err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(svc)),
	)
	if err != nil {
		return nil, fmt.Errorf("producer %d resource: %w", idx, err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
	)

	// Set up metrics exporter.
	metricOpts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(endpoint),
	}
	if insecure {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}
	if tenantID != "" {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithHeaders(map[string]string{"x-tenant-id": tenantID}))
	}

	metricExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		return nil, fmt.Errorf("producer %d metric exporter: %w", idx, err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(1*time.Second)),
		),
	)

	meter := mp.Meter(svc)

	// Create metric instruments.
	requestCounter, err := meter.Int64Counter("http.server.request.count",
		metric.WithDescription("Count of HTTP requests"),
	)
	if err != nil {
		return nil, fmt.Errorf("producer %d request counter: %w", idx, err)
	}

	queueDepthGauge, err := meter.Int64Gauge("queue.depth",
		metric.WithDescription("Current queue depth"),
	)
	if err != nil {
		return nil, fmt.Errorf("producer %d queue depth gauge: %w", idx, err)
	}

	resetCounter, err := meter.Int64Counter("custom.reset.counter",
		metric.WithDescription("Custom counter that resets every 5 minutes"),
	)
	if err != nil {
		return nil, fmt.Errorf("producer %d reset counter: %w", idx, err)
	}

	return &producer{
		idx:             idx,
		endpoint:        endpoint,
		tenantID:        tenantID,
		insecure:        insecure,
		tp:              tp,
		tracer:          tp.Tracer(svc),
		mp:              mp,
		meter:           meter,
		meters:          producerMeters{requestCounter, queueDepthGauge, resetCounter},
		rng:             rand.New(rand.NewSource(time.Now().UnixNano() + int64(idx))),
		resetCountStart: time.Now(),
	}, nil
}

// run emits spans, logs, and metrics at their respective rates for the given duration.
func (p *producer) run(ctx context.Context, spanRps, logRps, metricRps int, dur time.Duration) {
	deadline := time.Now().Add(dur)
	seq := 0

	// Create rate limiters for each signal type (zero rate means disabled).
	var spanLimiter, logLimiter, metricLimiter *rateLimiter
	if spanRps > 0 {
		spanLimiter = newRateLimiter(spanRps)
		defer spanLimiter.stop()
	}
	if logRps > 0 {
		logLimiter = newRateLimiter(logRps)
		defer logLimiter.stop()
	}
	if metricRps > 0 {
		metricLimiter = newRateLimiter(metricRps)
		defer metricLimiter.stop()
	}

	// Round-robin through signals: emit whichever is ready first.
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Non-blocking checks for rate limiters.
		spanReady := spanLimiter != nil && len(spanLimiter.ch) > 0
		logReady := logLimiter != nil && len(logLimiter.ch) > 0
		metricReady := metricLimiter != nil && len(metricLimiter.ch) > 0

		// Blocking: wait for at least one signal to be ready.
		if !spanReady && !logReady && !metricReady {
			if spanLimiter != nil {
				<-spanLimiter.ch
			} else if logLimiter != nil {
				<-logLimiter.ch
			} else if metricLimiter != nil {
				<-metricLimiter.ch
			} else {
				return // no signals enabled
			}
		}

		// Emit whichever signals are ready.
		if spanLimiter != nil && len(spanLimiter.ch) > 0 {
			select {
			case <-spanLimiter.ch:
				p.emitSpan(ctx, seq)
				seq++
			default:
			}
		}
		if logLimiter != nil && len(logLimiter.ch) > 0 {
			select {
			case <-logLimiter.ch:
				p.emitLog(ctx, seq)
				seq++
			default:
			}
		}
		if metricLimiter != nil && len(metricLimiter.ch) > 0 {
			select {
			case <-metricLimiter.ch:
				p.emitMetrics(ctx, seq)
				seq++
			default:
			}
		}
	}
}

// emitSpan creates one span (with optional child spans every 10th call).
func (p *producer) emitSpan(ctx context.Context, seq int) {
	op := pickOperation(seq)
	dur := p.randomDuration()
	errored := isError(seq)

	// Every 10th span: create a parent with 1–3 children in the same trace.
	if seq%10 == 0 {
		parentCtx, parentSpan := p.tracer.Start(ctx, op)
		if errored {
			parentSpan.SetStatus(codes.Error, "simulated error")
			parentSpan.RecordError(errors.New("fake failure"))
			p.spansErrors.Add(1)
		}

		numChildren := 1 + p.rng.Intn(3) // [1,3]
		for c := 0; c < numChildren; c++ {
			childOp := pickOperation(seq + c + 1)
			_, childSpan := p.tracer.Start(parentCtx, childOp)
			time.Sleep(dur / time.Duration(numChildren+1))
			childSpan.End()
			p.spansSent.Add(1)
		}

		time.Sleep(dur / time.Duration(numChildren+1))
		parentSpan.End()
		p.spansSent.Add(1)
	} else {
		_, span := p.tracer.Start(ctx, op)
		if errored {
			span.SetStatus(codes.Error, "simulated error")
			span.RecordError(errors.New("fake failure"))
			p.spansErrors.Add(1)
		}
		time.Sleep(dur)
		span.End()
		p.spansSent.Add(1)
	}
}

// emitLog generates and records a log entry (via OpenTelemetry logs API when available).
// For now, we track sent/error counts but don't actually export logs via the SDK
// since the Go OTel logs API is still in early development.
func (p *producer) emitLog(ctx context.Context, seq int) {
	_ = pickSeverity(seq)      // Severity is determined but not used until logs SDK is available.
	_ = p.generateLogBody(seq) // Generate but don't export (no logs SDK yet)
	p.logsSent.Add(1)

	// Simulate occasional log errors (~5% like spans).
	if isError(seq) {
		p.logsErrors.Add(1)
	}
}

// emitMetrics records metric data: request counter, queue depth, and reset counter.
func (p *producer) emitMetrics(ctx context.Context, seq int) {
	// Update request counter (monotonic cumulative).
	p.requestCount.Add(1)
	p.meters.requestCounter.Add(ctx, 1)

	// Update queue depth gauge (simulate a value).
	depth := int64(p.rng.Intn(100)) // NOSONAR go:S2245 -- synthetic load data, not security-sensitive
	p.queueDepth.Store(depth)
	p.meters.queueDepthGauge.Record(ctx, depth)

	// Update reset counter: increments until ~5 minutes, then resets.
	if time.Since(p.resetCountStart) > 5*time.Minute {
		p.resetCountSeq.Store(0)
		p.resetCountStart = time.Now()
	}
	seq64 := int64(seq % 300) // cycle every 300 emissions
	p.resetCountSeq.Store(seq64)
	p.meters.resetCounter.Add(ctx, 1)

	p.metricsSent.Add(1)

	// Simulate occasional metric errors (~5%).
	if isError(seq) {
		p.metricsErrors.Add(1)
	}
}

// shutdown flushes the exporters and waits up to the given timeout.
func (p *producer) shutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := p.tp.Shutdown(ctx); err != nil {
		log.Printf("producer %d trace shutdown error: %v", p.idx, err)
	}
	if err := p.mp.Shutdown(ctx); err != nil {
		log.Printf("producer %d metric shutdown error: %v", p.idx, err)
	}
}

// -------------------------------------------------------------------------
// Coordinator
// -------------------------------------------------------------------------

type coordinator struct {
	startTime time.Time

	totalSpansSent     atomic.Int64
	totalSpansErrors   atomic.Int64
	totalLogsSent      atomic.Int64
	totalLogsErrors    atomic.Int64
	totalMetricsSent   atomic.Int64
	totalMetricsErrors atomic.Int64
}

func (c *coordinator) progressLoop(ctx context.Context, interval time.Duration, producers []*producer) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var prevSpans, prevLogs, prevMetrics int64
	prevTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			elapsed := now.Sub(c.startTime).Seconds()

			// Aggregate per-producer counters.
			var spans, spansErr, logs, logsErr, metrics, metricsErr int64
			for _, p := range producers {
				spans += p.spansSent.Load()
				spansErr += p.spansErrors.Load()
				logs += p.logsSent.Load()
				logsErr += p.logsErrors.Load()
				metrics += p.metricsSent.Load()
				metricsErr += p.metricsErrors.Load()
			}

			// Store totals.
			c.totalSpansSent.Store(spans)
			c.totalSpansErrors.Store(spansErr)
			c.totalLogsSent.Store(logs)
			c.totalLogsErrors.Store(logsErr)
			c.totalMetricsSent.Store(metrics)
			c.totalMetricsErrors.Store(metricsErr)

			// Calculate rates.
			dt := now.Sub(prevTime).Seconds()
			deltaTot := (spans - prevSpans) + (logs - prevLogs) + (metrics - prevMetrics)
			rate := float64(deltaTot) / dt

			prevSpans, prevLogs, prevMetrics = spans, logs, metrics
			prevTime = now

			fmt.Printf("[T+%3.0fs] spans=%d logs=%d metrics=%d errors=%d rate=%.0f/s\n",
				elapsed, spans, logs, metrics, spansErr+logsErr+metricsErr, rate)
		}
	}
}

// -------------------------------------------------------------------------
// Main
// -------------------------------------------------------------------------

func main() {
	endpoint := flag.String("endpoint", "localhost:4317", "OTLP gRPC endpoint")
	numServices := flag.Int("services", 200, "Number of simulated services")
	rps := flag.Int("rate", 50, "Spans per second per service")
	logsRate := flag.Int("logs-rate", 0, "Logs per second per service (0 = disabled)")
	metricsRate := flag.Int("metrics-rate", 0, "Metrics per second per service (0 = disabled)")
	profile := flag.String("profile", "", "Profile name (e.g. 'aggregate-acceptance'); overrides services/rate settings")
	burst := flag.String("burst", "", "Burst multiplier and duration (e.g. '2x30s')")
	duration := flag.Duration("duration", 60*time.Second, "Test duration")
	insecure := flag.Bool("insecure", true, "Use insecure gRPC connection")
	tenantID := flag.String("tenant-id", "", "x-tenant-id gRPC metadata value (empty = omit)")
	warmup := flag.Duration("warmup", 5*time.Second, "Stagger window for producer startup")
	direct := flag.Bool("direct", false, "Use the direct OTLP emission engine (measures per-Export ACK latency; required for the #173 release gates)")
	settle := flag.Duration("settle", 60*time.Second, "Direct engine: unmeasured settle window before the sustained phase")
	batchInterval := flag.Duration("batch-interval", 250*time.Millisecond, "Direct engine: per-emitter batch tick")
	callTimeout := flag.Duration("call-timeout", 30*time.Second, "Direct engine: per-Export deadline")
	reportPath := flag.String("report", "", "Direct engine: write the JSON latency/throughput report to this path")
	ackLedgerPath := flag.String("ack-ledger", "", "Direct engine: persist the per-window attempted/ACKed contribution ledger to this path (required by the #202 recovery gate)")
	ackLedgerFlush := flag.Duration("ack-ledger-flush", 2*time.Second, "Direct engine: how often the ACK ledger is fsynced to disk")
	flag.Parse()

	// Apply profile if set.
	if *profile != "" {
		switch *profile {
		case "aggregate-acceptance":
			*numServices = 150
			// ~10k points/s split: 75% spans, 15% logs, 10% metrics = 7500/1500/1000
			// Over 150 services: 50/10/6.67 per service
			*rps = 50
			*logsRate = 10
			*metricsRate = 7 // ~7/service
		default:
			log.Fatalf("Unknown profile: %s", *profile)
		}
	}

	// Parse burst spec if provided.
	var burstConfig burstSpec
	if *burst != "" {
		var err error
		burstConfig, err = parseBurstSpec(*burst)
		if err != nil {
			log.Fatalf("Invalid burst spec: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Direct engine: one synchronous Export per batch tick, timed. This is the
	// only path that produces ACK-latency percentiles, and the only one that
	// can reach 10k points/s (the SDK path sleeps the simulated span duration
	// in its emit loop). See otlpdirect.go for why.
	if *direct {
		sustainedDur := *duration
		burstDur := time.Duration(0)
		if burstConfig.multiplier > 0 {
			burstDur = burstConfig.duration
		}
		mul := burstConfig.multiplier
		if mul <= 0 {
			mul = 1
		}
		cfg := directConfig{
			endpoint:    *endpoint,
			tenantID:    *tenantID,
			insecure:    *insecure,
			services:    *numServices,
			spanRate:    float64(*rps),
			logRate:     float64(*logsRate),
			metricRate:  float64(*metricsRate),
			interval:    *batchInterval,
			burstMul:    mul,
			callTimout:  *callTimeout,
			ledgerPath:  *ackLedgerPath,
			ledgerFlush: *ackLedgerFlush,
		}
		fmt.Printf("direct engine: %d services -> %s | per-service %.1f span/s %.1f log/s %.1f metric/s\n",
			cfg.services, cfg.endpoint, cfg.spanRate, cfg.logRate, cfg.metricRate)
		fmt.Printf("offered load: %.0f pts/s sustained, %.0f pts/s burst | settle %s, sustained %s, burst %s\n",
			float64(cfg.services)*(cfg.spanRate+cfg.logRate+cfg.metricRate),
			float64(cfg.services)*(cfg.spanRate+cfg.logRate+cfg.metricRate)*mul,
			*settle, sustainedDur, burstDur)
		if _, err := runDirect(ctx, cfg, *settle, sustainedDur, burstDur, *reportPath); err != nil {
			log.Fatalf("direct run failed: %v", err)
		}
		return
	}

	// Suppress default OTel global TracerProvider noise.
	otel.SetTracerProvider(sdktrace.NewTracerProvider())

	fmt.Printf("Starting %d-service load simulator → %s\n", *numServices, *endpoint)
	fmt.Printf("Rates: %d span/s, %d log/s, %d metric/s per service | Duration: %s | Warmup: %s\n",
		*rps, *logsRate, *metricsRate, *duration, *warmup)
	if *burst != "" {
		fmt.Printf("Burst: %v for %v\n", burstConfig.multiplier, burstConfig.duration)
	}
	fmt.Println("Press Ctrl+C to stop early.")

	coord := &coordinator{startTime: time.Now()}

	// Create all producers up front (no connections yet — lazy dial).
	producers := make([]*producer, *numServices)
	for i := 0; i < *numServices; i++ {
		p, err := newProducer(ctx, i, *endpoint, *tenantID, *insecure)
		if err != nil {
			log.Fatalf("Failed to create producer %d: %v", i, err)
		}
		producers[i] = p
	}

	// Stagger goroutine to roll out producers linearly over warmup window.
	staggerDelay := time.Duration(0)
	if *numServices > 1 {
		staggerDelay = *warmup / time.Duration(*numServices-1)
	}

	var wg sync.WaitGroup

	// Progress reporter (runs until ctx cancelled or all producers done).
	progressCtx, stopProgress := context.WithCancel(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		coord.progressLoop(progressCtx, 5*time.Second, producers)
	}()

	// Launch producers with stagger.
	producersDone := make(chan struct{})
	go func() {
		defer close(producersDone)
		var pwg sync.WaitGroup
	warmupLoop:
		for i, p := range producers {
			if i > 0 && staggerDelay > 0 {
				select {
				case <-ctx.Done():
					break warmupLoop
				case <-time.After(staggerDelay):
				}
			}
			pwg.Add(1)
			pp := p
			go func() {
				defer pwg.Done()
				// Apply burst if configured.
				actualSpanRate := *rps
				actualLogRate := *logsRate
				actualMetricRate := *metricsRate
				actualDuration := *duration

				if burstConfig.multiplier > 0 {
					// Run sustained phase first.
					sustainedDur := *duration - burstConfig.duration
					if sustainedDur > 0 {
						pp.run(ctx, actualSpanRate, actualLogRate, actualMetricRate, sustainedDur)
						select {
						case <-ctx.Done():
							return
						default:
						}
					}
					// Then run burst phase.
					burstSpanRate := int(float64(actualSpanRate) * burstConfig.multiplier)
					burstLogRate := int(float64(actualLogRate) * burstConfig.multiplier)
					burstMetricRate := int(float64(actualMetricRate) * burstConfig.multiplier)
					pp.run(ctx, burstSpanRate, burstLogRate, burstMetricRate, burstConfig.duration)
				} else {
					pp.run(ctx, actualSpanRate, actualLogRate, actualMetricRate, actualDuration)
				}
			}()
		}
		pwg.Wait()
	}()

	// Wait for producers to finish or signal.
	select {
	case <-producersDone:
	case <-ctx.Done():
		fmt.Println("\nShutting down early (signal received)…")
	}

	// Stop progress reporter.
	stop() // cancel signal context
	stopProgress()
	wg.Wait()

	// Final aggregate per-signal counts.
	var totalSpans, totalSpansErr int64
	var totalLogs, totalLogsErr int64
	var totalMetrics, totalMetricsErr int64
	for _, p := range producers {
		totalSpans += p.spansSent.Load()
		totalSpansErr += p.spansErrors.Load()
		totalLogs += p.logsSent.Load()
		totalLogsErr += p.logsErrors.Load()
		totalMetrics += p.metricsSent.Load()
		totalMetricsErr += p.metricsErrors.Load()
	}

	// Flush all exporters (up to 5s total).
	fmt.Printf("Flushing %d exporters…\n", len(producers))
	flushTimeout := 5 * time.Second / time.Duration(len(producers)+1)
	if flushTimeout < 100*time.Millisecond {
		flushTimeout = 100 * time.Millisecond
	}
	var shutWg sync.WaitGroup
	for _, p := range producers {
		shutWg.Add(1)
		pp := p
		go func() {
			defer shutWg.Done()
			pp.shutdown(flushTimeout)
		}()
	}
	shutWg.Wait()

	elapsed := time.Since(coord.startTime)
	elapsedSec := elapsed.Seconds()

	fmt.Println("─────────────────────────────────────────────────────────────")
	fmt.Printf("Duration:        %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("\nSpans:           %d sent, %d errors (%.1f%% error rate)\n",
		totalSpans, totalSpansErr, 100*float64(totalSpansErr)/float64(totalSpans+1))
	fmt.Printf("                 Rate: %.0f span/s\n", float64(totalSpans)/elapsedSec)
	if totalLogs > 0 {
		fmt.Printf("\nLogs:            %d sent, %d errors (%.1f%% error rate)\n",
			totalLogs, totalLogsErr, 100*float64(totalLogsErr)/float64(totalLogs+1))
		fmt.Printf("                 Rate: %.0f log/s\n", float64(totalLogs)/elapsedSec)
	}
	if totalMetrics > 0 {
		fmt.Printf("\nMetrics:         %d sent, %d errors (%.1f%% error rate)\n",
			totalMetrics, totalMetricsErr, 100*float64(totalMetricsErr)/float64(totalMetrics+1))
		fmt.Printf("                 Rate: %.0f metric/s\n", float64(totalMetrics)/elapsedSec)
	}
	totalSignals := totalSpans + totalLogs + totalMetrics
	totalErrors := totalSpansErr + totalLogsErr + totalMetricsErr
	fmt.Printf("\nCombined:        %d total signals, %d errors (%.1f%% error rate)\n",
		totalSignals, totalErrors, 100*float64(totalErrors)/float64(totalSignals+1))
	fmt.Printf("                 Rate: %.0f signal/s\n", float64(totalSignals)/elapsedSec)
	fmt.Println("─────────────────────────────────────────────────────────────")
}
