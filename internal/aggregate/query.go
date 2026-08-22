package aggregate

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// The engine's query facade (#164, #175).
//
// The engine is the SOLE query facade for aggregate data: HTTP handlers, MCP
// tools and the WebSocket publisher call QueryBuckets / QueryDashboard /
// QueryTopology, and nothing outside this package touches aggregate.db. The
// three entry points are purpose-built for broad requests — none of them hands
// a caller millions of Bucket structs through one generic call.
//
// Every query starts by capturing an ownership snapshot. Memory-owned windows
// are read exclusively from the shards, store-owned windows exclusively from
// the store, and the handover between the two is atomic (see ownership in
// engine.go). Row presence never decides the source; ownership does.

// Coverage is the honesty vocabulary of a response (#164). It says what the
// numbers are derived from, so a surface can never imply completeness it does
// not have.
type Coverage string

// Coverage values.
const (
	// CoverageFull means the numbers describe every accepted event.
	CoverageFull Coverage = "full"
	// CoverageSampled means part of the response is derived from a subset of
	// events — exact where the aggregate engine counted, partial elsewhere.
	CoverageSampled Coverage = "sampled"
	// CoverageExemplar means the response is built only from retained raw
	// exemplars. An absent exemplar NEVER implies zero matching events.
	CoverageExemplar Coverage = "exemplar"
)

// CoverageHeader is the response header that carries Coverage on bare-array
// endpoints, where an envelope would silently break the response shape.
const CoverageHeader = "OtelContext-Data-Coverage"

// Coverage notes. They exist because "exemplar" is only honest if the surface
// also says what an absent exemplar does and does not mean.
const (
	// SampledCoverageNote explains a partially aggregate-derived response.
	SampledCoverageNote = "counts and rates are exact for accepted telemetry; " +
		"parts of this response are derived from retained exemplars and are not complete"
	// ExemplarCoverageNote explains an exemplar-only response.
	ExemplarCoverageNote = "results come from retained raw exemplars only; " +
		"a missing exemplar does not mean zero matching events occurred"
)

// Note returns the caveat that belongs with a coverage value, or "" for full
// coverage, which needs none.
func (c Coverage) Note() string {
	switch c {
	case CoverageSampled:
		return SampledCoverageNote
	case CoverageExemplar:
		return ExemplarCoverageNote
	default:
		return ""
	}
}

// AccuracyMetadata describes how accurate a percentile in the response is. It
// is computed from the FINAL MERGED sketch on every response and is never a
// hard-coded figure: merging mismatched scales downscales to the coarser one,
// so a merged sketch can be less accurate than the platform default scale.
type AccuracyMetadata struct {
	// Approximate is true whenever a percentile came from a sketch.
	Approximate bool `json:"approximate"`
	// SketchScale is the mapping scale of the merged sketch.
	SketchScale uint8 `json:"sketch_scale"`
	// RelativeErrorBound is the worst-case relative error of a quantile
	// estimate at SketchScale, as a fraction (0.0217 = 2.17%).
	RelativeErrorBound float64 `json:"relative_error_bound"`
	// Degraded reports that the merged sketch collapsed or saturated, which
	// puts estimates in the affected range OUTSIDE RelativeErrorBound.
	Degraded bool `json:"degraded,omitempty"`

	// --- OTLP histogram provenance (#199) ---
	//
	// A bare Degraded=true does not describe a folded source histogram: an
	// explicit-bounds histogram imports its own bucket width as error, and an
	// unbounded +Inf bucket is not an error bound at all but a missing tail.
	// Both are named here rather than collapsed into one boolean.

	// SourceBucketError is the worst-case relative error imported from the
	// SOURCE histogram's bucket widths, as a fraction. Zero for a native
	// sketch and for exponential histograms, whose index transfer is exact.
	// When non-zero it, not RelativeErrorBound, is the number that dominates.
	SourceBucketError float64 `json:"source_bucket_error,omitempty"`
	// UnboundedTail reports that observations landed in the source
	// histogram's +Inf bucket. A quantile that falls there is a LOWER BOUND
	// (>= UnboundedTailBound), never an estimate.
	UnboundedTail      bool    `json:"unbounded_tail,omitempty"`
	UnboundedTailBound float64 `json:"unbounded_tail_bound,omitempty"`
	// PercentilesUnavailable suppresses every quantile: the sketch does not
	// describe the whole distribution. PercentilesUnavailableReason names why
	// (negative_observations, scale_out_of_range, no_finite_boundaries).
	PercentilesUnavailable       bool   `json:"percentiles_unavailable,omitempty"`
	PercentilesUnavailableReason string `json:"percentiles_unavailable_reason,omitempty"`
}

// AccuracyFromHistogramDelta derives the accuracy metadata of a folded OTLP
// histogram distribution. It layers the source histogram's provenance on top
// of the merged sketch's own bound, so a caller cannot read a 2.17% error
// bound off a distribution whose source buckets were a decade wide.
func AccuracyFromHistogramDelta(d *AggregateDelta) AccuracyMetadata {
	if d == nil {
		return AccuracyFromSketch(nil)
	}
	acc := AccuracyFromSketch(d.Sketch)
	acc.SourceBucketError = d.HistogramSourceError
	if d.HistogramFlags&HistUnboundedTail != 0 {
		acc.UnboundedTail = true
		acc.UnboundedTailBound = d.HistogramTailBound
	}
	if d.HistogramFlags&HistPercentilesUnavailable != 0 {
		acc.PercentilesUnavailable = true
		acc.PercentilesUnavailableReason = histReason(d.HistogramFlags).String()
	}
	return acc
}

// AccuracyFromSketch derives the accuracy metadata of a merged sketch. A nil
// sketch still reports approximate: the path is approximate whether or not any
// duration happened to arrive in the window.
func AccuracyFromSketch(s *Sketch) AccuracyMetadata {
	if s == nil {
		gamma := sketchBase(SketchDefaultScale)
		return AccuracyMetadata{
			Approximate:        true,
			SketchScale:        SketchDefaultScale,
			RelativeErrorBound: (gamma - 1) / (gamma + 1),
		}
	}
	return AccuracyMetadata{
		Approximate:        true,
		SketchScale:        s.Scale(),
		RelativeErrorBound: s.RelativeError(),
		Degraded:           s.Collapsed() || s.Saturations() > 0,
	}
}

// Query bounds one read. Tenant, Start and End are mandatory; the rest narrow
// the scan.
type Query struct {
	// Tenant is the tenant name. Required.
	Tenant string
	// Start and End bound the read. Required, Start before End.
	Start, End time.Time
	// Services, when non-empty, restricts the read to these service names.
	Services []string
	// Signal, when non-zero, restricts the read to one signal.
	Signal Signal
}

// TrafficPoint is one window of the traffic series.
//
// BOTH bases are carried, explicitly named (#197 Q3). Traffic is plotted on the
// request basis; the span basis stays available because it is what per-operation
// diagnostics and latency are counted in.
type TrafficPoint struct {
	// WindowStart is the UTC start of the five-minute window.
	WindowStart time.Time
	// RequestCount is accepted request entry points — root or SERVER spans.
	// ErrorRequestCount is its error subset.
	RequestCount, ErrorRequestCount int64
	// SpanCount is accepted spans; SpanErrorCount is its error subset.
	SpanCount, SpanErrorCount int64
}

// BucketsResult is the answer to QueryBuckets.
type BucketsResult struct {
	Points   []TrafficPoint
	Coverage Coverage
	Epoch    string
	Revision uint64
}

// ServiceStat is one service's aggregate accounting over the queried range.
//
// Count/ErrorCount/ErrorRate stay SPAN-based: per-service and per-operation
// diagnostics are about the work done, not about how many requests entered.
// RequestCount/ErrorRequestCount are carried alongside for the surfaces that
// want the entry-point basis.
type ServiceStat struct {
	Service           string
	Count             int64
	ErrorCount        int64
	ErrorRate         float64
	AvgLatencyMs      float64
	RequestCount      int64
	ErrorRequestCount int64
}

// DashboardResult is the answer to QueryDashboard.
//
// There is no TotalTraces field: #194 blocker 5 is precisely that the old one
// carried a SPAN count under a trace name, and a field that has been wrong
// cannot be fixed by leaving its name in place. Every count here says its basis
// (#197 Q3), and the headline error rate is the request-basis one.
type DashboardResult struct {
	// RequestCount is accepted request entry points over the range;
	// ErrorRequestCount is its error subset and RequestErrorRate is the
	// headline dashboard error rate, as a PERCENT.
	RequestCount      int64
	ErrorRequestCount int64
	RequestErrorRate  float64
	// SpanCount is accepted spans; SpanErrorCount is its error subset and
	// SpanErrorRate the corresponding PERCENT.
	SpanCount      int64
	SpanErrorCount int64
	SpanErrorRate  float64

	TotalLogs        int64
	AvgLatencyMs     float64
	ActiveServices   int64
	P99LatencyMicros float64
	TopFailing       []ServiceStat
	Accuracy         AccuracyMetadata
	Coverage         Coverage
	Epoch            string
	Revision         uint64
}

// TopologyEdge is one caller/callee edge of the topology.
type TopologyEdge struct {
	Source, Target string
	CallCount      int64
	AvgLatencyMs   float64
	ErrorRate      float64
}

// TopologyResult is the answer to QueryTopology.
type TopologyResult struct {
	Nodes    []ServiceStat
	Edges    []TopologyEdge
	Coverage Coverage
	Epoch    string
	Revision uint64
}

// asInt64 narrows an aggregate counter to the signed type the wire format uses,
// saturating rather than wrapping. A count past 2^63 is not reachable in this
// system, but a wrapped negative total on a dashboard would be worse than a
// pinned one.
func asInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// visitFunc receives one (window, series, delta) triple. The delta pointer is
// only valid for the duration of the call: memory-owned deltas are visited
// under the shard lock and are NOT cloned, because every caller folds them
// into accumulators rather than retaining them.
type visitFunc func(windowStart int64, key SeriesKey, d *AggregateDelta)

// plan is the first half of every query: it validates the bounds, visits the
// memory-owned windows, and returns the sub-range the STORE owns.
//
// The three query classes diverge only in what they do with that sub-range
// (#197 Q1), and they diverge on purpose:
//
//	scalar totals  -> SumBuckets: the database sums, one row per group, so no
//	                  row cap exists to truncate the answer.
//	percentiles    -> pageStore: sketches cannot be merged in SQL, so every
//	                  sketch-bearing row is paged to completion.
//	generic rows   -> ReadBuckets: keeps its row cap and says so.
//
// hasStore is false when memory owns the whole range or no store is attached.
func (e *Engine) plan(q Query, visit visitFunc) (Ownership, Selector, bool, error) {
	var (
		own Ownership
		sel Selector
	)
	if q.Tenant == "" {
		return own, sel, false, fmt.Errorf("%w: tenant is required", ErrSelectorUnbounded)
	}
	if !q.End.After(q.Start) {
		return own, sel, false, fmt.Errorf("%w: [%s,%s) is not a bounded forward range",
			ErrSelectorUnbounded, q.Start.Format(time.RFC3339), q.End.Format(time.RFC3339))
	}
	tenantID := e.cache.InternTenant(q.Tenant)
	start := WindowStart(q.Start)
	end := WindowStart(q.End.Add(WindowSize - time.Second))
	if end <= start {
		end = start + int64(WindowSize/time.Second)
	}

	// Memory first, under the ownership read lock, so the shard contents and
	// the snapshot describe the same instant.
	own = e.readMutable(tenantID, start, end, q.Signal, visit)

	// Everything below the oldest memory-owned window in range belongs to the
	// store. The mutable set is a contiguous suffix by construction, so this
	// is one bounded query rather than one per window.
	storeEnd := end
	for _, w := range own.Mutable {
		if w >= start && w < storeEnd {
			storeEnd = w
		}
	}
	if storeEnd <= start || e.Store() == nil {
		return own, sel, false, nil
	}
	return own, Selector{TenantID: tenantID, Start: start, End: storeEnd, Signal: q.Signal}, true, nil
}

// readMutable visits the memory-owned windows in [start, end) and returns the
// ownership snapshot it read under.
func (e *Engine) readMutable(tenantID uint32, start, end int64, signal Signal, visit visitFunc) Ownership {
	e.own.mu.RLock()
	defer e.own.mu.RUnlock()
	own := e.ownershipLocked()
	for _, w := range own.Mutable {
		if w < start || w >= end {
			continue
		}
		for i := range e.shards {
			sh := &e.shards[i]
			sh.mu.Lock()
			for key, d := range sh.windows[w] {
				if key.TenantID != tenantID {
					continue
				}
				if signal != SignalUnspecified && key.Signal != signal {
					continue
				}
				visit(w, key, d)
			}
			sh.mu.Unlock()
		}
	}
	return own
}

// sumStore runs the SQL aggregation over the store-owned sub-range.
func (e *Engine) sumStore(sel Selector, by GroupBy) ([]SumRow, error) {
	st := e.Store()
	if st == nil {
		return nil, nil
	}
	return st.SumBuckets(sel, by)
}

// pageStore visits every store-owned row matching sel, paging to COMPLETION.
//
// The page size is the store's own row cap; the number of pages is bounded by
// the selector's window range, which Selector.Validate already clamps to the
// retention horizon. Truncated is the loop condition, never a result: this
// function returns only when the store says there is nothing left.
func (e *Engine) pageStore(sel Selector, visit visitFunc) error {
	st := e.Store()
	if st == nil {
		return nil
	}
	for {
		page, err := st.ReadBuckets(sel)
		if err != nil {
			return err
		}
		if err := e.visitPage(st, page.Buckets, visit); err != nil {
			return err
		}
		if !page.Truncated {
			return nil
		}
		// A store that reports "more rows" without advancing the cursor would
		// spin this loop forever. Refuse rather than hang a dashboard.
		if !sel.After.zero() && !sel.After.After(page.Next.WindowStart, page.Next.SeriesID, page.Next.Source) {
			return fmt.Errorf("aggregate: paged read did not advance past %d/%d", sel.After.WindowStart, sel.After.SeriesID)
		}
		sel.After = page.Next
	}
}

// visitPage resolves one page's series identities and visits its rows.
func (e *Engine) visitPage(st Store, buckets []Bucket, visit visitFunc) error {
	if len(buckets) == 0 {
		return nil
	}
	ids := make([]SeriesID, 0, len(buckets))
	seen := make(map[SeriesID]struct{}, len(buckets))
	for _, b := range buckets {
		if _, ok := seen[b.SeriesID]; ok {
			continue
		}
		seen[b.SeriesID] = struct{}{}
		ids = append(ids, b.SeriesID)
	}
	infos, err := st.ResolveSeries(ids)
	if err != nil {
		return err
	}
	keys := make(map[SeriesID]SeriesKey, len(infos))
	for _, info := range infos {
		keys[info.ID] = info.Key
	}
	for _, b := range buckets {
		key, ok := keys[b.SeriesID]
		if !ok {
			// A bucket whose identity no longer resolves cannot be attributed
			// to a service. Counting it under an invented name would be worse
			// than leaving it out of a per-service breakdown.
			continue
		}
		visit(b.WindowStart, key, b.Delta)
	}
	return nil
}

// serviceName resolves a series' service through the dictionary. An
// unresolvable ID yields "" and the caller drops the series from per-service
// breakdowns; totals still count it.
func (e *Engine) serviceName(key SeriesKey) string {
	return e.serviceNameByID(key.ServiceID)
}

// serviceNameByID is serviceName for the SQL aggregation path, which knows the
// dictionary ID but never builds a SeriesKey.
func (e *Engine) serviceNameByID(id uint32) string {
	entry, ok := e.cache.Lookup(id)
	if !ok {
		return ""
	}
	return string(entry.Value)
}

// serviceFilter turns the query's service list into a membership test. A nil
// result means "no filter".
func serviceFilter(services []string) map[string]struct{} {
	if len(services) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(services))
	for _, s := range services {
		set[s] = struct{}{}
	}
	return set
}

// trafficCounters accumulates one window's traffic on both bases.
type trafficCounters struct {
	requests, errRequests int64
	spans, spanErrors     int64
}

// add folds one delta into the window's counters.
func (c *trafficCounters) add(d *AggregateDelta) {
	c.requests += asInt64(d.RequestCount)
	c.errRequests += asInt64(d.ErrorRequestCount)
	c.spans += asInt64(d.Count)
	c.spanErrors += asInt64(d.ErrorCount)
}

// addSum folds one SQL aggregation row into the window's counters.
func (c *trafficCounters) addSum(r SumRow) {
	c.requests += asInt64(r.RequestCount)
	c.errRequests += asInt64(r.ErrorRequestCount)
	c.spans += asInt64(r.Count)
	c.spanErrors += asInt64(r.ErrorCount)
}

// QueryBuckets returns per-window traffic counts. It is the traffic-chart
// query: one point per five-minute window, never one row per series.
//
// The store half is a SQL GROUP BY: the result is one row per window (per
// service when a service filter forces it), so the 20,000-row read cap cannot
// reach it. That is #194 blocker 4 for this query class.
func (e *Engine) QueryBuckets(q Query) (*BucketsResult, error) {
	if q.Signal == SignalUnspecified {
		q.Signal = SignalTraceOp
	}
	filter := serviceFilter(q.Services)
	byWindow := make(map[int64]*trafficCounters)
	at := func(window int64) *trafficCounters {
		c := byWindow[window]
		if c == nil {
			c = &trafficCounters{}
			byWindow[window] = c
		}
		return c
	}

	own, sel, hasStore, err := e.plan(q, func(windowStart int64, key SeriesKey, d *AggregateDelta) {
		if filter != nil {
			if _, ok := filter[e.serviceName(key)]; !ok {
				return
			}
		}
		at(windowStart).add(d)
	})
	if err != nil {
		return nil, err
	}
	if hasStore {
		// The service dimension is only requested when a filter needs it:
		// grouping by window alone keeps the result one row per window.
		by := GroupByWindow
		if filter != nil {
			by |= GroupByService
		}
		sums, err := e.sumStore(sel, by)
		if err != nil {
			return nil, err
		}
		for _, r := range sums {
			if filter != nil {
				if _, ok := filter[e.serviceNameByID(r.ServiceID)]; !ok {
					continue
				}
			}
			at(r.WindowStart).addSum(r)
		}
	}

	points := make([]TrafficPoint, 0, len(byWindow))
	for start, c := range byWindow {
		points = append(points, TrafficPoint{
			WindowStart:       time.Unix(start, 0).UTC(),
			RequestCount:      c.requests,
			ErrorRequestCount: c.errRequests,
			SpanCount:         c.spans,
			SpanErrorCount:    c.spanErrors,
		})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].WindowStart.Before(points[j].WindowStart) })
	return &BucketsResult{
		Points:   points,
		Coverage: CoverageFull,
		Epoch:    own.Epoch,
		Revision: own.Revision,
	}, nil
}

// dashAccum accumulates one service's contribution to the dashboard.
type dashAccum struct {
	count       uint64
	errors      uint64
	requests    uint64
	errRequests uint64
	durCount    uint64
	durSum      float64
}

// addDelta folds one in-memory delta into the accumulator.
func (a *dashAccum) addDelta(d *AggregateDelta) {
	a.count += d.Count
	a.errors += d.ErrorCount
	a.requests += d.RequestCount
	a.errRequests += d.ErrorRequestCount
	a.durCount += d.DurationCount
	a.durSum += d.DurationSum
}

// addSum folds one SQL aggregation row into the accumulator.
func (a *dashAccum) addSum(r SumRow) {
	a.count += r.Count
	a.errors += r.ErrorCount
	a.requests += r.RequestCount
	a.errRequests += r.ErrorRequestCount
	a.durCount += r.DurationCount
	a.durSum += r.DurationSum
}

// QueryDashboard returns the dashboard summary: totals, averages, active
// services, the p99 from the merged sketch, and the accuracy metadata that
// sketch justifies.
//
// The store side is read TWICE, on purpose (#197 Q1). The scalar totals come
// from one SQL aggregation that no row cap can truncate. The p99 cannot: a
// quantile sketch is not SUMmable, so the sketch-bearing rows are paged to
// completion and merged in Go. Doing both through one capped read is exactly
// what made the old dashboard quietly wrong past 20,000 rows.
func (e *Engine) QueryDashboard(q Query) (*DashboardResult, error) {
	q.Signal = SignalUnspecified // the scan covers traces and logs in one pass
	filter := serviceFilter(q.Services)

	var (
		total    dashAccum
		logs     uint64
		merged   *Sketch
		perSvc   = make(map[string]*dashAccum)
		services = make(map[uint32]struct{})
	)
	mergeSketch := func(s *Sketch) {
		if s == nil {
			return
		}
		if merged == nil {
			merged = NewSketchAtScaleUnchecked(s.Scale())
		}
		merged.Merge(s)
	}
	accumulate := func(serviceID uint32, name string, r SumRow) {
		services[serviceID] = struct{}{}
		total.addSum(r)
		if name == "" {
			return
		}
		acc := perSvc[name]
		if acc == nil {
			acc = &dashAccum{}
			perSvc[name] = acc
		}
		acc.addSum(r)
	}

	own, sel, hasStore, err := e.plan(q, func(_ int64, key SeriesKey, d *AggregateDelta) {
		name := e.serviceName(key)
		if filter != nil {
			if _, ok := filter[name]; !ok {
				return
			}
		}
		switch key.Signal {
		case SignalLog:
			logs += d.LogCount
			return
		case SignalTraceOp:
		default:
			return
		}
		mergeSketch(d.Sketch)
		accumulate(key.ServiceID, name, SumRow{
			Count: d.Count, ErrorCount: d.ErrorCount,
			RequestCount: d.RequestCount, ErrorRequestCount: d.ErrorRequestCount,
			DurationCount: d.DurationCount, DurationSum: d.DurationSum,
		})
	})
	if err != nil {
		return nil, err
	}
	if hasStore {
		sums, err := e.sumStore(sel, GroupByService|GroupBySignal)
		if err != nil {
			return nil, err
		}
		for _, r := range sums {
			name := e.serviceNameByID(r.ServiceID)
			if filter != nil {
				if _, ok := filter[name]; !ok {
					continue
				}
			}
			switch r.Signal {
			case SignalLog:
				logs += r.LogCount
			case SignalTraceOp:
				accumulate(r.ServiceID, name, r)
			}
		}

		sketchSel := sel
		sketchSel.Signal = SignalTraceOp
		sketchSel.SketchOnly = true
		if err := e.pageStore(sketchSel, func(_ int64, key SeriesKey, d *AggregateDelta) {
			if filter != nil {
				if _, ok := filter[e.serviceName(key)]; !ok {
					return
				}
			}
			mergeSketch(d.Sketch)
		}); err != nil {
			return nil, err
		}
	}

	res := &DashboardResult{
		RequestCount:      asInt64(total.requests),
		ErrorRequestCount: asInt64(total.errRequests),
		SpanCount:         asInt64(total.count),
		SpanErrorCount:    asInt64(total.errors),
		TotalLogs:         asInt64(logs),
		ActiveServices:    int64(len(services)),
		Accuracy:          AccuracyFromSketch(merged),
		Coverage:          CoverageFull,
		Epoch:             own.Epoch,
		Revision:          own.Revision,
	}
	if total.durCount > 0 {
		res.AvgLatencyMs = total.durSum / float64(total.durCount) / 1000.0
	}
	if total.requests > 0 {
		res.RequestErrorRate = float64(total.errRequests) / float64(total.requests) * 100
	}
	if total.count > 0 {
		res.SpanErrorRate = float64(total.errors) / float64(total.count) * 100
	}
	if merged != nil {
		res.P99LatencyMicros = merged.Quantile(0.99)
	}
	res.TopFailing = topFailing(perSvc, 5)
	return res, nil
}

// topFailing ranks services by error count, highest first, capped at limit.
func topFailing(perSvc map[string]*dashAccum, limit int) []ServiceStat {
	out := make([]ServiceStat, 0, len(perSvc))
	for name, acc := range perSvc {
		if acc.errors == 0 {
			continue
		}
		rate := 0.0
		if acc.count > 0 {
			rate = float64(acc.errors) / float64(acc.count)
		}
		out = append(out, ServiceStat{
			Service:           name,
			Count:             asInt64(acc.count),
			ErrorCount:        asInt64(acc.errors),
			ErrorRate:         rate,
			RequestCount:      asInt64(acc.requests),
			ErrorRequestCount: asInt64(acc.errRequests),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ErrorCount != out[j].ErrorCount {
			return out[i].ErrorCount > out[j].ErrorCount
		}
		return out[i].Service < out[j].Service
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// QueryTopology returns the service topology: one node per service with its
// aggregate accounting, plus whatever caller/callee edge series exist.
//
// Like the other two, its store half is a SQL GROUP BY — one row per service,
// not one per series — so CoverageFull here means what it says.
func (e *Engine) QueryTopology(q Query) (*TopologyResult, error) {
	q.Signal = SignalTraceOp
	filter := serviceFilter(q.Services)
	nodes := make(map[string]*dashAccum)
	at := func(name string) *dashAccum {
		acc := nodes[name]
		if acc == nil {
			acc = &dashAccum{}
			nodes[name] = acc
		}
		return acc
	}

	own, sel, hasStore, err := e.plan(q, func(_ int64, key SeriesKey, d *AggregateDelta) {
		name := e.serviceName(key)
		if name == "" {
			return
		}
		if filter != nil {
			if _, ok := filter[name]; !ok {
				return
			}
		}
		at(name).addDelta(d)
	})
	if err != nil {
		return nil, err
	}
	if hasStore {
		sums, err := e.sumStore(sel, GroupByService)
		if err != nil {
			return nil, err
		}
		for _, r := range sums {
			name := e.serviceNameByID(r.ServiceID)
			if name == "" {
				continue
			}
			if filter != nil {
				if _, ok := filter[name]; !ok {
					continue
				}
			}
			at(name).addSum(r)
		}
	}

	out := make([]ServiceStat, 0, len(nodes))
	for name, acc := range nodes {
		stat := ServiceStat{
			Service:           name,
			Count:             asInt64(acc.count),
			ErrorCount:        asInt64(acc.errors),
			RequestCount:      asInt64(acc.requests),
			ErrorRequestCount: asInt64(acc.errRequests),
		}
		if acc.count > 0 {
			stat.ErrorRate = float64(acc.errors) / float64(acc.count)
		}
		if acc.durCount > 0 {
			stat.AvgLatencyMs = acc.durSum / float64(acc.durCount) / 1000.0
		}
		out = append(out, stat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })

	return &TopologyResult{
		Nodes:    out,
		Edges:    []TopologyEdge{},
		Coverage: CoverageFull,
		Epoch:    own.Epoch,
		Revision: own.Revision,
	}, nil
}
