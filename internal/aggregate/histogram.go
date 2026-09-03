package aggregate

import (
	"errors"
	"fmt"
	"math"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// OTLP histogram folding (#199).
//
// The platform sketch is positive-only and fixed at scale 4. Every OTLP
// distribution point that reaches aggregate accounting has to be expressed in
// that mapping or be honestly refused, and the two source shapes fail in
// different ways:
//
//   - ExponentialHistogram shares the sketch's own mapping, so a scale change
//     is an exact integer index shift ("perfect subsetting"). Folding one is
//     lossless at the bucket-count level; only the NEGATIVE range and scales
//     below 0 have no representation.
//   - Histogram (explicit bounds) has arbitrary boundaries. Its counts can only
//     be folded as weighted synthetic observations placed inside each bucket,
//     which imports the SOURCE histogram's bucket width as error on top of the
//     sketch's own. That error is carried out on the fold, not hidden.
//
// Nothing here expands a bucket count into individual observations: a bucket
// holding a million counts costs one bin add.

// ErrHistogramMalformed marks a data point that violates the OTLP data model.
// A malformed point is refused entirely -- it never contributes a count, a sum
// or a bin -- and is reported in ExportMetricsPartialSuccess.
var ErrHistogramMalformed = errors.New("aggregate: malformed histogram point")

// Exponential-histogram scale bounds from the OTLP data model. Scales outside
// this range are malformed, not merely unrepresentable.
const (
	ExpHistogramMinScale int32 = -10
	ExpHistogramMaxScale int32 = 20
)

// SketchDropReason names why a histogram point's percentiles are unavailable
// while its scalar statistics were still accepted. It is persisted with the
// delta so a read can say WHY it has no percentile rather than implying the
// window was empty.
type SketchDropReason uint8

// SketchDropReason values. The numbering is durable: it is stored in the
// delta's histogram flags word.
const (
	// SketchDropNone means the sketch describes the whole distribution.
	SketchDropNone SketchDropReason = 0
	// SketchDropNegativeObservations means the point recorded values below
	// zero. The positive-only sketch cannot hold them, and reporting the
	// positive side's p99 as the distribution's p99 would be a lie.
	SketchDropNegativeObservations SketchDropReason = 1
	// SketchDropScaleOutOfRange means an ExponentialHistogram arrived at a
	// scale below 0. Downscaling the sketch to a negative scale is not
	// representable, and upscaling the point's buckets would not be exact.
	SketchDropScaleOutOfRange SketchDropReason = 2
	// SketchDropNoFiniteBoundaries means an explicit-bounds Histogram carried
	// observations but no finite boundary to place them between.
	SketchDropNoFiniteBoundaries SketchDropReason = 3
)

// String renders the reason as the metric label value.
func (r SketchDropReason) String() string {
	switch r {
	case SketchDropNegativeObservations:
		return "negative_observations"
	case SketchDropScaleOutOfRange:
		return "scale_out_of_range"
	case SketchDropNoFiniteBoundaries:
		return "no_finite_boundaries"
	default:
		return "none"
	}
}

// Histogram delta flag bits. They live in AggregateDelta.HistogramFlags; the
// low byte is a bit set and the high byte carries the SketchDropReason.
const (
	// HistPercentilesUnavailable marks that the sketch does NOT describe the
	// whole distribution, so no quantile may be published from it.
	HistPercentilesUnavailable uint32 = 1 << 0
	// HistUnboundedTail marks that observations landed in a +Inf bucket.
	HistUnboundedTail uint32 = 1 << 1
	// HistHasMin marks that the producer reported the minimum.
	HistHasMin uint32 = 1 << 2
	// HistHasMax marks that the producer reported the maximum.
	HistHasMax uint32 = 1 << 3

	histReasonShift = 8
	histReasonMask  = uint32(0xff) << histReasonShift
)

// histReason extracts the SketchDropReason from a flags word.
func histReason(flags uint32) SketchDropReason {
	return SketchDropReason((flags & histReasonMask) >> histReasonShift) // #nosec G115 -- masked to one byte
}

// withHistReason returns flags carrying reason.
func withHistReason(flags uint32, reason SketchDropReason) uint32 {
	return (flags &^ histReasonMask) | (uint32(reason) << histReasonShift)
}

// ExpBuckets is one side (positive or negative) of an OTLP exponential
// histogram: Counts[i] belongs to bucket index Offset+i.
type ExpBuckets struct {
	Offset int32
	Counts []uint64
}

// total sums the bucket counts, reporting overflow rather than wrapping.
func (b ExpBuckets) total() (uint64, bool) {
	var sum uint64
	for _, c := range b.Counts {
		next := sum + c
		if next < sum {
			return 0, false
		}
		sum = next
	}
	return sum, true
}

// HistogramCommon is the identity and scalar payload shared by both histogram
// point shapes. Attributes are the RAW OTLP point attributes: dimension
// extraction happens in the reducer, against a request-local scratch, so no
// per-point map is allocated (#199 Q4).
type HistogramCommon struct {
	Tenant   string
	Service  string
	Name     string
	Resource ResourceIdentity

	Timestamp time.Time
	StartTime time.Time

	Temporality Temporality
	Attributes  []*commonpb.KeyValue
	// ResourceAttributes are the RAW OTLP resource attributes; a configured
	// dimension key the point lacks falls back to them (#279).
	ResourceAttributes []*commonpb.KeyValue

	Count  uint64
	Sum    float64
	HasSum bool
	Min    float64
	HasMin bool
	Max    float64
	HasMax bool
}

// HistogramInput is one OTLP explicit-bounds HistogramDataPoint.
//
// Bounds are the explicit_bounds array and BucketCounts the bucket_counts
// array; the OTLP data model requires len(BucketCounts) == len(Bounds)+1, with
// the last bucket unbounded above.
type HistogramInput struct {
	HistogramCommon
	Bounds       []float64
	BucketCounts []uint64
}

// ExponentialHistogramInput is one OTLP ExponentialHistogramDataPoint.
type ExponentialHistogramInput struct {
	HistogramCommon
	Scale     int32
	ZeroCount uint64
	Positive  ExpBuckets
	Negative  ExpBuckets
}

// HistogramFold is the result of folding one histogram data point.
//
// Scalars are always populated for an accepted point. Sketch is nil exactly
// when PercentilesUnavailable is set: the two never disagree, because a caller
// that saw a non-nil sketch and an "unavailable" flag would eventually publish
// the sketch.
type HistogramFold struct {
	Sketch *Sketch
	Count  uint64
	Sum    float64
	HasSum bool
	Min    float64
	HasMin bool
	Max    float64
	HasMax bool

	// PercentilesUnavailable suppresses every quantile derived from this
	// point, and DropReason says why.
	PercentilesUnavailable bool
	DropReason             SketchDropReason

	// SourceBucketError is the worst-case relative error contributed by the
	// SOURCE histogram's own bucket widths, as a fraction. It is 0 for an
	// exponential histogram (index transfer is exact) and can dwarf the
	// scale-4 sketch's 2.17% for a coarse explicit-bounds histogram.
	SourceBucketError float64

	// UnboundedTail reports that observations landed in the +Inf bucket.
	// Those observations are deliberately NOT folded into the sketch: their
	// only known property is "greater than UnboundedTailBound", and placing
	// them at any finite value would turn a lower bound into a fabricated
	// estimate. UnboundedTailCount is how many.
	UnboundedTail      bool
	UnboundedTailBound float64
	UnboundedTailCount uint64
}

// flags renders the fold's metadata as the delta's histogram flags word.
func (f HistogramFold) flags() uint32 {
	var flags uint32
	if f.PercentilesUnavailable {
		flags |= HistPercentilesUnavailable
	}
	if f.UnboundedTail {
		flags |= HistUnboundedTail
	}
	if f.HasMin {
		flags |= HistHasMin
	}
	if f.HasMax {
		flags |= HistHasMax
	}
	return withHistReason(flags, f.DropReason)
}

// scalarsOnly marks the fold as accepted-without-percentiles.
func (f *HistogramFold) scalarsOnly(reason SketchDropReason) {
	f.Sketch = nil
	f.PercentilesUnavailable = true
	f.DropReason = reason
}

// foldScalars copies the point's scalar statistics onto a fold.
func foldScalars(c HistogramCommon) HistogramFold {
	return HistogramFold{
		Count:  c.Count,
		Sum:    c.Sum,
		HasSum: c.HasSum,
		Min:    c.Min,
		HasMin: c.HasMin,
		Max:    c.Max,
		HasMax: c.HasMax,
	}
}

// validateScalars rejects scalar statistics that cannot describe any
// population. A non-finite sum or an inverted min/max is a producer bug, and
// letting it into an additive delta poisons every merge downstream.
func validateScalars(c HistogramCommon) error {
	if c.HasSum && (math.IsNaN(c.Sum) || math.IsInf(c.Sum, 0)) {
		return fmt.Errorf("%w: non-finite sum", ErrHistogramMalformed)
	}
	if c.HasMin && (math.IsNaN(c.Min) || math.IsInf(c.Min, 0)) {
		return fmt.Errorf("%w: non-finite min", ErrHistogramMalformed)
	}
	if c.HasMax && (math.IsNaN(c.Max) || math.IsInf(c.Max, 0)) {
		return fmt.Errorf("%w: non-finite max", ErrHistogramMalformed)
	}
	if c.HasMin && c.HasMax && c.Min > c.Max {
		return fmt.Errorf("%w: min %g above max %g", ErrHistogramMalformed, c.Min, c.Max)
	}
	return nil
}

// maxExpBuckets bounds one side of an exponential histogram. A conforming
// producer needs a few thousand buckets in practice; anything past this cap is
// a corrupt or hostile payload and is refused before it is walked.
const maxExpBuckets = 1 << 16

// validateExpBuckets rejects an index range that cannot exist.
func validateExpBuckets(side string, b ExpBuckets) error {
	if len(b.Counts) > maxExpBuckets {
		return fmt.Errorf("%w: %s buckets %d above cap %d",
			ErrHistogramMalformed, side, len(b.Counts), maxExpBuckets)
	}
	if int64(b.Offset)+int64(len(b.Counts)) > math.MaxInt32 {
		return fmt.Errorf("%w: %s bucket index overflows int32", ErrHistogramMalformed, side)
	}
	return nil
}

// FoldExponentialHistogram converts one OTLP ExponentialHistogramDataPoint
// into the platform sketch (#199 Q1).
//
// Scale handling:
//   - s > 4: downscale exactly to 4 by an arithmetic right shift of each
//     bucket index, merging the counts that collapse together. OTel's
//     perfect-subsetting property makes this exact at the bucket-count level.
//   - s == 4: direct index transfer.
//   - 0 <= s < 4: the ACCUMULATED sketch is explicitly downscaled to s before
//     any bucket lands, so the result carries the source's coarser mapping
//     honestly. Relying on the sketch's incidental bin-collapse instead would
//     leave the advertised relative error a lie.
//   - s < 0: not representable by the sketch (its scale is unsigned). Scalars
//     are kept, percentiles are marked unavailable.
//
// zero_count folds into the sketch's zero bucket; count, sum, min and max fold
// normally. Negative buckets holding observations suppress percentiles for the
// WHOLE point: publishing the positive side's p99 as the distribution's p99
// would be a lie, and dropping the point entirely would lose a legitimate
// count.
func FoldExponentialHistogram(in ExponentialHistogramInput) (HistogramFold, error) {
	if err := validateScalars(in.HistogramCommon); err != nil {
		return HistogramFold{}, err
	}
	if in.Scale < ExpHistogramMinScale || in.Scale > ExpHistogramMaxScale {
		return HistogramFold{}, fmt.Errorf("%w: scale %d outside [%d,%d]",
			ErrHistogramMalformed, in.Scale, ExpHistogramMinScale, ExpHistogramMaxScale)
	}
	if err := validateExpBuckets("positive", in.Positive); err != nil {
		return HistogramFold{}, err
	}
	if err := validateExpBuckets("negative", in.Negative); err != nil {
		return HistogramFold{}, err
	}
	posTotal, ok := in.Positive.total()
	if !ok {
		return HistogramFold{}, fmt.Errorf("%w: positive bucket counts overflow", ErrHistogramMalformed)
	}
	negTotal, ok := in.Negative.total()
	if !ok {
		return HistogramFold{}, fmt.Errorf("%w: negative bucket counts overflow", ErrHistogramMalformed)
	}
	// The data model REQUIRES count == zero_count + sum(positive) +
	// sum(negative). A point that disagrees is describing a population it did
	// not measure, and there is no defensible way to guess which half is true.
	observed := in.ZeroCount + posTotal + negTotal
	if observed < posTotal || in.Count != observed {
		return HistogramFold{}, fmt.Errorf("%w: count %d disagrees with bucket total %d",
			ErrHistogramMalformed, in.Count, observed)
	}

	fold := foldScalars(in.HistogramCommon)
	if negTotal > 0 {
		fold.scalarsOnly(SketchDropNegativeObservations)
		return fold, nil
	}
	if in.Scale < 0 {
		fold.scalarsOnly(SketchDropScaleOutOfRange)
		return fold, nil
	}

	sk := NewSketch()
	scale := uint8(in.Scale) // #nosec G115 -- guarded to [0, ExpHistogramMaxScale] above
	var shift uint8
	switch {
	case scale > SketchDefaultScale:
		shift = scale - SketchDefaultScale
	case scale < SketchDefaultScale:
		// Explicit downscale of the accumulation target, per #199 Q1.
		sk.downscale(scale)
	}
	for i, c := range in.Positive.Counts {
		if c == 0 {
			continue
		}
		idx := in.Positive.Offset + int32(i) // #nosec G115 -- length capped at maxExpBuckets
		if shift > 0 {
			idx >>= shift
		}
		sk.ObserveBucket(idx, c)
	}
	sk.ObserveZero(in.ZeroCount)
	fold.Sketch = sk
	return fold, nil
}

// FoldHistogram converts one OTLP explicit-bounds HistogramDataPoint into the
// platform sketch (#199 Q2).
//
// Each finite bucket's count enters the sketch ONCE, as a weighted synthetic
// observation at the bucket's geometric midpoint. The geometric midpoint is
// only defined for a strictly positive bucket, so:
//
//   - A bucket whose lower boundary is below zero can only be folded when the
//     point's own min proves the population is non-negative; the effective
//     lower boundary then becomes max(0, boundary, min). Without that proof
//     the point keeps its scalars and loses its percentiles.
//   - A bucket whose effective lower boundary is exactly 0 has no geometric
//     midpoint. Its observations are placed at upper/2 and the bucket
//     contributes 100% source error, which is the truth: the observation could
//     have been anywhere in (0, upper].
//
// The +Inf bucket is never folded. Its count and the last finite boundary are
// carried out separately so a quantile that lands in the tail can be answered
// as a LOWER BOUND (p99 >= boundary) instead of an invented number.
func FoldHistogram(in HistogramInput) (HistogramFold, error) {
	if err := validateScalars(in.HistogramCommon); err != nil {
		return HistogramFold{}, err
	}
	if err := validateBounds(in); err != nil {
		return HistogramFold{}, err
	}

	fold := foldScalars(in.HistogramCommon)
	if in.Count == 0 || len(in.BucketCounts) == 0 {
		// Nothing observed: an empty sketch is an honest answer, and its
		// relative error is still the scale-4 bound.
		fold.Sketch = NewSketch()
		return fold, nil
	}
	if len(in.Bounds) == 0 {
		// One bucket spanning (-Inf, +Inf]. Every observation is in the
		// unbounded tail with no finite boundary to bound it by.
		fold.scalarsOnly(SketchDropNoFiniteBoundaries)
		return fold, nil
	}

	nonNegative := in.HasMin && in.Min >= 0
	lastIdx := len(in.BucketCounts) - 1
	sk := NewSketch()

	for i, c := range in.BucketCounts {
		if c == 0 {
			continue
		}
		if i == lastIdx {
			fold.UnboundedTail = true
			fold.UnboundedTailBound = in.Bounds[len(in.Bounds)-1]
			fold.UnboundedTailCount += c
			continue
		}
		upper := in.Bounds[i]
		lower := math.Inf(-1)
		if i > 0 {
			lower = in.Bounds[i-1]
		}
		if in.HasMin && in.Min > lower {
			lower = in.Min
		}
		if lower < 0 || upper <= 0 {
			// The bucket admits negative values. Only a reported min at or
			// above zero proves none were observed there; an upper boundary
			// at or below zero with a non-zero count contradicts such a min
			// outright.
			if !nonNegative || upper <= 0 {
				fold.scalarsOnly(SketchDropNegativeObservations)
				fold.UnboundedTail = false
				fold.UnboundedTailBound = 0
				fold.UnboundedTailCount = 0
				return fold, nil
			}
			lower = 0
		}
		mid, relErr := bucketMidpoint(lower, upper)
		if relErr > fold.SourceBucketError {
			fold.SourceBucketError = relErr
		}
		sk.ObserveN(mid, c)
	}
	fold.Sketch = sk
	return fold, nil
}

// bucketMidpoint returns the representative value of the finite bucket
// (lower, upper] and the worst-case relative error of placing every one of its
// observations there. lower is at or above zero and below upper.
func bucketMidpoint(lower, upper float64) (mid, relErr float64) {
	if lower <= 0 {
		// (0, upper]: no geometric midpoint exists. The true value can be
		// arbitrarily close to zero, so the arithmetic midpoint carries 100%
		// worst-case relative error.
		return upper / 2, 1
	}
	mid = math.Sqrt(lower * upper)
	// Placing an observation at the geometric midpoint of a bucket whose
	// boundary ratio is gamma is wrong by at most sqrt(gamma)-1 either way.
	return mid, math.Sqrt(upper/lower) - 1
}

// validateBounds enforces the explicit-bounds data model: bucket_counts is one
// longer than explicit_bounds, boundaries are finite and strictly ascending,
// and the bucket counts add up to count.
func validateBounds(in HistogramInput) error {
	switch {
	case len(in.BucketCounts) == 0 && len(in.Bounds) == 0:
		if in.Count != 0 {
			return fmt.Errorf("%w: count %d with no buckets", ErrHistogramMalformed, in.Count)
		}
		return nil
	case len(in.BucketCounts) != len(in.Bounds)+1:
		return fmt.Errorf("%w: %d bucket counts for %d bounds",
			ErrHistogramMalformed, len(in.BucketCounts), len(in.Bounds))
	}
	prev := math.Inf(-1)
	for i, b := range in.Bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) {
			return fmt.Errorf("%w: bound %d is not finite", ErrHistogramMalformed, i)
		}
		if b <= prev {
			return fmt.Errorf("%w: bound %d (%g) does not exceed its predecessor", ErrHistogramMalformed, i, b)
		}
		prev = b
	}
	var total uint64
	for _, c := range in.BucketCounts {
		next := total + c
		if next < total {
			return fmt.Errorf("%w: bucket counts overflow", ErrHistogramMalformed)
		}
		total = next
	}
	if total != in.Count {
		return fmt.Errorf("%w: count %d disagrees with bucket total %d", ErrHistogramMalformed, in.Count, total)
	}
	return nil
}
