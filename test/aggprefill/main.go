//go:build prefill

// Command aggprefill fills an aggregate store with a fully populated seven-day
// history for the release-gate disk-sizing measurement.
//
// By default it writes 6,000 series x 2,016 windows = 12,096,000 rows.
// The explicit synthetic-200 log profile writes 14,500 series x 2,016 windows
// = 29,232,000 rows, reusing the same 500 historical log template names. This
// models measured log density, not exact production miner identities.
// Rows go to aggregate_buckets using only the public
// aggregate store API: one CommitGroup per window followed by FinalizeWindow of
// that window. Latency series carry real sketches built from a per-series
// lognormal distribution, so the encoded blob sizes are representative rather
// than uniform.
//
// Build tag: prefill. Run with:
//
//	go run -tags prefill ./test/aggprefill -db /path/to/aggregate.db
//	go run -tags prefill ./test/aggprefill -db /path/to/fresh.db -log-profile synthetic-200 -services 200
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
)

// Default series mix. The synthetic log profile changes only the log count.
const (
	numTraceSeries  = 2400
	numEdgeSeries   = 500
	numLogSeries    = 500
	numMetricSeries = 2400
	numSystemSeries = 200
	totalSeries     = numTraceSeries + numEdgeSeries + numLogSeries + numMetricSeries + numSystemSeries

	defaultServices = 120

	legacyLogProfile = "legacy"
	denseLogProfile  = "synthetic-200"
	denseServices    = 200
	denseLogSeries   = denseServices * 45

	windowsPerDay = 288
	days          = 7
	totalWindows  = windowsPerDay * days
	windowSecs    = 300

	// sizeHistBuckets bounds the encoded-sketch-size histogram. Any blob at or
	// above it lands in the final bucket and is also tracked by maxSize.
	sizeHistBuckets = 8192

	// diskGuardBytes stops the run before the host fills up.
	diskGuardBytes = int64(20) << 30
)

// series kinds.
const (
	kindLatency = iota // trace ops and service edges: counters + sketch
	kindLog
	kindGauge
	kindCounter
)

type seriesSpec struct {
	id   aggregate.SeriesID
	key  aggregate.SeriesKey
	kind int

	// latency distribution (microseconds), lognormal.
	logMu    float64
	logSigma float64

	// nominal observations per window; jittered per window.
	baseRate int
	errRate  float64
}

type workerStats struct {
	sketchN       int64
	sketchBytes   int64
	binCountSum   int64
	maxSize       int
	maxBins       int
	sizeHist      [sizeHistBuckets]int64
	observations  int64
	requests      uint64
	requestErrors uint64
	spans         uint64
	spanErrors    uint64
	logs          uint64
}

func main() {
	dbPath := flag.String("db", "./data/aggregate.db", "aggregate database path")
	workers := flag.Int("workers", 8, "delta-building worker goroutines")
	numWindows := flag.Int("windows", totalWindows, "windows to fill (default is the full 7-day gate: 2016)")
	services := flag.Int("services", defaultServices, "service identities (1..2400; synthetic-200 log profile requires 200)")
	servicePrefix := flag.String("service-prefix", "checkout-svc-", "service name prefix, followed by a zero-padded three-digit index")
	endTime := flag.String("end-time", "", "exclusive RFC3339 end, aligned to 5 minutes; empty preserves current-window default")
	logProfile := flag.String("log-profile", legacyLogProfile, "log density: legacy (500 keys) or synthetic-200 (9000 keys, 500 shared names; requires -services 200)")
	flag.Parse()
	if err := validatePrefill(*logProfile, *services, *servicePrefix, *numWindows); err != nil {
		log.Fatal(err)
	}
	endWindow, err := prefillEndWindow(*endTime, time.Now().UTC())
	if err != nil {
		log.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}

	store, err := aggregate.OpenSQLiteStore(aggregate.StoreConfig{
		Path:        *dbPath,
		Synchronous: "NORMAL",
	})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = store.Close()
		}
	}()

	dicts, specs := buildIdentities(*services, *servicePrefix, *logProfile)
	log.Printf("identities: %d dict rows, %d series; log-profile=%s, expected buckets=%d",
		len(dicts), len(specs), *logProfile, int64(len(specs))*int64(*numWindows))

	seriesRows := make([]aggregate.SeriesRow, len(specs))
	seen := make(map[aggregate.SeriesKey]bool, len(specs))
	for i := range specs {
		if err := specs[i].key.Validate(); err != nil {
			log.Fatalf("series %d key invalid: %v", i, err)
		}
		if seen[specs[i].key] {
			log.Fatalf("series %d duplicates key %+v", i, specs[i].key)
		}
		seen[specs[i].key] = true
		seriesRows[i] = aggregate.SeriesRow{ID: specs[i].id, Key: specs[i].key}
	}

	// Reusable per-window state. Sketches are pre-attached so ObserveSpan never
	// allocates and the per-window allocation cost stays flat.
	deltas := make([]aggregate.AggregateDelta, len(specs))
	rows := make([]aggregate.DeltaRow, len(specs))
	numLatency := numTraceSeries + numEdgeSeries
	sketches := make([]aggregate.Sketch, numLatency)
	freshSketch := *aggregate.NewSketch()

	// Explicit end-time excludes that boundary's window, keeping live ingest separate.
	firstWindow := endWindow - int64(*numWindows-1)*windowSecs

	if *workers < 1 {
		*workers = 1
	}
	stats := make([]*workerStats, *workers)
	rngs := make([]*rand.Rand, *workers)
	bounds := partition(len(specs), *workers)
	for w := 0; w < *workers; w++ {
		stats[w] = &workerStats{}
		rngs[w] = rand.New(rand.NewPCG(uint64(w)+1, 0x9e3779b97f4a7c15))
	}

	// Cumulative baselines for the counter series, committed once with the
	// first window's deltas (the baseline table holds one row per counter
	// series regardless of window count, so re-upserting every window would
	// cost time without changing the measured size).
	baselines := make([]aggregate.BaselineRow, 0, numMetricSeries/2)
	baseTime := time.Unix(firstWindow, 0).UTC()
	for i := range specs {
		if specs[i].kind != kindCounter {
			continue
		}
		baselines = append(baselines, aggregate.BaselineRow{
			SeriesID: specs[i].id,
			Producer: aggregate.ProducerID(uint64(i)%97 + 1),
			Baseline: aggregate.Baseline{
				StartTime:     baseTime,
				LastTimestamp: baseTime,
				Value:         float64(i) * 13.5,
			},
		})
	}

	start := time.Now()
	var (
		totalDeltaRows   int64
		totalBuckets     int64
		windowsFinalized int
		diskGuardTripped bool
	)

	for wi := 0; wi < *numWindows; wi++ {
		windowStart := firstWindow + int64(wi)*windowSecs
		ts := time.Unix(windowStart, 0).UTC()

		var wg sync.WaitGroup
		for w := 0; w < len(bounds)-1; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				buildRange(specs, deltas, sketches, rows, freshSketch,
					bounds[w], bounds[w+1], wi, windowStart, ts, rngs[w], stats[w])
			}(w)
		}
		wg.Wait()

		batch := &aggregate.GroupBatch{Deltas: rows}
		if wi == 0 {
			batch.Dicts = dicts
			batch.Series = seriesRows
			batch.Baselines = baselines
		}
		if err := store.CommitGroup(batch); err != nil {
			log.Fatalf("window %d (%d): commit: %v", wi, windowStart, err)
		}
		fs, err := store.FinalizeWindow(windowStart)
		if err != nil {
			log.Fatalf("window %d (%d): finalize: %v", wi, windowStart, err)
		}
		totalDeltaRows += int64(fs.DeltaRows)
		totalBuckets += int64(fs.Buckets)
		windowsFinalized++
		if fs.Buckets != len(specs) || fs.DeltaRows != len(specs) {
			log.Fatalf("window %d (%d): finalized %d buckets from %d deltas, want %d each", wi, windowStart, fs.Buckets, fs.DeltaRows, len(specs))
		}

		if (wi+1)%100 == 0 || wi == *numWindows-1 {
			db, wal, shm, sum := dbSizes(*dbPath)
			el := time.Since(start)
			rate := float64(totalBuckets) / el.Seconds()
			eta := time.Duration(float64(*numWindows-wi-1) / float64(wi+1) * float64(el))
			log.Printf("windows %d/%d buckets=%d deltas=%d elapsed=%s rate=%.0f rows/s eta=%s db=%s wal=%s shm=%s sum=%s",
				wi+1, *numWindows, totalBuckets, totalDeltaRows,
				el.Truncate(time.Second), rate, eta.Truncate(time.Second),
				human(db), human(wal), human(shm), human(sum))
			if sum > diskGuardBytes {
				diskGuardTripped = true
				log.Printf("DISK GUARD TRIPPED: %d bytes > %d; stopping early", sum, diskGuardBytes)
				break
			}
		}
	}

	elapsed := time.Since(start)
	var requests, requestErrors, spans, spanErrors, logs uint64
	for _, st := range stats {
		requests += st.requests
		requestErrors += st.requestErrors
		spans += st.spans
		spanErrors += st.spanErrors
		logs += st.logs
	}

	// Pre-checkpoint sizes, store still open.
	db, wal, shm, sum := dbSizes(*dbPath)
	if diskGuardTripped {
		fmt.Printf("\n=== PREFILL INCOMPLETE ===\n")
	} else {
		fmt.Printf("\n=== PREFILL DONE ===\n")
	}
	fmt.Printf("log_profile: %s\n", *logProfile)
	fmt.Printf("windows_expected: %d\n", *numWindows)
	fmt.Printf("bucket_rows_expected: %d\n", int64(len(specs))*int64(*numWindows))
	fmt.Printf("wall_time_seconds: %.1f (%s)\n", elapsed.Seconds(), elapsed.Truncate(time.Second))
	fmt.Printf("windows_finalized: %d\n", windowsFinalized)
	fmt.Printf("bucket_rows_written: %d\n", totalBuckets)
	fmt.Printf("delta_rows_incorporated: %d\n", totalDeltaRows)
	fmt.Printf("first_window: %d  last_window: %d\n", firstWindow, firstWindow+int64(windowsFinalized-1)*windowSecs)
	fmt.Printf("series_total: %d\n", len(specs))
	fmt.Printf("services_total: %d\n", *services)
	fmt.Printf("log_series_total: %d\n", logSeriesCount(*logProfile))
	fmt.Printf("log_template_names: %d\n", numLogSeries)
	fmt.Printf("dashboard_requests: %d\n", requests)
	fmt.Printf("dashboard_request_errors: %d\n", requestErrors)
	fmt.Printf("dashboard_spans: %d\n", spans)
	fmt.Printf("dashboard_span_errors: %d\n", spanErrors)
	fmt.Printf("dashboard_logs: %d\n", logs)
	fmt.Printf("PRE-CLOSE sizes: db=%d wal=%d shm=%d sum=%d\n", db, wal, shm, sum)

	if err := store.Close(); err != nil {
		log.Fatalf("close: %v", err)
	}
	closed = true
	db, wal, shm, sum = dbSizes(*dbPath)
	fmt.Printf("POST-CLOSE sizes: db=%d wal=%d shm=%d sum=%d\n", db, wal, shm, sum)

	reportSketchStats(stats)
	if diskGuardTripped {
		log.Fatal("prefill incomplete: disk guard tripped")
	}
}

func validatePrefill(profile string, services int, prefix string, windows int) error {
	if services < 1 || services > numTraceSeries || prefix == "" || windows < 1 {
		return fmt.Errorf("services must be 1..2400, service-prefix nonempty, and windows positive")
	}
	switch profile {
	case legacyLogProfile:
	case denseLogProfile:
		if services != denseServices {
			return fmt.Errorf("log-profile %s requires -services %d", profile, denseServices)
		}
	default:
		return fmt.Errorf("unknown log-profile %q: want %s or %s", profile, legacyLogProfile, denseLogProfile)
	}
	return nil
}

func logSeriesCount(profile string) int {
	if profile == denseLogProfile {
		return denseLogSeries
	}
	return numLogSeries
}

func prefillEndWindow(raw string, now time.Time) (int64, error) {
	if raw == "" {
		return aggregate.WindowStart(now), nil
	}
	end, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return 0, fmt.Errorf("end-time: %w", err)
	}
	if end.After(now) || end.Nanosecond() != 0 || end.Unix()%windowSecs != 0 {
		return 0, fmt.Errorf("end-time must be in the past and aligned to 5 minutes")
	}
	return end.Unix() - windowSecs, nil
}

// partition splits n items into w contiguous ranges, returned as w+1 bounds.
func partition(n, w int) []int {
	out := make([]int, 0, w+1)
	for i := 0; i <= w; i++ {
		out = append(out, n*i/w)
	}
	return out
}

// buildRange fills deltas[lo:hi] for one window and refreshes rows[lo:hi].
func buildRange(specs []seriesSpec, deltas []aggregate.AggregateDelta, sketches []aggregate.Sketch,
	rows []aggregate.DeltaRow, fresh aggregate.Sketch,
	lo, hi, windowIdx int, windowStart int64, ts time.Time, r *rand.Rand, st *workerStats) {

	numLatency := numTraceSeries + numEdgeSeries
	for i := lo; i < hi; i++ {
		sp := &specs[i]
		d := &deltas[i]
		*d = aggregate.AggregateDelta{}

		switch sp.kind {
		case kindLatency:
			sketches[i] = fresh
			d.Sketch = &sketches[i]
			// Per-window drift plus an occasional degraded window, so the
			// populated bin count and therefore the encoded size vary.
			mu := sp.logMu + 0.30*(r.Float64()-0.5)
			sigma := sp.logSigma
			if (windowIdx+i)%37 == 0 {
				mu += 0.8
				sigma *= 1.25
			}
			n := sp.baseRate + r.IntN(sp.baseRate/2+1) - sp.baseRate/4
			if n < 1 {
				n = 1
			}
			for k := 0; k < n; k++ {
				v := math.Exp(mu + sigma*r.NormFloat64())
				if v < 1 {
					v = 1
				} else if v > 60e6 {
					v = 60e6
				}
				// Trace-operation series model server-side operations and so
				// count on the request basis; service-edge series model the
				// client side of a call and do not (#197 request-basis
				// counting). Without this the seeded history carries a zero
				// request count and every request-basis read over the prefill
				// range is silently empty.
				d.ObserveSpan(v, r.Float64() < sp.errRate, i < numTraceSeries)
			}
			st.observations += int64(n)
			sz := len(d.Sketch.Encode())
			bins := d.Sketch.PopulatedBins()
			st.sketchN++
			st.sketchBytes += int64(sz)
			st.binCountSum += int64(bins)
			if sz > st.maxSize {
				st.maxSize = sz
			}
			if bins > st.maxBins {
				st.maxBins = bins
			}
			idx := sz
			if idx >= sizeHistBuckets {
				idx = sizeHistBuckets - 1
			}
			st.sizeHist[idx]++

		case kindLog:
			n := sp.baseRate + r.IntN(sp.baseRate/2+1) - sp.baseRate/4
			if n < 1 {
				n = 1
			}
			isErr := sp.key.StatusClass >= aggregate.SeverityTierError
			for k := 0; k < n; k++ {
				off := time.Duration(r.IntN(windowSecs*1000)) * time.Millisecond
				d.ObserveLog(ts.Add(off), isErr)
			}
			st.observations += int64(n)

		case kindGauge:
			n := 30 + r.IntN(31)
			for k := 0; k < n; k++ {
				off := time.Duration(k) * 10 * time.Second
				d.ObserveGauge(sp.logMu+sp.logSigma*r.NormFloat64(), ts.Add(off))
			}
			st.observations += int64(n)

		case kindCounter:
			n := 30 + r.IntN(31)
			for k := 0; k < n; k++ {
				reset := r.IntN(4096) == 0
				d.ObserveCounter(math.Abs(r.NormFloat64())*sp.logSigma+1, reset)
			}
			st.observations += int64(n)
		}

		if sp.kind != kindLatency && i < numLatency {
			d.Sketch = nil
		}
		rows[i] = aggregate.DeltaRow{SeriesID: sp.id, WindowStart: windowStart, Delta: d}
		switch sp.key.Signal {
		case aggregate.SignalTraceOp:
			st.requests += d.RequestCount
			st.requestErrors += d.ErrorRequestCount
			st.spans += d.Count
			st.spanErrors += d.ErrorCount
		case aggregate.SignalLog:
			st.logs += d.LogCount
		}
	}
}

// buildIdentities mints dictionary rows and series specs for a validated profile. Dict
// IDs are globally unique because aggregate_dict.id is the primary key; values
// are unique within (tenant, kind) because of idx_aggregate_dict_scope.
func buildIdentities(numServices int, prefix, logProfile string) ([]aggregate.DictRow, []seriesSpec) {
	var dicts []aggregate.DictRow
	nextDict := uint32(0)
	add := func(tenant uint32, kind aggregate.Kind, value string) uint32 {
		nextDict++
		dicts = append(dicts, aggregate.DictRow{
			ID: nextDict, TenantID: tenant, Kind: kind, Value: []byte(value),
		})
		return nextDict
	}

	tenantID := add(aggregate.GlobalTenant, aggregate.KindTenant, "default")

	serviceIDs := make([]uint32, numServices)
	for i := range serviceIDs {
		serviceIDs[i] = add(tenantID, aggregate.KindService, fmt.Sprintf("%s%03d", prefix, i))
	}

	// Deterministic per-series distribution parameters.
	r := rand.New(rand.NewPCG(0x5eed, 0xc0ffee))
	drawLatency := func() (mu, sigma float64) {
		// median between 5 ms and 500 ms, expressed in microseconds.
		lo, hi := math.Log(5000.0), math.Log(500000.0)
		mu = lo + r.Float64()*(hi-lo)
		sigma = 0.40 + r.Float64()*1.00
		return mu, sigma
	}

	wantSeries := totalSeries - numLogSeries + logSeriesCount(logProfile)
	specs := make([]seriesSpec, 0, wantSeries)
	nextSeries := aggregate.SeriesID(0)
	newID := func() aggregate.SeriesID { nextSeries++; return nextSeries }

	// 2,400 trace-operation series.
	for i := 0; i < numTraceSeries; i++ {
		svc := i % numServices
		nameID := add(tenantID, aggregate.KindOperation,
			fmt.Sprintf("GET /api/v%d/resource-%04d", i%3+1, i))
		mu, sigma := drawLatency()
		specs = append(specs, seriesSpec{
			id:   newID(),
			kind: kindLatency,
			key: aggregate.SeriesKey{
				TenantID:    tenantID,
				ServiceID:   serviceIDs[svc],
				NameID:      nameID,
				Signal:      aggregate.SignalTraceOp,
				StatusClass: traceStatus(i),
				HTTPClass:   aggregate.HTTPClass(i % 6),
				Method:      aggregate.Method(i % 11),
				Variant:     aggregate.Variant(i % 6),
			},
			logMu: mu, logSigma: sigma,
			baseRate: 200 + (i*7)%401,
			errRate:  0.005 + float64(i%20)/400.0,
		})
	}

	// 500 service-edge series. NameID resolves through KindOperation.
	for i := 0; i < numEdgeSeries; i++ {
		svc := i % numServices
		callee := (i*7 + 3) % numServices
		nameID := add(tenantID, aggregate.KindOperation,
			fmt.Sprintf("edge:%s%03d->%s%03d#%03d", prefix, svc, prefix, callee, i))
		mu, sigma := drawLatency()
		specs = append(specs, seriesSpec{
			id:   newID(),
			kind: kindLatency,
			key: aggregate.SeriesKey{
				TenantID:    tenantID,
				ServiceID:   serviceIDs[svc],
				NameID:      nameID,
				Signal:      aggregate.SignalServiceEdge,
				StatusClass: traceStatus(i),
				HTTPClass:   aggregate.HTTPClass(i % 6),
				Method:      aggregate.Method(i % 11),
				Variant:     aggregate.Variant(i%2 + 2), // server / client
			},
			logMu: mu, logSigma: sigma,
			baseRate: 200 + (i*13)%401,
			errRate:  0.01 + float64(i%15)/300.0,
		})
	}

	// Both profiles retain exactly the same 500 historical template names.
	// Names may be shared across services; the complete SeriesKey stays unique.
	logNames := make([]uint32, numLogSeries)
	for i := range logNames {
		logNames[i] = add(tenantID, aggregate.KindLogTemplate,
			fmt.Sprintf("template-%04d: request <*> failed with <*> after <*> ms", i))
	}
	appendLog := func(svc int, nameID uint32, tier aggregate.StatusClass, i int) {
		specs = append(specs, seriesSpec{
			id:   newID(),
			kind: kindLog,
			key: aggregate.SeriesKey{
				TenantID:    tenantID,
				ServiceID:   serviceIDs[svc],
				NameID:      nameID,
				Signal:      aggregate.SignalLog,
				StatusClass: tier,
			},
			baseRate: 20 + (i*11)%181,
		})
	}
	if logProfile == denseLogProfile {
		// The measured 20-template / 12-severity joint cycle has these pairs
		// for template index modulo 4: 20 INFO, 10 DEBUG, 10 WARN, 5 ERROR.
		// This is synthetic history, not production miner identity replay.
		tiers := [][]aggregate.StatusClass{
			{aggregate.SeverityTierInfo, aggregate.SeverityTierWarn},
			{aggregate.SeverityTierInfo, aggregate.SeverityTierDebug},
			{aggregate.SeverityTierInfo, aggregate.SeverityTierDebug},
			{aggregate.SeverityTierInfo, aggregate.SeverityTierWarn, aggregate.SeverityTierError},
		}
		i := 0
		for svc := 0; svc < numServices; svc++ {
			for template := 0; template < 20; template++ {
				nameID := logNames[(svc*20+template)%len(logNames)]
				for _, tier := range tiers[template%len(tiers)] {
					appendLog(svc, nameID, tier, i)
					i++
				}
			}
		}
	} else {
		for i, nameID := range logNames {
			appendLog(i%numServices, nameID, aggregate.StatusClass(i%6+1), i)
		}
	}

	// 2,400 native metric series: half gauge-like, half cumulative counter.
	for i := 0; i < numMetricSeries; i++ {
		svc := i % numServices
		nameID := add(tenantID, aggregate.KindMetricName,
			fmt.Sprintf("otelcontext.app.metric.%04d", i))
		kind := kindGauge
		if i >= numMetricSeries/2 {
			kind = kindCounter
		}
		specs = append(specs, seriesSpec{
			id:   newID(),
			kind: kind,
			key: aggregate.SeriesKey{
				TenantID:  tenantID,
				ServiceID: serviceIDs[svc],
				NameID:    nameID,
				Signal:    aggregate.SignalMetric,
			},
			logMu:    100 + float64(i%500),
			logSigma: 5 + float64(i%40),
		})
	}

	// 200 "system" series: metrics under a distinct dims tuple.
	sysDims := add(tenantID, aggregate.KindDimTuple, "scope=system\x00tier=platform")
	for i := 0; i < numSystemSeries; i++ {
		svc := i % numServices
		nameID := add(tenantID, aggregate.KindMetricName,
			fmt.Sprintf("otelcontext.system.metric.%04d", i))
		specs = append(specs, seriesSpec{
			id:   newID(),
			kind: kindGauge,
			key: aggregate.SeriesKey{
				TenantID:  tenantID,
				ServiceID: serviceIDs[svc],
				NameID:    nameID,
				DimsID:    sysDims,
				Signal:    aggregate.SignalMetric,
			},
			logMu:    50 + float64(i%200),
			logSigma: 3 + float64(i%20),
		})
	}

	if len(specs) != wantSeries {
		log.Fatalf("built %d series, want %d", len(specs), wantSeries)
	}
	return dicts, specs
}

func traceStatus(i int) aggregate.StatusClass {
	switch i % 5 {
	case 0, 1:
		return aggregate.StatusOK
	case 2:
		return aggregate.StatusError
	default:
		return aggregate.StatusUnset
	}
}

func dbSizes(path string) (db, wal, shm, sum int64) {
	db = fileSize(path)
	wal = fileSize(path + "-wal")
	shm = fileSize(path + "-shm")
	return db, wal, shm, db + wal + shm
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f%ciB", float64(b)/float64(div), "KMGTP"[exp])
}

func reportSketchStats(stats []*workerStats) {
	var (
		n       int64
		bytes   int64
		bins    int64
		maxSize int
		maxBins int
		obs     int64
		hist    [sizeHistBuckets]int64
	)
	for _, s := range stats {
		n += s.sketchN
		bytes += s.sketchBytes
		bins += s.binCountSum
		obs += s.observations
		if s.maxSize > maxSize {
			maxSize = s.maxSize
		}
		if s.maxBins > maxBins {
			maxBins = s.maxBins
		}
		for i := range s.sizeHist {
			hist[i] += s.sizeHist[i]
		}
	}
	fmt.Printf("\n=== SKETCH STATS (in-process, len(Sketch.Encode())) ===\n")
	if n == 0 {
		fmt.Printf("no sketches recorded\n")
		return
	}
	fmt.Printf("sketches_encoded: %d\n", n)
	fmt.Printf("total_observations_all_signals: %d\n", obs)
	fmt.Printf("avg_encoded_bytes: %.2f\n", float64(bytes)/float64(n))
	fmt.Printf("total_encoded_bytes: %d\n", bytes)
	fmt.Printf("avg_populated_bins: %.2f\n", float64(bins)/float64(n))
	fmt.Printf("max_populated_bins: %d\n", maxBins)
	fmt.Printf("max_encoded_bytes: %d\n", maxSize)
	for _, q := range []float64{0.50, 0.90, 0.99, 0.999} {
		fmt.Printf("%s: %d\n", fmt.Sprintf("p%g_encoded_bytes", q*100), percentile(&hist, n, q))
	}
}

func percentile(hist *[sizeHistBuckets]int64, total int64, q float64) int {
	target := int64(math.Ceil(q * float64(total)))
	if target < 1 {
		target = 1
	}
	var cum int64
	for i := 0; i < sizeHistBuckets; i++ {
		cum += hist[i]
		if cum >= target {
			return i
		}
	}
	return sizeHistBuckets - 1
}
