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
type TrafficPoint struct {
	// WindowStart is the UTC start of the five-minute window.
	WindowStart time.Time
	// Count is accepted events; ErrorCount is the error subset.
	Count, ErrorCount int64
}

// BucketsResult is the answer to QueryBuckets.
type BucketsResult struct {
	Points   []TrafficPoint
	Coverage Coverage
	Epoch    string
	Revision uint64
}

// ServiceStat is one service's aggregate accounting over the queried range.
type ServiceStat struct {
	Service      string
	Count        int64
	ErrorCount   int64
	ErrorRate    float64
	AvgLatencyMs float64
}

// DashboardResult is the answer to QueryDashboard. Field names mirror the
// legacy dashboard payload so the response contract survives the migration.
type DashboardResult struct {
	TotalTraces      int64
	TotalLogs        int64
	TotalErrors      int64
	AvgLatencyMs     float64
	ErrorRate        float64
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

// scan walks every series in the query's range, memory-owned windows from the
// shards and store-owned windows from the store, and returns the ownership
// snapshot the walk was consistent with.
func (e *Engine) scan(q Query, visit visitFunc) (Ownership, error) {
	var own Ownership
	if q.Tenant == "" {
		return own, fmt.Errorf("%w: tenant is required", ErrSelectorUnbounded)
	}
	if !q.End.After(q.Start) {
		return own, fmt.Errorf("%w: [%s,%s) is not a bounded forward range",
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
	if storeEnd <= start {
		return own, nil
	}
	st := e.Store()
	if st == nil {
		return own, nil
	}
	if err := e.readFinalized(st, tenantID, start, storeEnd, q.Signal, visit); err != nil {
		return own, err
	}
	return own, nil
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

// readFinalized visits the store-owned windows in [start, end).
func (e *Engine) readFinalized(st Store, tenantID uint32, start, end int64, signal Signal, visit visitFunc) error {
	buckets, err := st.ReadBuckets(Selector{
		TenantID: tenantID,
		Start:    start,
		End:      end,
		Signal:   signal,
	})
	if err != nil {
		return err
	}
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
	entry, ok := e.cache.Lookup(key.ServiceID)
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

// QueryBuckets returns per-window traffic counts. It is the traffic-chart
// query: one point per five-minute window, never one row per series.
func (e *Engine) QueryBuckets(q Query) (*BucketsResult, error) {
	if q.Signal == SignalUnspecified {
		q.Signal = SignalTraceOp
	}
	filter := serviceFilter(q.Services)
	type counters struct{ count, errors int64 }
	byWindow := make(map[int64]*counters)

	own, err := e.scan(q, func(windowStart int64, key SeriesKey, d *AggregateDelta) {
		if filter != nil {
			if _, ok := filter[e.serviceName(key)]; !ok {
				return
			}
		}
		c := byWindow[windowStart]
		if c == nil {
			c = &counters{}
			byWindow[windowStart] = c
		}
		c.count += asInt64(d.Count)
		c.errors += asInt64(d.ErrorCount)
	})
	if err != nil {
		return nil, err
	}

	points := make([]TrafficPoint, 0, len(byWindow))
	for start, c := range byWindow {
		points = append(points, TrafficPoint{
			WindowStart: time.Unix(start, 0).UTC(),
			Count:       c.count,
			ErrorCount:  c.errors,
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
	count    uint64
	errors   uint64
	durCount uint64
	durSum   float64
}

// QueryDashboard returns the dashboard summary: totals, averages, active
// services, the p99 from the merged sketch, and the accuracy metadata that
// sketch justifies.
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
	own, err := e.scan(q, func(_ int64, key SeriesKey, d *AggregateDelta) {
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
		services[key.ServiceID] = struct{}{}
		total.count += d.Count
		total.errors += d.ErrorCount
		total.durCount += d.DurationCount
		total.durSum += d.DurationSum
		if d.Sketch != nil {
			if merged == nil {
				merged = NewSketchAtScaleUnchecked(d.Sketch.Scale())
			}
			merged.Merge(d.Sketch)
		}
		if name == "" {
			return
		}
		acc := perSvc[name]
		if acc == nil {
			acc = &dashAccum{}
			perSvc[name] = acc
		}
		acc.count += d.Count
		acc.errors += d.ErrorCount
	})
	if err != nil {
		return nil, err
	}

	res := &DashboardResult{
		TotalTraces:    asInt64(total.count),
		TotalLogs:      asInt64(logs),
		TotalErrors:    asInt64(total.errors),
		ActiveServices: int64(len(services)),
		Accuracy:       AccuracyFromSketch(merged),
		Coverage:       CoverageFull,
		Epoch:          own.Epoch,
		Revision:       own.Revision,
	}
	if total.durCount > 0 {
		res.AvgLatencyMs = total.durSum / float64(total.durCount) / 1000.0
	}
	if total.count > 0 {
		res.ErrorRate = float64(total.errors) / float64(total.count) * 100
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
			Service:    name,
			Count:      asInt64(acc.count),
			ErrorCount: asInt64(acc.errors),
			ErrorRate:  rate,
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
func (e *Engine) QueryTopology(q Query) (*TopologyResult, error) {
	q.Signal = SignalUnspecified
	filter := serviceFilter(q.Services)
	nodes := make(map[string]*dashAccum)

	own, err := e.scan(q, func(_ int64, key SeriesKey, d *AggregateDelta) {
		if key.Signal != SignalTraceOp {
			return
		}
		name := e.serviceName(key)
		if name == "" {
			return
		}
		if filter != nil {
			if _, ok := filter[name]; !ok {
				return
			}
		}
		acc := nodes[name]
		if acc == nil {
			acc = &dashAccum{}
			nodes[name] = acc
		}
		acc.count += d.Count
		acc.errors += d.ErrorCount
		acc.durCount += d.DurationCount
		acc.durSum += d.DurationSum
	})
	if err != nil {
		return nil, err
	}

	out := make([]ServiceStat, 0, len(nodes))
	for name, acc := range nodes {
		stat := ServiceStat{
			Service:    name,
			Count:      asInt64(acc.count),
			ErrorCount: asInt64(acc.errors),
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
