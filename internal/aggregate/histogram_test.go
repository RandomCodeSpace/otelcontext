package aggregate

import (
	"errors"
	"math"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// #199: OTLP histogram completeness for aggregate mode.
//
// The shared builders below exist because every case here needs the same
// identity/scalar preamble and only differs in its buckets. Copy-pasting the
// preamble per test would trip the CI duplication gate and, worse, let two
// cases drift apart on the fields they are not testing.

const histTestMetric = "http.server.duration"

// histCommon returns the identity and scalar payload every case shares.
func histCommon(count uint64, sum float64) HistogramCommon {
	return HistogramCommon{
		Tenant:      "acme",
		Service:     "checkout",
		Name:        histTestMetric,
		Timestamp:   time.Unix(1_700_000_000, 0),
		StartTime:   time.Unix(1_699_999_700, 0),
		Temporality: TemporalityDelta,
		Count:       count,
		Sum:         sum,
		HasSum:      true,
	}
}

// withExtremes stamps the producer-reported min/max onto a payload.
func withExtremes(c HistogramCommon, minV, maxV float64) HistogramCommon {
	c.Min, c.HasMin = minV, true
	c.Max, c.HasMax = maxV, true
	return c
}

// expPointFromValues builds an exponential histogram at scale whose positive
// buckets hold exactly the given values, using the platform's own index
// mapping. Building the source this way is what makes the downscale assertion
// meaningful: the reference sketch observes the SAME values directly.
func expPointFromValues(scale uint8, values []float64) ExponentialHistogramInput {
	minIdx, maxIdx := int32(math.MaxInt32), int32(math.MinInt32)
	for _, v := range values {
		idx := sketchIndex(v, scale)
		if idx < minIdx {
			minIdx = idx
		}
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	counts := make([]uint64, maxIdx-minIdx+1)
	var sum float64
	for _, v := range values {
		counts[sketchIndex(v, scale)-minIdx]++
		sum += v
	}
	return ExponentialHistogramInput{
		HistogramCommon: histCommon(uint64(len(values)), sum),
		Scale:           int32(scale),
		Positive:        ExpBuckets{Offset: minIdx, Counts: counts},
	}
}

// referenceSketch observes values directly at scale — the answer the fold has
// to reproduce.
func referenceSketch(t *testing.T, scale uint8, values []float64) *Sketch {
	t.Helper()
	sk, err := NewSketchAtScale(scale)
	if err != nil {
		t.Fatalf("NewSketchAtScale(%d): %v", scale, err)
	}
	for _, v := range values {
		sk.Observe(v)
	}
	return sk
}

// assertSameQuantiles fails when two sketches disagree on any quantile of
// interest. Equality is EXACT: both sides answer with a bucket representative,
// so an approximate comparison would hide a one-bin error.
func assertSameQuantiles(t *testing.T, got, want *Sketch) {
	t.Helper()
	for _, q := range []float64{0, 0.25, 0.5, 0.9, 0.99, 1} {
		g, w := got.Quantile(q), want.Quantile(q)
		if g != w {
			t.Errorf("quantile(%g) = %v, want %v", q, g, w)
		}
	}
}

var histTestValues = []float64{1.5, 3.7, 12.25, 99.9, 512.5, 1024, 4096.75}

// TestFoldExponentialHistogramDownscalesAboveScaleFour pins #199 Q1's main
// claim: an s > 4 point is folded by shifting indexes, and the result is
// bit-identical to observing the same values at scale 4. Perfect subsetting is
// either exact or it is not a property worth relying on.
func TestFoldExponentialHistogramDownscalesAboveScaleFour(t *testing.T) {
	for _, scale := range []uint8{5, 6, 10} {
		in := expPointFromValues(scale, histTestValues)
		fold, err := FoldExponentialHistogram(in)
		if err != nil {
			t.Fatalf("scale %d: FoldExponentialHistogram: %v", scale, err)
		}
		if fold.PercentilesUnavailable {
			t.Fatalf("scale %d: percentiles unavailable on a clean positive point", scale)
		}
		if fold.Sketch.Scale() != SketchDefaultScale {
			t.Errorf("scale %d: folded sketch scale = %d, want %d", scale, fold.Sketch.Scale(), SketchDefaultScale)
		}
		if fold.Sketch.Count() != uint64(len(histTestValues)) {
			t.Errorf("scale %d: sketch count = %d, want %d", scale, fold.Sketch.Count(), len(histTestValues))
		}
		if fold.SourceBucketError != 0 {
			t.Errorf("scale %d: source bucket error = %v, want 0 (index transfer is exact)", scale, fold.SourceBucketError)
		}
		assertSameQuantiles(t, fold.Sketch, referenceSketch(t, SketchDefaultScale, histTestValues))
	}
}

// TestFoldExponentialHistogramDownscalesSketchBelowScaleFour pins the other
// half of Q1: a coarser source downscales the ACCUMULATOR, so the sketch
// advertises the source's real resolution instead of a scale-4 bound it cannot
// honour.
func TestFoldExponentialHistogramDownscalesSketchBelowScaleFour(t *testing.T) {
	for _, scale := range []uint8{0, 1, 2, 3} {
		in := expPointFromValues(scale, histTestValues)
		fold, err := FoldExponentialHistogram(in)
		if err != nil {
			t.Fatalf("scale %d: FoldExponentialHistogram: %v", scale, err)
		}
		if fold.Sketch.Scale() != scale {
			t.Errorf("scale %d: folded sketch scale = %d, want %d", scale, fold.Sketch.Scale(), scale)
		}
		want := referenceSketch(t, scale, histTestValues)
		if fold.Sketch.RelativeError() != want.RelativeError() {
			t.Errorf("scale %d: relative error = %v, want %v",
				scale, fold.Sketch.RelativeError(), want.RelativeError())
		}
		assertSameQuantiles(t, fold.Sketch, want)
	}
}

// TestFoldExponentialHistogramFoldsZeroCount pins that zero_count is folded
// normally rather than dropped: those are real observations, just not
// representable in the log mapping.
func TestFoldExponentialHistogramFoldsZeroCount(t *testing.T) {
	in := expPointFromValues(SketchDefaultScale, histTestValues)
	in.ZeroCount = 3
	in.Count += 3
	fold, err := FoldExponentialHistogram(in)
	if err != nil {
		t.Fatalf("FoldExponentialHistogram: %v", err)
	}
	if fold.Sketch.ZeroCount() != 3 {
		t.Errorf("zero count = %d, want 3", fold.Sketch.ZeroCount())
	}
	if fold.Sketch.Count() != in.Count {
		t.Errorf("sketch count = %d, want %d", fold.Sketch.Count(), in.Count)
	}
	if fold.Sketch.Quantile(0) != 0 {
		t.Errorf("q0 = %v, want 0 (the zero bucket is the smallest)", fold.Sketch.Quantile(0))
	}
}

// TestFoldExponentialHistogramNegativeBucketsSuppressPercentiles is the
// non-negotiable half of Q1: the count and the scalars survive, the percentiles
// do not. Publishing the positive side's p99 as the distribution's p99 is the
// specific lie this test exists to prevent.
func TestFoldExponentialHistogramNegativeBucketsSuppressPercentiles(t *testing.T) {
	in := expPointFromValues(SketchDefaultScale, histTestValues)
	in.Negative = ExpBuckets{Offset: 0, Counts: []uint64{2, 1}}
	in.Count += 3
	in.Min, in.HasMin = -8, true
	fold, err := FoldExponentialHistogram(in)
	if err != nil {
		t.Fatalf("FoldExponentialHistogram: %v", err)
	}
	if fold.Sketch != nil {
		t.Error("sketch retained despite negative observations")
	}
	if !fold.PercentilesUnavailable || fold.DropReason != SketchDropNegativeObservations {
		t.Errorf("unavailable=%v reason=%v, want true/negative_observations",
			fold.PercentilesUnavailable, fold.DropReason)
	}
	if fold.DropReason.String() != "negative_observations" {
		t.Errorf("reason label = %q", fold.DropReason.String())
	}
	if fold.Count != in.Count || fold.Sum != in.Sum || fold.Min != -8 {
		t.Errorf("scalars lost: count=%d sum=%v min=%v", fold.Count, fold.Sum, fold.Min)
	}
}

// TestFoldExponentialHistogramNegativeScaleKeepsScalars covers the one valid
// OTLP scale range the unsigned sketch scale cannot express.
func TestFoldExponentialHistogramNegativeScaleKeepsScalars(t *testing.T) {
	in := ExponentialHistogramInput{
		HistogramCommon: histCommon(9, 900),
		Scale:           -3,
		Positive:        ExpBuckets{Offset: 1, Counts: []uint64{4, 5}},
	}
	fold, err := FoldExponentialHistogram(in)
	if err != nil {
		t.Fatalf("FoldExponentialHistogram: %v", err)
	}
	if fold.Sketch != nil || fold.DropReason != SketchDropScaleOutOfRange {
		t.Fatalf("sketch=%v reason=%v, want nil/scale_out_of_range", fold.Sketch, fold.DropReason)
	}
	if fold.Count != 9 || fold.Sum != 900 {
		t.Errorf("scalars lost: count=%d sum=%v", fold.Count, fold.Sum)
	}
}

// TestFoldExponentialHistogramRejectsMalformed pins that a point violating the
// data model is refused ENTIRELY, not silently repaired.
func TestFoldExponentialHistogramRejectsMalformed(t *testing.T) {
	cases := map[string]ExponentialHistogramInput{
		"scale above max": {HistogramCommon: histCommon(1, 1), Scale: 21,
			Positive: ExpBuckets{Counts: []uint64{1}}},
		"scale below min": {HistogramCommon: histCommon(1, 1), Scale: -11,
			Positive: ExpBuckets{Counts: []uint64{1}}},
		"count disagrees with buckets": {HistogramCommon: histCommon(5, 1), Scale: 4,
			Positive: ExpBuckets{Counts: []uint64{1, 1}}},
		"bucket cap exceeded": {HistogramCommon: histCommon(0, 0), Scale: 4,
			Positive: ExpBuckets{Counts: make([]uint64, maxExpBuckets+1)}},
		"index overflows int32": {HistogramCommon: histCommon(1, 1), Scale: 4,
			Positive: ExpBuckets{Offset: math.MaxInt32, Counts: []uint64{1}}},
		"non-finite sum": {HistogramCommon: HistogramCommon{Sum: math.Inf(1), HasSum: true}, Scale: 4},
		"min above max":  {HistogramCommon: withExtremes(histCommon(0, 0), 9, 1), Scale: 4},
	}
	for name, in := range cases {
		if _, err := FoldExponentialHistogram(in); !errors.Is(err, ErrHistogramMalformed) {
			t.Errorf("%s: err = %v, want ErrHistogramMalformed", name, err)
		}
	}
}

// histPoint builds an explicit-bounds point with the given bounds and counts.
func histPoint(bounds []float64, counts []uint64) HistogramInput {
	var total uint64
	for _, c := range counts {
		total += c
	}
	return HistogramInput{
		HistogramCommon: histCommon(total, 1234),
		Bounds:          bounds,
		BucketCounts:    counts,
	}
}

var histTestBounds = []float64{1, 2, 4}

// TestFoldHistogramFoldsBucketsAsWeightedObservations pins #199 Q2's folding
// rule: one weighted add per bucket at the geometric midpoint, with the
// source's own bucket error carried out on the fold.
func TestFoldHistogramFoldsBucketsAsWeightedObservations(t *testing.T) {
	in := histPoint(histTestBounds, []uint64{0, 5, 7, 0})
	fold, err := FoldHistogram(in)
	if err != nil {
		t.Fatalf("FoldHistogram: %v", err)
	}
	if fold.Sketch.Count() != 12 {
		t.Fatalf("sketch count = %d, want 12", fold.Sketch.Count())
	}
	if fold.UnboundedTail {
		t.Error("unbounded tail reported for an empty +Inf bucket")
	}
	// Both folded buckets have a boundary ratio of 2, so the worst-case error
	// of a geometric midpoint is sqrt(2)-1 for each.
	if want := math.Sqrt(2) - 1; math.Abs(fold.SourceBucketError-want) > 1e-12 {
		t.Errorf("source bucket error = %v, want %v", fold.SourceBucketError, want)
	}
	// The (1,2] bucket's five observations must all sit at sqrt(2).
	if got, want := fold.Sketch.Quantile(0), sketchValue(sketchIndex(math.Sqrt2, SketchDefaultScale), SketchDefaultScale); got != want {
		t.Errorf("q0 = %v, want the sqrt(2) bucket representative %v", got, want)
	}
}

// TestFoldHistogramDoesNotExpandCounts pins the "never expand count
// observations in a loop" requirement. A billion-count bucket that folded per
// observation would not return inside this test's lifetime.
func TestFoldHistogramDoesNotExpandCounts(t *testing.T) {
	const huge = uint64(1_000_000_000)
	in := histPoint(histTestBounds, []uint64{0, huge, huge, 0})
	start := time.Now()
	fold, err := FoldHistogram(in)
	if err != nil {
		t.Fatalf("FoldHistogram: %v", err)
	}
	if fold.Sketch.Count() != 2*huge {
		t.Fatalf("sketch count = %d, want %d", fold.Sketch.Count(), 2*huge)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("folding two buckets took %s; counts are being expanded", elapsed)
	}
}

// TestFoldHistogramUnboundedTail pins that +Inf observations are tracked
// separately and never folded: their only known property is a lower bound.
func TestFoldHistogramUnboundedTail(t *testing.T) {
	in := histPoint(histTestBounds, []uint64{0, 5, 7, 2})
	fold, err := FoldHistogram(in)
	if err != nil {
		t.Fatalf("FoldHistogram: %v", err)
	}
	if !fold.UnboundedTail || fold.UnboundedTailCount != 2 || fold.UnboundedTailBound != 4 {
		t.Fatalf("tail = %v/%d/%v, want true/2/4",
			fold.UnboundedTail, fold.UnboundedTailCount, fold.UnboundedTailBound)
	}
	if fold.Sketch.Count() != 12 {
		t.Errorf("sketch count = %d, want 12 — tail observations must stay out", fold.Sketch.Count())
	}
	if fold.PercentilesUnavailable {
		t.Error("an unbounded tail suppresses the tail quantiles, not the whole distribution")
	}
}

// TestFoldHistogramNegativeBucketNeedsAProvingMin pins Q2's negative rule from
// both sides: without a min at or above zero the percentiles go, with one the
// bucket folds against an effective lower boundary.
func TestFoldHistogramNegativeBucketNeedsAProvingMin(t *testing.T) {
	unproven := histPoint(histTestBounds, []uint64{3, 5, 7, 0})
	fold, err := FoldHistogram(unproven)
	if err != nil {
		t.Fatalf("FoldHistogram: %v", err)
	}
	if fold.Sketch != nil || fold.DropReason != SketchDropNegativeObservations {
		t.Fatalf("unproven: sketch=%v reason=%v, want nil/negative_observations", fold.Sketch, fold.DropReason)
	}
	if fold.Count != 15 {
		t.Errorf("unproven: count = %d, want 15 — scalars must survive", fold.Count)
	}

	proven := unproven
	proven.HistogramCommon = withExtremes(proven.HistogramCommon, 0.5, 3.9)
	fold, err = FoldHistogram(proven)
	if err != nil {
		t.Fatalf("FoldHistogram (proven): %v", err)
	}
	if fold.PercentilesUnavailable {
		t.Fatalf("proven: percentiles suppressed despite min=0.5")
	}
	if fold.Sketch.Count() != 15 {
		t.Errorf("proven: sketch count = %d, want 15", fold.Sketch.Count())
	}
}

// TestFoldHistogramZeroLowerBoundCarriesFullError pins that a bucket whose
// effective lower boundary is exactly zero admits 100% source error rather
// than pretending a geometric midpoint exists.
func TestFoldHistogramZeroLowerBoundCarriesFullError(t *testing.T) {
	in := histPoint(histTestBounds, []uint64{4, 0, 0, 0})
	in.HistogramCommon = withExtremes(in.HistogramCommon, 0, 0.9)
	fold, err := FoldHistogram(in)
	if err != nil {
		t.Fatalf("FoldHistogram: %v", err)
	}
	if fold.SourceBucketError != 1 {
		t.Errorf("source bucket error = %v, want 1", fold.SourceBucketError)
	}
	if fold.Sketch.Count() != 4 {
		t.Errorf("sketch count = %d, want 4", fold.Sketch.Count())
	}
}

// TestFoldHistogramRejectsMalformed pins the explicit-bounds data model.
func TestFoldHistogramRejectsMalformed(t *testing.T) {
	cases := map[string]HistogramInput{
		"bucket/bound arity": {HistogramCommon: histCommon(1, 1), Bounds: histTestBounds, BucketCounts: []uint64{1, 0}},
		"count disagrees":    {HistogramCommon: histCommon(99, 1), Bounds: histTestBounds, BucketCounts: []uint64{1, 0, 0, 0}},
		"bounds not ascending": {HistogramCommon: histCommon(1, 1), Bounds: []float64{4, 2},
			BucketCounts: []uint64{1, 0, 0}},
		"non-finite bound": {HistogramCommon: histCommon(1, 1), Bounds: []float64{math.Inf(1)},
			BucketCounts: []uint64{1, 0}},
		"count with no buckets": {HistogramCommon: histCommon(7, 1)},
	}
	for name, in := range cases {
		if _, err := FoldHistogram(in); !errors.Is(err, ErrHistogramMalformed) {
			t.Errorf("%s: err = %v, want ErrHistogramMalformed", name, err)
		}
	}
}

// TestFoldHistogramWithoutFiniteBoundsSuppressesPercentiles covers the single
// (-Inf, +Inf] bucket: there is no boundary to place anything against.
func TestFoldHistogramWithoutFiniteBoundsSuppressesPercentiles(t *testing.T) {
	fold, err := FoldHistogram(histPoint(nil, []uint64{6}))
	if err != nil {
		t.Fatalf("FoldHistogram: %v", err)
	}
	if fold.Sketch != nil || fold.DropReason != SketchDropNoFiniteBoundaries {
		t.Fatalf("sketch=%v reason=%v, want nil/no_finite_boundaries", fold.Sketch, fold.DropReason)
	}
}

// TestHistogramQuantileAnswersTailAsALowerBound pins the three-way contract on
// the read helper: a quantile inside the +Inf bucket is "at least the last
// finite boundary", never an ordinary estimate.
func TestHistogramQuantileAnswersTailAsALowerBound(t *testing.T) {
	fold, err := FoldHistogram(histPoint(histTestBounds, []uint64{0, 5, 7, 2}))
	if err != nil {
		t.Fatalf("FoldHistogram: %v", err)
	}
	var d AggregateDelta
	d.ObserveHistogram(fold)

	value, lower, ok := d.HistogramQuantile(0.99)
	if !ok || !lower || value != 4 {
		t.Errorf("p99 = %v lower=%v ok=%v, want 4/true/true", value, lower, ok)
	}
	value, lower, ok = d.HistogramQuantile(0.5)
	if !ok || lower || value <= 0 {
		t.Errorf("p50 = %v lower=%v ok=%v, want a positive estimate", value, lower, ok)
	}

	suppressed, err := FoldHistogram(histPoint(histTestBounds, []uint64{3, 5, 7, 0}))
	if err != nil {
		t.Fatalf("FoldHistogram: %v", err)
	}
	var sd AggregateDelta
	sd.ObserveHistogram(suppressed)
	if _, _, ok := sd.HistogramQuantile(0.99); ok {
		t.Error("a suppressed distribution published a quantile")
	}
	acc := AccuracyFromHistogramDelta(&sd)
	if !acc.PercentilesUnavailable || acc.PercentilesUnavailableReason != "negative_observations" {
		t.Errorf("accuracy = %+v, want percentiles_unavailable/negative_observations", acc)
	}
}

// TestAccuracyFromHistogramDeltaNamesTheSourceApproximation pins that a bare
// degraded=true is not what an unbounded tail or a coarse source reports.
func TestAccuracyFromHistogramDeltaNamesTheSourceApproximation(t *testing.T) {
	fold, err := FoldHistogram(histPoint(histTestBounds, []uint64{0, 5, 7, 2}))
	if err != nil {
		t.Fatalf("FoldHistogram: %v", err)
	}
	var d AggregateDelta
	d.ObserveHistogram(fold)
	acc := AccuracyFromHistogramDelta(&d)
	if !acc.UnboundedTail || acc.UnboundedTailBound != 4 {
		t.Errorf("accuracy tail = %v/%v, want true/4", acc.UnboundedTail, acc.UnboundedTailBound)
	}
	if acc.SourceBucketError <= acc.RelativeErrorBound {
		t.Errorf("source error %v must dominate the sketch bound %v for decade-ish buckets",
			acc.SourceBucketError, acc.RelativeErrorBound)
	}
}

// TestHistogramDeltaMergeIsOrderIndependent pins the additive contract for the
// new fields, including the tail bound, whose sound merge is the LOWEST of the
// two boundaries.
func TestHistogramDeltaMergeIsOrderIndependent(t *testing.T) {
	foldA, err := FoldHistogram(histPoint(histTestBounds, []uint64{0, 5, 0, 2}))
	if err != nil {
		t.Fatalf("FoldHistogram A: %v", err)
	}
	foldB, err := FoldHistogram(histPoint([]float64{1, 2, 8}, []uint64{0, 0, 3, 1}))
	if err != nil {
		t.Fatalf("FoldHistogram B: %v", err)
	}
	var ab, ba AggregateDelta
	ab.ObserveHistogram(foldA)
	ab.ObserveHistogram(foldB)
	ba.ObserveHistogram(foldB)
	ba.ObserveHistogram(foldA)

	if ab.HistogramCount != ba.HistogramCount || ab.HistogramTailCount != ba.HistogramTailCount {
		t.Fatalf("counts differ by order: %+v vs %+v", ab, ba)
	}
	if ab.HistogramTailBound != 4 || ba.HistogramTailBound != 4 {
		t.Errorf("tail bound = %v/%v, want the lower of 4 and 8 both ways",
			ab.HistogramTailBound, ba.HistogramTailBound)
	}
	if ab.HistogramFlags != ba.HistogramFlags {
		t.Errorf("flags differ by order: %d vs %d", ab.HistogramFlags, ba.HistogramFlags)
	}
}

// --- Q3: temporality ---------------------------------------------------------

// TestReducerRejectsNonDeltaHistograms pins #199 Q3: GA aggregates
// delta-temporality histograms only, and a refusal contributes NOTHING.
func TestReducerRejectsNonDeltaHistograms(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	cases := []struct {
		temporality Temporality
		reason      string
	}{
		{TemporalityCumulative, ReasonCumulativeTemporality},
		{TemporalityUnspecified, ReasonUnspecifiedTemporality},
	}
	for _, c := range cases {
		e := testEngine(t, now)
		r := e.NewReducer(now)

		hist := histPoint(histTestBounds, []uint64{0, 5, 7, 0})
		hist.Timestamp, hist.Temporality = now, c.temporality
		res := r.ReduceHistogramPoint(hist)
		if !res.Rejected() || res.Reason != c.reason {
			t.Errorf("histogram %v: rejected=%v reason=%q, want true/%s",
				c.temporality, res.Rejected(), res.Reason, c.reason)
		}

		exp := expPointFromValues(SketchDefaultScale, histTestValues)
		exp.Timestamp, exp.Temporality = now, c.temporality
		res = r.ReduceExponentialHistogramPoint(exp)
		if !res.Rejected() || res.Reason != c.reason {
			t.Errorf("exponential %v: rejected=%v reason=%q, want true/%s",
				c.temporality, res.Rejected(), res.Reason, c.reason)
		}

		if r.Len() != 0 {
			t.Errorf("%v: %d deltas emitted; a rejected point must contribute nothing", c.temporality, r.Len())
		}
		if got := r.Stats().InputPoints[SignalMetric]; got != 0 {
			t.Errorf("%v: %d input points counted for rejected histograms", c.temporality, got)
		}
	}
}

// --- Q4: dimensions ----------------------------------------------------------

// dimsEngine builds an engine that aggregates histTestMetric by two dimensions.
func dimsEngine(t *testing.T, now time.Time) *Engine {
	t.Helper()
	e, err := NewEngine(EngineConfig{
		Mode:       ModeShadow,
		Now:        func() time.Time { return now },
		MetricDims: DimsConfig{histTestMetric: {"http.method", "http.status"}},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// attr builds one OTLP attribute with a string value.
func attr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// intAttr builds one OTLP attribute with an int value.
func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

// dimsIDs reduces one point of each shape with the given attributes and
// returns the DimsID each landed on.
func dimsIDs(t *testing.T, r *Reducer, now time.Time, attrs []*commonpb.KeyValue) map[Signal][]uint32 {
	t.Helper()

	num := MetricInput{Tenant: "acme", Service: "checkout", Name: histTestMetric,
		Value: 1, Timestamp: now, Temporality: TemporalityDelta, Attributes: attrs}
	r.ReduceMetricPoint(num)

	hist := histPoint(histTestBounds, []uint64{0, 5, 7, 0})
	hist.Timestamp, hist.Attributes = now, attrs
	if res := r.ReduceHistogramPoint(hist); res.Rejected() {
		t.Fatalf("histogram rejected: %v", res.Err)
	}

	exp := expPointFromValues(SketchDefaultScale, histTestValues)
	exp.Timestamp, exp.Attributes = now, attrs
	if res := r.ReduceExponentialHistogramPoint(exp); res.Rejected() {
		t.Fatalf("exponential rejected: %v", res.Err)
	}

	ids := make([]uint32, 0, len(r.Deltas()))
	for swk := range r.Deltas() {
		ids = append(ids, swk.Key.DimsID)
	}
	return map[Signal][]uint32{SignalMetric: ids}
}

// TestDimsInternedIdenticallyAcrossPointTypes pins #199 Q4: the configured
// tuple resolves to ONE series identity whatever shape the point arrived in.
// Three shapes landing on three DimsIDs would silently triple the cardinality
// of every histogram-backed metric.
func TestDimsInternedIdenticallyAcrossPointTypes(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := dimsEngine(t, now)
	r := e.NewReducer(now)
	attrs := []*commonpb.KeyValue{attr("http.method", "GET"), intAttr("http.status", 200), attr("noise", "x")}

	ids := dimsIDs(t, r, now, attrs)[SignalMetric]
	if len(ids) != 1 {
		t.Fatalf("three points produced %d series, want 1 (%v)", len(ids), ids)
	}
	if ids[0] == 0 {
		t.Fatal("configured dimensions resolved to DimsID 0")
	}
	if r.Stats().DimsRejected != 0 {
		t.Errorf("dims rejected = %d, want 0", r.Stats().DimsRejected)
	}
}

// TestDimsMissingKeyFallsBackToZero pins the all-or-nothing contract, and
// TestDimsRejectsNonScalarValues the reasoned counter for values with no
// canonical rendering.
func TestDimsMissingKeyFallsBackToZero(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := dimsEngine(t, now)
	r := e.NewReducer(now)

	ids := dimsIDs(t, r, now, []*commonpb.KeyValue{attr("http.method", "GET")})[SignalMetric]
	if len(ids) != 1 || ids[0] != 0 {
		t.Fatalf("partial tuple produced %v, want a single DimsID 0", ids)
	}
}

func TestDimsRejectsNonScalarValues(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := dimsEngine(t, now)
	r := e.NewReducer(now)

	arrayAttr := &commonpb.KeyValue{Key: "http.status", Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{}}}}
	ids := dimsIDs(t, r, now, []*commonpb.KeyValue{attr("http.method", "GET"), arrayAttr})[SignalMetric]
	if len(ids) != 1 || ids[0] != 0 {
		t.Fatalf("array dimension produced %v, want a single DimsID 0", ids)
	}
	if got := r.Stats().DimsRejected; got != 3 {
		t.Errorf("dims rejected = %d, want 3 (one per point shape)", got)
	}
}

// TestDimsExtractionDoesNotAllocatePerPoint pins the hot-path requirement:
// resolving a bounded tuple must not allocate a map per point.
func TestDimsExtractionDoesNotAllocatePerPoint(t *testing.T) {
	keys := []string{"http.method", "http.status"}
	attrs := []*commonpb.KeyValue{attr("http.method", "GET"), attr("http.status", "200"), attr("noise", "x")}
	var s dimScratch
	// Warm the scratch buffer so its one-time growth is not counted.
	s.resolve(keys, attrs)
	allocs := testing.AllocsPerRun(100, func() {
		if _, _, ok := s.resolve(keys, attrs); !ok {
			t.Fatal("resolve failed")
		}
	})
	if allocs != 0 {
		t.Errorf("resolve allocated %v times per point, want 0", allocs)
	}
}
