package aggregate

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

// Latency range the sketch is dimensioned for: 1 microsecond to 100 seconds,
// expressed in seconds.
const (
	sketchMinLatency = 1e-6
	sketchMaxLatency = 100.0

	// sketchErrorSlack absorbs float rounding for values that sit exactly on a
	// bucket boundary, where the estimate lands exactly on the error bound.
	sketchErrorSlack = 1e-9
)

// sketchDistribution generates a deterministic latency sample.
type sketchDistribution struct {
	name string
	gen  func(r *rand.Rand) float64
}

func sketchDistributions() []sketchDistribution {
	return []sketchDistribution{
		{
			// Log-normal around 50 ms: the canonical request-latency shape.
			name: "lognormal",
			gen: func(r *rand.Rand) float64 {
				return math.Exp(math.Log(0.05) + 1.3*r.NormFloat64())
			},
		},
		{
			// Exponential with a 20 ms mean: memoryless service times.
			name: "exponential",
			gen: func(r *rand.Rand) float64 {
				return r.ExpFloat64() * 0.02
			},
		},
		{
			// Bimodal: a fast cached path plus a slow backend path.
			name: "bimodal",
			gen: func(r *rand.Rand) float64 {
				if r.Float64() < 0.8 {
					return math.Exp(math.Log(0.002) + 0.4*r.NormFloat64())
				}
				return math.Exp(math.Log(3.0) + 0.6*r.NormFloat64())
			},
		},
		{
			// Log-uniform across the entire 1us-100s range: worst case for
			// bucket occupancy, exercising ~426 of the 512 bins.
			name: "loguniform_full_range",
			gen: func(r *rand.Rand) float64 {
				lo, hi := math.Log2(sketchMinLatency), math.Log2(sketchMaxLatency)
				return math.Exp2(lo + r.Float64()*(hi-lo))
			},
		},
	}
}

// sketchSample draws n clamped samples from a distribution.
func sketchSample(gen func(r *rand.Rand) float64, n int, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	out := make([]float64, n)
	for i := range out {
		v := gen(r)
		if v < sketchMinLatency {
			v = sketchMinLatency
		}
		if v > sketchMaxLatency {
			v = sketchMaxLatency
		}
		out[i] = v
	}
	return out
}

// sketchExactQuantile returns the sample element the sketch's rank convention
// selects: the element at rank floor(q*(n-1)) of the sorted sample.
func sketchExactQuantile(values []float64, q float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := int(math.Floor(q * float64(len(sorted)-1)))
	return sorted[rank]
}

// sketchBins returns the populated bins keyed by bucket index.
func sketchBins(s *Sketch) map[int32]uint32 {
	out := make(map[int32]uint32)
	if !s.hasBins {
		return out
	}
	for i := s.minIdx; i <= s.maxIdx; i++ {
		if c := s.bins[i-s.minIdx]; c != 0 {
			out[i] = c
		}
	}
	return out
}

func sketchAssertSameContent(t *testing.T, want, got *Sketch) {
	t.Helper()
	if want.count != got.count || want.zeroCount != got.zeroCount || want.binTotal != got.binTotal {
		t.Fatalf("totals differ: want count=%d zero=%d bins=%d, got count=%d zero=%d bins=%d",
			want.count, want.zeroCount, want.binTotal, got.count, got.zeroCount, got.binTotal)
	}
	if want.collapsed != got.collapsed {
		t.Fatalf("collapsed differs: want %v, got %v", want.collapsed, got.collapsed)
	}
	if want.scale != got.scale {
		t.Fatalf("scale differs: want %d, got %d", want.scale, got.scale)
	}
	wb, gb := sketchBins(want), sketchBins(got)
	if len(wb) != len(gb) {
		t.Fatalf("populated bins differ: want %d, got %d", len(wb), len(gb))
	}
	for idx, c := range wb {
		if gb[idx] != c {
			t.Fatalf("bucket %d: want count %d, got %d", idx, c, gb[idx])
		}
	}
}

func TestSketchRelativeErrorBound(t *testing.T) {
	s := NewSketch()
	if got, want := s.Scale(), SketchDefaultScale; got != want {
		t.Fatalf("scale: got %d, want %d", got, want)
	}
	// (2^(1/16)-1)/(2^(1/16)+1) = 0.02165746...
	if got := s.RelativeError(); math.Abs(got-0.02165746) > 1e-7 {
		t.Fatalf("scale-4 relative error: got %.9f, want ~0.02165746", got)
	}
	coarse, err := NewSketchAtScale(2)
	if err != nil {
		t.Fatalf("NewSketchAtScale: %v", err)
	}
	if got := coarse.RelativeError(); math.Abs(got-0.08642723) > 1e-7 {
		t.Fatalf("scale-2 relative error: got %.9f, want ~0.08642723", got)
	}
	if _, err := NewSketchAtScale(SketchMaxScale + 1); err == nil {
		t.Fatal("expected an error for a scale above SketchMaxScale")
	}
}

func TestSketchIndexBucketContainment(t *testing.T) {
	base := sketchBase(SketchDefaultScale)
	r := rand.New(rand.NewSource(7))
	for range 20000 {
		lo, hi := math.Log2(sketchMinLatency), math.Log2(sketchMaxLatency)
		v := math.Exp2(lo + r.Float64()*(hi-lo))
		idx := sketchIndex(v, SketchDefaultScale)
		lower := math.Exp2(math.Ldexp(float64(idx), -int(SketchDefaultScale)))
		upper := lower * base
		if v <= lower*(1-1e-12) || v > upper*(1+1e-12) {
			t.Fatalf("value %g mapped to bucket %d covering (%g, %g]", v, idx, lower, upper)
		}
	}
}

func TestSketchIndexPowersOfTwo(t *testing.T) {
	// An exact power of two is the inclusive upper boundary of the bucket
	// below its boundary index.
	for exp := -20; exp <= 7; exp++ {
		v := math.Exp2(float64(exp))
		want := int32(exp)<<SketchDefaultScale - 1
		if got := sketchIndex(v, SketchDefaultScale); got != want {
			t.Fatalf("sketchIndex(2^%d) = %d, want %d", exp, got, want)
		}
	}
}

func TestSketchQuantileAccuracy(t *testing.T) {
	quantiles := []float64{0.5, 0.95, 0.99}
	const samples = 200000

	for _, d := range sketchDistributions() {
		t.Run(d.name, func(t *testing.T) {
			values := sketchSample(d.gen, samples, 20260821)
			s := NewSketch()
			for _, v := range values {
				s.Observe(v)
			}
			if s.Collapsed() {
				t.Fatalf("sketch collapsed on a 1us-100s sample; bins=%d", s.PopulatedBins())
			}
			if s.Count() != samples {
				t.Fatalf("count: got %d, want %d", s.Count(), samples)
			}
			bound := s.RelativeError()
			for _, q := range quantiles {
				want := sketchExactQuantile(values, q)
				got := s.Quantile(q)
				relErr := math.Abs(got-want) / want
				if relErr > bound+sketchErrorSlack {
					t.Errorf("q%.2f: estimate %g vs exact %g, relative error %.6f exceeds bound %.6f",
						q, got, want, relErr, bound)
				}
				t.Logf("%s q%.2f exact=%.9g est=%.9g relerr=%.5f%% (bound %.5f%%, bins=%d)",
					d.name, q, want, got, relErr*100, bound*100, s.PopulatedBins())
			}
		})
	}
}

func TestSketchQuantileEdgeCases(t *testing.T) {
	empty := NewSketch()
	if got := empty.Quantile(0.5); got != 0 {
		t.Fatalf("empty sketch quantile: got %g, want 0", got)
	}
	if empty.Count() != 0 || empty.PopulatedBins() != 0 || empty.Sum() != 0 {
		t.Fatal("empty sketch has non-zero state")
	}

	single := NewSketch()
	single.Observe(0.25)
	for _, q := range []float64{0, 0.5, 1} {
		got := single.Quantile(q)
		if relErr := math.Abs(got-0.25) / 0.25; relErr > single.RelativeError()+sketchErrorSlack {
			t.Fatalf("single observation q%.2f: got %g, relative error %.6f", q, got, relErr)
		}
	}
	if single.Count() != 1 || single.Sum() != 0.25 {
		t.Fatalf("single observation: count=%d sum=%g", single.Count(), single.Sum())
	}
	if got := single.Quantile(1.5); !math.IsNaN(got) {
		t.Fatalf("out-of-range quantile: got %g, want NaN", got)
	}
	if got := single.Quantile(math.NaN()); !math.IsNaN(got) {
		t.Fatalf("NaN quantile: got %g, want NaN", got)
	}
}

func TestSketchZeroAndNonFiniteObservations(t *testing.T) {
	s := NewSketch()
	s.Observe(0)
	s.Observe(-1)
	s.Observe(math.NaN())
	s.Observe(math.Inf(1))
	s.Observe(math.Inf(-1))

	if s.Count() != 2 {
		t.Fatalf("count: got %d, want 2 (non-finite values rejected)", s.Count())
	}
	if s.ZeroCount() != 2 {
		t.Fatalf("zero count: got %d, want 2", s.ZeroCount())
	}
	if s.PopulatedBins() != 0 {
		t.Fatalf("populated bins: got %d, want 0", s.PopulatedBins())
	}
	if s.Sum() != -1 {
		t.Fatalf("sum: got %g, want -1", s.Sum())
	}
	if got := s.Quantile(0.5); got != 0 {
		t.Fatalf("quantile of zero-bucket-only sketch: got %g, want 0", got)
	}

	// The zero bucket participates in the rank: with 2 zeros and 2 positive
	// values, the lower half of the distribution is still zero.
	s.Observe(1.0)
	s.Observe(2.0)
	if got := s.Quantile(0.25); got != 0 {
		t.Fatalf("q0.25: got %g, want 0", got)
	}
	if got := s.Quantile(0.9); math.Abs(got-1)/1 > s.RelativeError()+sketchErrorSlack {
		t.Fatalf("q0.90: got %g, want ~1", got)
	}
	if got := s.Quantile(1); math.Abs(got-2)/2 > s.RelativeError()+sketchErrorSlack {
		t.Fatalf("q1.00: got %g, want ~2", got)
	}
}

func TestSketchCollapseLowestOnRangeOverflow(t *testing.T) {
	s := NewSketch()
	// Two values 2^40 apart require 640 bins at scale 4; only 512 exist.
	s.Observe(1e-9)
	s.Observe(1e-9 * math.Exp2(40))

	if !s.Collapsed() {
		t.Fatal("expected collapse-lowest to fire")
	}
	if s.Count() != 2 || s.binTotal != 2 {
		t.Fatalf("collapse lost observations: count=%d binTotal=%d", s.Count(), s.binTotal)
	}
	if span := s.maxIdx - s.minIdx; span >= SketchMaxBins {
		t.Fatalf("window span %d exceeds %d bins", span+1, SketchMaxBins)
	}
	// The high tail is unaffected: p99 must still be within the bound.
	want := 1e-9 * math.Exp2(40)
	if relErr := math.Abs(s.Quantile(1)-want) / want; relErr > s.RelativeError()+sketchErrorSlack {
		t.Fatalf("tail quantile after collapse: relative error %.6f", relErr)
	}
}

func TestSketchCollapseIsOrderIndependent(t *testing.T) {
	values := []float64{1e-9, 5e-4, 0.02, 3.7, 1e-9 * math.Exp2(40), 12.5, 1e-7}

	forward := NewSketch()
	for _, v := range values {
		forward.Observe(v)
	}
	reverse := NewSketch()
	for i := len(values) - 1; i >= 0; i-- {
		reverse.Observe(values[i])
	}
	shuffled := NewSketch()
	order := []int{4, 0, 6, 2, 5, 1, 3}
	for _, i := range order {
		shuffled.Observe(values[i])
	}

	if !forward.Collapsed() {
		t.Fatal("expected the sample to collapse")
	}
	sketchAssertSameContent(t, forward, reverse)
	sketchAssertSameContent(t, forward, shuffled)
}

func TestSketchMergeCommutativeAndAssociative(t *testing.T) {
	parts := make([][]float64, 3)
	for i := range parts {
		parts[i] = sketchSample(sketchDistributions()[0].gen, 5000, int64(100+i))
	}
	build := func(values []float64) *Sketch {
		s := NewSketch()
		for _, v := range values {
			s.Observe(v)
		}
		return s
	}

	// (a+b)+c
	left := build(parts[0])
	left.Merge(build(parts[1]))
	left.Merge(build(parts[2]))

	// a+(b+c)
	bc := build(parts[1])
	bc.Merge(build(parts[2]))
	right := build(parts[0])
	right.Merge(bc)

	// c+(b+a) - reversed order at every level
	ba := build(parts[1])
	ba.Merge(build(parts[0]))
	reversed := build(parts[2])
	reversed.Merge(ba)

	sketchAssertSameContent(t, left, right)
	sketchAssertSameContent(t, left, reversed)
}

func TestSketchMergeEqualsOneShot(t *testing.T) {
	values := sketchSample(sketchDistributions()[3].gen, 60000, 999)

	oneShot := NewSketch()
	for _, v := range values {
		oneShot.Observe(v)
	}

	// Partition into shuffled, uneven chunks and merge them in shuffled order.
	r := rand.New(rand.NewSource(4242))
	perm := r.Perm(len(values))
	shuffled := make([]float64, len(values))
	for i, p := range perm {
		shuffled[i] = values[p]
	}
	var partials []*Sketch
	for start := 0; start < len(shuffled); {
		size := 1 + r.Intn(4000)
		end := min(start+size, len(shuffled))
		part := NewSketch()
		for _, v := range shuffled[start:end] {
			part.Observe(v)
		}
		partials = append(partials, part)
		start = end
	}
	merged := NewSketch()
	for _, p := range partials {
		merged.Merge(p)
	}

	sketchAssertSameContent(t, oneShot, merged)
	for _, q := range []float64{0.5, 0.95, 0.99} {
		if a, b := oneShot.Quantile(q), merged.Quantile(q); a != b {
			t.Fatalf("q%.2f: one-shot %g, merged %g", q, a, b)
		}
	}
	if math.Abs(merged.Sum()-oneShot.Sum())/oneShot.Sum() > 1e-12 {
		t.Fatalf("sum drift: one-shot %g, merged %g", oneShot.Sum(), merged.Sum())
	}
}

func TestSketchMergeEmptyAndNil(t *testing.T) {
	s := NewSketch()
	s.Observe(0.5)
	before := *s

	s.Merge(nil)
	s.Merge(NewSketch())
	sketchAssertSameContent(t, &before, s)

	empty := NewSketch()
	empty.Merge(s)
	sketchAssertSameContent(t, s, empty)
}

func TestSketchDownscaleIsExact(t *testing.T) {
	values := sketchSample(sketchDistributions()[0].gen, 50000, 31337)
	fine := NewSketch()
	for _, v := range values {
		fine.Observe(v)
	}

	coarse := *fine
	if err := coarse.Downscale(2); err != nil {
		t.Fatalf("Downscale: %v", err)
	}
	if coarse.Scale() != 2 {
		t.Fatalf("scale after downscale: got %d, want 2", coarse.Scale())
	}
	if coarse.count != fine.count || coarse.binTotal != fine.binTotal {
		t.Fatalf("downscale lost observations: %d/%d vs %d/%d",
			coarse.count, coarse.binTotal, fine.count, fine.binTotal)
	}
	if coarse.PopulatedBins() > fine.PopulatedBins() {
		t.Fatal("downscale increased the populated bin count")
	}

	// Downscaling folds four scale-4 buckets into one scale-2 bucket: every
	// count must land in the bucket its index shift selects.
	want := make(map[int32]uint32)
	for idx, c := range sketchBins(fine) {
		want[idx>>2] += c
	}
	got := sketchBins(&coarse)
	if len(want) != len(got) {
		t.Fatalf("folded bins: want %d, got %d", len(want), len(got))
	}
	for idx, c := range want {
		if got[idx] != c {
			t.Fatalf("bucket %d: want %d, got %d", idx, c, got[idx])
		}
	}

	if err := coarse.Downscale(4); err == nil {
		t.Fatal("expected upscaling to be rejected")
	}

	// Accuracy is now the coarser scale's bound, and it holds.
	for _, q := range []float64{0.5, 0.95, 0.99} {
		exact := sketchExactQuantile(values, q)
		relErr := math.Abs(coarse.Quantile(q)-exact) / exact
		if relErr > coarse.RelativeError()+sketchErrorSlack {
			t.Errorf("q%.2f after downscale: relative error %.6f exceeds bound %.6f",
				q, relErr, coarse.RelativeError())
		}
		t.Logf("scale-2 q%.2f relerr=%.5f%% (bound %.5f%%)", q, relErr*100, coarse.RelativeError()*100)
	}
}

func TestSketchMergeAlignsScales(t *testing.T) {
	values := sketchSample(sketchDistributions()[1].gen, 40000, 5150)
	half := len(values) / 2

	fine := NewSketch()
	for _, v := range values[:half] {
		fine.Observe(v)
	}
	coarse, err := NewSketchAtScale(2)
	if err != nil {
		t.Fatalf("NewSketchAtScale: %v", err)
	}
	for _, v := range values[half:] {
		coarse.Observe(v)
	}

	// Merging in either direction lands on the coarser scale with the same
	// content: alignment is a downscale of the finer side, which is exact.
	a := *fine
	a.Merge(coarse)
	b := *coarse
	b.Merge(fine)
	if a.Scale() != 2 || b.Scale() != 2 {
		t.Fatalf("aligned scale: got %d and %d, want 2", a.Scale(), b.Scale())
	}
	sketchAssertSameContent(t, &a, &b)
	if a.Count() != uint64(len(values)) {
		t.Fatalf("count after mixed-scale merge: got %d, want %d", a.Count(), len(values))
	}

	for _, q := range []float64{0.5, 0.95, 0.99} {
		exact := sketchExactQuantile(values, q)
		relErr := math.Abs(a.Quantile(q)-exact) / exact
		if relErr > a.RelativeError()+sketchErrorSlack {
			t.Errorf("q%.2f after mixed-scale merge: relative error %.6f exceeds bound %.6f",
				q, relErr, a.RelativeError())
		}
	}
}

func TestSketchSaturatingAdds(t *testing.T) {
	s := NewSketch()
	s.Observe(0.1)
	idx := sketchIndex(0.1, SketchDefaultScale)

	s.add(idx, math.MaxUint32-1)
	if s.Saturations() != 0 {
		t.Fatalf("premature saturation: %d", s.Saturations())
	}
	if s.bins[idx-s.minIdx] != math.MaxUint32 {
		t.Fatalf("bin: got %d, want MaxUint32", s.bins[idx-s.minIdx])
	}

	// One more observation must clamp rather than wrap.
	s.add(idx, 10)
	if s.Saturations() != 1 {
		t.Fatalf("saturation counter: got %d, want 1", s.Saturations())
	}
	if got := s.bins[idx-s.minIdx]; got != math.MaxUint32 {
		t.Fatalf("bin wrapped: got %d, want MaxUint32", got)
	}
	if s.binTotal != math.MaxUint32 {
		t.Fatalf("retained observations: got %d, want MaxUint32", s.binTotal)
	}

	// Quantiles stay usable, they are only degraded.
	if got := s.Quantile(0.5); math.Abs(got-0.1)/0.1 > s.RelativeError()+sketchErrorSlack {
		t.Fatalf("quantile after saturation: got %g", got)
	}

	// Saturation counters accumulate across a merge.
	other := NewSketch()
	other.Observe(0.1)
	other.add(idx, math.MaxUint32)
	if other.Saturations() != 1 {
		t.Fatalf("other saturation counter: got %d, want 1", other.Saturations())
	}
	s.Merge(other)
	if s.Saturations() < 3 {
		t.Fatalf("merged saturation counter: got %d, want >= 3", s.Saturations())
	}
	if s.bins[idx-s.minIdx] != math.MaxUint32 {
		t.Fatal("merge wrapped a saturated bin")
	}
}

func TestSketchObserveDoesNotAllocate(t *testing.T) {
	s := NewSketch()
	values := sketchSample(sketchDistributions()[3].gen, 1024, 8080)
	i := 0
	allocs := testing.AllocsPerRun(20000, func() {
		s.Observe(values[i&1023])
		i++
	})
	if allocs != 0 {
		t.Fatalf("Observe allocated %.2f times per call, want 0", allocs)
	}
}
