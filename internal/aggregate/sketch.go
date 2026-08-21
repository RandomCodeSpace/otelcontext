// Package aggregate implements the aggregate accounting engine.
package aggregate

import (
	"errors"
	"fmt"
	"math"
)

// Sketch parameters. The mapping is the OTLP exponential-histogram base-2
// function at a fixed platform scale: bucket i covers (base^i, base^(i+1)]
// with base = 2^(2^-scale). At the platform default scale of 4 the base is
// 2^(1/16) ~= 1.044274 and the worst-case relative error of a quantile
// estimate is (base-1)/(base+1) ~= 2.17%.
//
// Choosing the OTel mapping rather than an arbitrary DDSketch gamma makes a
// scale change an exact integer index shift ("perfect subsetting"), which is
// what allows Downscale and mismatched-scale Merge to stay lossless in the
// mapping. See docs/research/latency-sketch.md and issue #157.
const (
	// SketchDefaultScale is the platform-wide mapping scale.
	SketchDefaultScale uint8 = 4

	// SketchMaxScale is the finest scale the codec accepts. The OTel data model
	// defines positive scales up to 20.
	SketchMaxScale uint8 = 20

	// SketchMaxBins bounds the in-memory dense bin array. 512 bins at scale 4
	// span a 2^32 dynamic range (~9.6 decades); the theoretical requirement for
	// 1us-100s is 426 bins.
	SketchMaxBins int32 = 512
)

// ErrSketchScale is returned when a scale outside the supported range is
// requested or decoded.
var ErrSketchScale = errors.New("aggregate: unsupported sketch scale")

// Sketch is a fixed-size relative-error quantile sketch for latency values.
//
// The zero value is not usable; construct with NewSketch or NewSketchAtScale.
// A Sketch contains no pointers and no slices: it is copyable by assignment and
// Observe performs no allocation.
//
// Sketch is not safe for concurrent use. Callers own the synchronization.
type Sketch struct {
	// sum is the sum of every observed value, including zero-bucket values.
	sum float64
	// count is the total number of observations, including the zero bucket and
	// including observations whose bin saturated.
	count uint64
	// zeroCount holds observations that are not representable in the log
	// mapping: zero and (by construction, buggy) negative latencies.
	zeroCount uint64
	// binTotal is the number of observations actually retained in bins. It is
	// below count-zeroCount only when a bin has saturated.
	binTotal uint64
	// saturations counts the number of adds that clamped at MaxUint32.
	saturations uint64
	// minIdx is the bucket index of bins[0]. It bounds the populated window and
	// is not necessarily a populated bin.
	minIdx int32
	// maxIdx is the highest bucket index in the window. It is always populated
	// while hasBins is true.
	maxIdx int32
	scale  uint8
	// collapsed records that counts were merged into the lowest retained bin,
	// so estimates below that bin are no longer within the relative-error bound.
	collapsed bool
	hasBins   bool
	bins      [SketchMaxBins]uint32
}

// NewSketch returns an empty sketch at the platform default scale.
func NewSketch() *Sketch {
	return &Sketch{scale: SketchDefaultScale}
}

// NewSketchAtScale returns an empty sketch at an explicit scale. Scales above
// SketchMaxScale are rejected.
func NewSketchAtScale(scale uint8) (*Sketch, error) {
	if scale > SketchMaxScale {
		return nil, fmt.Errorf("%w: %d", ErrSketchScale, scale)
	}
	return &Sketch{scale: scale}, nil
}

// Scale returns the mapping scale of the sketch.
func (s *Sketch) Scale() uint8 { return s.scale }

// Count returns the total number of observations, including zero-bucket values.
func (s *Sketch) Count() uint64 { return s.count }

// ZeroCount returns the number of observations that landed in the zero bucket.
func (s *Sketch) ZeroCount() uint64 { return s.zeroCount }

// Sum returns the sum of all observed values.
func (s *Sketch) Sum() float64 { return s.sum }

// Saturations returns the number of bin adds that clamped at MaxUint32.
// Quantile estimates from a saturated sketch are degraded, not corrupted.
func (s *Sketch) Saturations() uint64 { return s.saturations }

// Collapsed reports whether counts were merged into the lowest retained bin,
// either by range overflow or by the serialized bin cap.
func (s *Sketch) Collapsed() bool { return s.collapsed }

// RelativeError returns the worst-case relative error of a quantile estimate at
// the sketch's current scale, ignoring degradation from collapse or saturation.
// This is the accuracy the sketch advertises to callers.
func (s *Sketch) RelativeError() float64 {
	gamma := sketchBase(s.scale)
	return (gamma - 1) / (gamma + 1)
}

// PopulatedBins returns the number of bins holding a non-zero count.
func (s *Sketch) PopulatedBins() int {
	if !s.hasBins {
		return 0
	}
	n := 0
	for i := s.minIdx; i <= s.maxIdx; i++ {
		if s.bins[i-s.minIdx] != 0 {
			n++
		}
	}
	return n
}

// Observe records one value. Non-finite values are rejected and do not affect
// any counter. Values at or below zero go to the zero bucket: latency is
// non-negative by construction, so a negative value is a caller bug that must
// not be allowed to poison the mapping.
//
// Observe never allocates.
func (s *Sketch) Observe(value float64) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	s.count++
	s.sum += value
	if value <= 0 {
		s.zeroCount++
		return
	}
	s.add(sketchIndex(value, s.scale), 1)
}

// Quantile returns the estimated q-quantile of the observed values, for q in
// [0,1]. An empty sketch returns 0; an out-of-range or NaN q returns NaN.
//
// The returned value is within RelativeError of the exact q-quantile of the
// observed sample, provided the sketch has not collapsed below that quantile
// and no bin has saturated.
func (s *Sketch) Quantile(q float64) float64 {
	if math.IsNaN(q) || q < 0 || q > 1 {
		return math.NaN()
	}
	total := s.zeroCount + s.binTotal
	if total == 0 {
		return 0
	}
	// Rank convention: the smallest bucket whose cumulative count exceeds
	// q*(n-1) holds the element at that rank.
	rank := q * float64(total-1)
	if float64(s.zeroCount) > rank {
		return 0
	}
	if !s.hasBins {
		return 0
	}
	cum := s.zeroCount
	for i := s.minIdx; i <= s.maxIdx; i++ {
		c := s.bins[i-s.minIdx]
		if c == 0 {
			continue
		}
		cum += uint64(c)
		if float64(cum) > rank {
			return sketchValue(i, s.scale)
		}
	}
	return sketchValue(s.maxIdx, s.scale)
}

// Merge folds other into s. Totals add, bins add bin-wise, and the result is
// independent of the order in which sketches are merged.
//
// Scales are aligned before merging by downscaling the finer sketch, which is
// exact; the result carries the coarser of the two scales. Merging a nil or
// empty sketch is a no-op.
func (s *Sketch) Merge(other *Sketch) {
	if other == nil || other.count == 0 {
		return
	}
	src := other
	switch {
	case other.scale > s.scale:
		aligned := *other
		aligned.downscale(s.scale)
		src = &aligned
	case other.scale < s.scale:
		s.downscale(other.scale)
	}

	s.count += src.count
	s.sum += src.sum
	s.zeroCount += src.zeroCount
	s.saturations += src.saturations
	if src.collapsed {
		s.collapsed = true
	}
	if !src.hasBins {
		return
	}
	for i := src.minIdx; i <= src.maxIdx; i++ {
		if c := src.bins[i-src.minIdx]; c != 0 {
			s.add(i, uint64(c))
		}
	}
}

// Downscale converts the sketch to a coarser scale. The conversion is exact:
// every bucket at the current scale maps onto exactly one bucket at the target
// scale via an arithmetic right shift of the index, so no count crosses a
// boundary. The relative-error bound becomes that of the coarser scale.
//
// Downscaling to the current scale is a no-op; a finer target is rejected.
func (s *Sketch) Downscale(target uint8) error {
	if target > s.scale {
		return fmt.Errorf("%w: cannot upscale %d to %d", ErrSketchScale, s.scale, target)
	}
	s.downscale(target)
	return nil
}

func (s *Sketch) downscale(target uint8) {
	if target >= s.scale {
		return
	}
	shift := s.scale - target
	s.scale = target
	if !s.hasBins {
		return
	}

	newMin := s.minIdx >> shift
	newMax := s.maxIdx >> shift
	var folded [SketchMaxBins]uint32
	for i := s.minIdx; i <= s.maxIdx; i++ {
		c := s.bins[i-s.minIdx]
		if c == 0 {
			continue
		}
		pos := (i >> shift) - newMin
		total := uint64(folded[pos]) + uint64(c)
		if total > math.MaxUint32 {
			folded[pos] = math.MaxUint32
			s.binTotal -= total - math.MaxUint32
			s.saturations++
			continue
		}
		folded[pos] = uint32(total)
	}
	s.bins = folded
	s.minIdx, s.maxIdx = newMin, newMax
}

// add applies n observations to a bucket index, growing or collapsing the
// window as needed. The resulting state depends only on the multiset of
// observations, never on their order: every observation below
// maxIdx-SketchMaxBins+1 ends up folded into that lowest retained bin.
func (s *Sketch) add(index int32, n uint64) {
	if n == 0 {
		return
	}
	if !s.hasBins {
		s.hasBins = true
		s.minIdx, s.maxIdx = index, index
		s.binTotal += s.addAt(0, n)
		return
	}

	switch {
	case index > s.maxIdx:
		if index-s.minIdx >= SketchMaxBins {
			s.collapseTo(index - SketchMaxBins + 1)
		}
		s.maxIdx = index
	case index < s.minIdx:
		lowest := s.maxIdx - SketchMaxBins + 1
		if index < lowest {
			s.extendTo(lowest)
			index = lowest
			s.collapsed = true
		} else {
			s.extendTo(index)
		}
	}
	s.binTotal += s.addAt(index-s.minIdx, n)
}

// addAt applies a saturating add to the bin at a physical position and returns
// the number of observations actually stored, which is below n only when the
// bin clamps. Counts clamp at MaxUint32 and are never allowed to wrap: a
// clamped bin is a degraded estimate, a wrapped bin is corruption.
//
// The caller owns binTotal, because moving counts between bins must not change
// it while a saturating move must.
func (s *Sketch) addAt(pos int32, n uint64) uint64 {
	cur := uint64(s.bins[pos])
	if total := cur + n; total <= math.MaxUint32 {
		s.bins[pos] = uint32(total)
		return n
	}
	s.bins[pos] = math.MaxUint32
	s.saturations++
	return math.MaxUint32 - cur
}

// extendTo lowers the window floor to newMin, shifting the populated bins up.
// The caller guarantees newMin < minIdx and maxIdx-newMin < SketchMaxBins.
func (s *Sketch) extendTo(newMin int32) {
	shift := s.minIdx - newMin
	used := s.maxIdx - s.minIdx + 1
	copy(s.bins[shift:shift+used], s.bins[:used])
	clear(s.bins[:shift])
	s.minIdx = newMin
}

// collapseTo raises the window floor to newMin, merging every count below it
// into the bin at newMin (DDSketch collapse-lowest). The caller guarantees
// newMin > minIdx.
func (s *Sketch) collapseTo(newMin int32) {
	shift := newMin - s.minIdx
	used := s.maxIdx - s.minIdx + 1

	var merged uint64
	for i := int32(0); i < shift && i < used; i++ {
		merged += uint64(s.bins[i])
	}

	if shift >= used {
		clear(s.bins[:used])
		s.minIdx, s.maxIdx = newMin, newMin
	} else {
		copy(s.bins[:used-shift], s.bins[shift:used])
		clear(s.bins[used-shift : used])
		s.minIdx = newMin
	}

	if merged > 0 {
		s.collapsed = true
		// The counts already belong to binTotal; only a clamp loses any.
		s.binTotal -= merged - s.addAt(0, merged)
	}
}

// collapseToBinCap collapses the lowest bins until at most limit bins are
// populated, so the sketch fits the serialized bin budget. Totals are preserved.
func (s *Sketch) collapseToBinCap(limit int) {
	if s.PopulatedBins() <= limit {
		return
	}
	seen := 0
	floor := s.minIdx
	for i := s.maxIdx; i >= s.minIdx; i-- {
		if s.bins[i-s.minIdx] == 0 {
			continue
		}
		seen++
		if seen == limit {
			floor = i
			break
		}
	}
	s.collapseTo(floor)
}

// sketchBase returns the mapping base gamma = 2^(2^-scale).
func sketchBase(scale uint8) float64 {
	return math.Exp2(math.Ldexp(1, -int(scale)))
}

// sketchScaleFactor returns 1/ln(base), the multiplier that turns a natural
// logarithm into a scaled base-2 logarithm.
func sketchScaleFactor(scale uint8) float64 {
	return math.Ldexp(math.Log2E, int(scale))
}

// sketchIndex maps a strictly positive value to its bucket index using the OTel
// exponential-histogram mapping ceil(log_base(value)) - 1, so that bucket i
// covers (base^i, base^(i+1)].
func sketchIndex(value float64, scale uint8) int32 {
	if frac, exp := math.Frexp(value); frac == 0.5 {
		// Exact power of two: value is the upper boundary of the bucket below
		// the boundary index, and boundaries are inclusive on the upper side.
		return (int32(exp-1) << scale) - 1 // #nosec G115 -- Frexp exponent of a finite float64 is within [-1074, 1024]
	}
	return int32(math.Ceil(math.Log(value)*sketchScaleFactor(scale))) - 1
}

// sketchValue returns the representative value of a bucket: 2*upper/(base+1),
// which sits equidistant in relative terms from both boundaries and so attains
// the (base-1)/(base+1) worst-case relative error.
func sketchValue(index int32, scale uint8) float64 {
	upper := math.Exp2(math.Ldexp(float64(index+1), -int(scale)))
	return 2 * upper / (sketchBase(scale) + 1)
}
