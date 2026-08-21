package aggregate

import "testing"

func BenchmarkSketchObserve(b *testing.B) {
	values := sketchSample(sketchDistributions()[0].gen, 4096, 1)
	s := NewSketch()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		s.Observe(values[i&4095])
	}
}

// BenchmarkSketchObserveFullRange spans the entire 1us-100s range, so the hot
// path repeatedly grows and collapses the bin window.
func BenchmarkSketchObserveFullRange(b *testing.B) {
	values := sketchSample(sketchDistributions()[3].gen, 4096, 2)
	s := NewSketch()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		s.Observe(values[i&4095])
	}
}

func BenchmarkSketchMerge(b *testing.B) {
	build := func(seed int64) *Sketch {
		s := NewSketch()
		for _, v := range sketchSample(sketchDistributions()[0].gen, 20000, seed) {
			s.Observe(v)
		}
		return s
	}
	src := build(3)
	dst := build(4)
	base := *dst

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		*dst = base
		dst.Merge(src)
	}
}

func BenchmarkSketchEncode(b *testing.B) {
	s := NewSketch()
	for _, v := range sketchSample(sketchDistributions()[0].gen, 20000, 5) {
		s.Observe(v)
	}
	buf := make([]byte, 0, 2048)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buf = s.AppendTo(buf[:0])
	}
	b.StopTimer()
	b.ReportMetric(float64(s.PopulatedBins()), "bins")
	b.ReportMetric(float64(len(buf)), "encoded_B")
}

func BenchmarkSketchDecode(b *testing.B) {
	s := NewSketch()
	for _, v := range sketchSample(sketchDistributions()[0].gen, 20000, 6) {
		s.Observe(v)
	}
	encoded := s.Encode()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := DecodeSketch(encoded); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSketchQuantile(b *testing.B) {
	s := NewSketch()
	for _, v := range sketchSample(sketchDistributions()[0].gen, 20000, 7) {
		s.Observe(v)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = s.Quantile(0.99)
	}
}
