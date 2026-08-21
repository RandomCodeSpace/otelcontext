package aggregate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// sketchWithBins returns a sketch populated across n consecutive buckets, one
// observation per bucket, plus a few zero-bucket values.
func sketchWithBins(t *testing.T, n int) *Sketch {
	t.Helper()
	s := NewSketch()
	for i := range n {
		// Bucket midpoints at scale 4, so each value lands in its own bucket.
		s.Observe(math.Exp2((float64(i) + 0.5) / 16))
	}
	if got := s.PopulatedBins(); got != n {
		t.Fatalf("setup: populated bins %d, want %d", got, n)
	}
	return s
}

func TestSketchCodecRoundTrip(t *testing.T) {
	values := sketchSample(sketchDistributions()[0].gen, 20000, 616)
	s := NewSketch()
	for _, v := range values {
		s.Observe(v)
	}
	s.Observe(0)
	s.Observe(-3)

	encoded := s.Encode()
	decoded, err := DecodeSketch(encoded)
	if err != nil {
		t.Fatalf("DecodeSketch: %v", err)
	}

	sketchAssertSameContent(t, s, decoded)
	if decoded.Sum() != s.Sum() {
		t.Fatalf("sum: got %g, want %g", decoded.Sum(), s.Sum())
	}
	if decoded.Scale() != s.Scale() {
		t.Fatalf("scale: got %d, want %d", decoded.Scale(), s.Scale())
	}
	for _, q := range []float64{0, 0.5, 0.95, 0.99, 1} {
		if a, b := s.Quantile(q), decoded.Quantile(q); a != b {
			t.Fatalf("q%.2f: original %g, decoded %g", q, a, b)
		}
	}

	// Deterministic: re-encoding a decoded sketch reproduces the same bytes.
	if again := decoded.Encode(); !bytes.Equal(encoded, again) {
		t.Fatalf("encode(decode(encode(x))) differs: %d vs %d bytes", len(encoded), len(again))
	}
	// And encoding the same state twice is stable.
	if !bytes.Equal(encoded, s.Encode()) {
		t.Fatal("repeated Encode produced different bytes")
	}

	t.Logf("%d observations, %d populated bins, %d encoded bytes (%.2f B/bin)",
		s.Count(), s.PopulatedBins(), len(encoded),
		float64(len(encoded))/float64(s.PopulatedBins()))
}

func TestSketchCodecEmptyRoundTrip(t *testing.T) {
	s := NewSketch()
	encoded := s.Encode()
	decoded, err := DecodeSketch(encoded)
	if err != nil {
		t.Fatalf("DecodeSketch: %v", err)
	}
	sketchAssertSameContent(t, s, decoded)
	if !bytes.Equal(encoded, decoded.Encode()) {
		t.Fatal("empty sketch does not re-encode identically")
	}
	if got := decoded.Quantile(0.5); got != 0 {
		t.Fatalf("decoded empty quantile: got %g, want 0", got)
	}
}

func TestSketchCodecAppendToPreservesPrefix(t *testing.T) {
	s := NewSketch()
	s.Observe(0.42)
	prefix := []byte{0xde, 0xad}
	out := s.AppendTo(prefix)
	if !bytes.Equal(out[:2], prefix) {
		t.Fatal("AppendTo overwrote the destination prefix")
	}
	if !bytes.Equal(out[2:], s.Encode()) {
		t.Fatal("AppendTo and Encode disagree")
	}
}

func TestSketchCodecCollapsesToSerializedCap(t *testing.T) {
	const populated = 400
	s := sketchWithBins(t, populated)
	wantCount, wantSum := s.Count(), s.Sum()

	encoded := s.Encode()
	if s.PopulatedBins() != populated || s.Collapsed() {
		t.Fatal("Encode mutated the source sketch")
	}

	decoded, err := DecodeSketch(encoded)
	if err != nil {
		t.Fatalf("DecodeSketch: %v", err)
	}
	if got := decoded.PopulatedBins(); got != SketchMaxSerializedBins {
		t.Fatalf("populated bins after encode: got %d, want %d", got, SketchMaxSerializedBins)
	}
	if !decoded.Collapsed() {
		t.Fatal("collapsed flag not set after hitting the serialized bin cap")
	}
	if decoded.Count() != wantCount || decoded.Sum() != wantSum {
		t.Fatalf("totals lost: count %d/%d sum %g/%g", decoded.Count(), wantCount, decoded.Sum(), wantSum)
	}
	if decoded.binTotal != s.binTotal {
		t.Fatalf("retained observations: got %d, want %d", decoded.binTotal, s.binTotal)
	}
	// The upper quantiles survive: collapse only merges the lowest buckets.
	if a, b := s.Quantile(0.99), decoded.Quantile(0.99); a != b {
		t.Fatalf("q0.99 changed across the cap: %g vs %g", a, b)
	}
	if !bytes.Equal(encoded, decoded.Encode()) {
		t.Fatal("capped sketch does not re-encode identically")
	}

	// Worst-case serialized size stays inside the ~1.5 KiB budget.
	if len(encoded) > 1600 {
		t.Fatalf("encoded size %d exceeds the serialized budget", len(encoded))
	}
	t.Logf("%d populated bins collapsed to %d, %d encoded bytes",
		populated, SketchMaxSerializedBins, len(encoded))
}

func TestSketchCodecExactlyAtCap(t *testing.T) {
	s := sketchWithBins(t, SketchMaxSerializedBins)
	decoded, err := DecodeSketch(s.Encode())
	if err != nil {
		t.Fatalf("DecodeSketch: %v", err)
	}
	if decoded.Collapsed() {
		t.Fatal("a sketch exactly at the cap must not be collapsed")
	}
	sketchAssertSameContent(t, s, decoded)
}

func TestSketchCodecRejectsBadInput(t *testing.T) {
	valid := sketchWithBins(t, 8).Encode()

	cases := []struct {
		name  string
		input []byte
		want  error
	}{
		{"empty", nil, ErrSketchTruncated},
		{"header only partial", valid[:2], ErrSketchTruncated},
		{"unknown version", append([]byte{0x02}, valid[1:]...), ErrSketchVersion},
		{"zero version", append([]byte{0x00}, valid[1:]...), ErrSketchVersion},
		{"unknown scale", append([]byte{valid[0], 0xff}, valid[2:]...), ErrSketchScale},
		{"reserved flags", append([]byte{valid[0], valid[1], 0x80}, valid[3:]...), ErrSketchCorrupt},
		{"truncated body", valid[:len(valid)-3], ErrSketchTruncated},
		{"trailing bytes", append(append([]byte(nil), valid...), 0x00), ErrSketchCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := DecodeSketch(tc.input)
			if s != nil {
				t.Fatal("expected a nil sketch on error")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}

	// Every proper prefix of a valid encoding must be rejected, never
	// silently accepted as a shorter sketch.
	for i := range len(valid) {
		if _, err := DecodeSketch(valid[:i]); err == nil {
			t.Fatalf("prefix of length %d decoded without error", i)
		}
	}
}

// sketchEncodeManual builds an encoding field by field so tests can plant
// structurally invalid bodies.
func sketchEncodeManual(zeroCount, count uint64, sum float64, first int64, bins [][2]uint64) []byte {
	out := []byte{SketchEncodingVersion, SketchDefaultScale, 0x00}
	out = binary.AppendUvarint(out, zeroCount)
	out = binary.AppendUvarint(out, count)
	out = binary.LittleEndian.AppendUint64(out, math.Float64bits(sum))
	out = binary.AppendUvarint(out, uint64(len(bins)))
	out = binary.AppendVarint(out, first)
	for _, b := range bins {
		out = binary.AppendUvarint(out, b[0])
		out = binary.AppendUvarint(out, b[1])
	}
	return out
}

func TestSketchCodecRejectsCorruptBody(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{
			name:  "first delta not zero",
			input: sketchEncodeManual(0, 2, 1, 10, [][2]uint64{{1, 2}}),
		},
		{
			name:  "repeated bucket",
			input: sketchEncodeManual(0, 3, 1, 10, [][2]uint64{{0, 1}, {1, 1}, {0, 1}}),
		},
		{
			name:  "empty bin",
			input: sketchEncodeManual(0, 1, 1, 10, [][2]uint64{{0, 0}}),
		},
		{
			name:  "bin count above uint32",
			input: sketchEncodeManual(0, math.MaxUint32+2, 1, 10, [][2]uint64{{0, math.MaxUint32 + 1}}),
		},
		{
			name:  "index delta overflowing int64",
			input: sketchEncodeManual(0, 2, 1, 0, [][2]uint64{{0, 1}, {math.MaxUint64, 1}}),
		},
		{
			name:  "span beyond bin budget",
			input: sketchEncodeManual(0, 2, 1, 0, [][2]uint64{{0, 1}, {uint64(SketchMaxBins), 1}}),
		},
		{
			name:  "count below stored observations",
			input: sketchEncodeManual(0, 1, 1, 10, [][2]uint64{{0, 5}}),
		},
		{
			name:  "count below zero count",
			input: sketchEncodeManual(9, 1, 1, 0, nil),
		},
		{
			name:  "first index without bins",
			input: sketchEncodeManual(1, 1, 1, 7, nil),
		},
		{
			name:  "non-finite sum",
			input: sketchEncodeManual(0, 1, math.NaN(), 10, [][2]uint64{{0, 1}}),
		},
		{
			name:  "more bins than the serialized cap",
			input: sketchWithBins(t, 400).sketchEncodeUncapped(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeSketch(tc.input); !errors.Is(err, ErrSketchCorrupt) {
				t.Fatalf("got %v, want ErrSketchCorrupt", err)
			}
		})
	}
}

// sketchEncodeUncapped serializes every populated bin, bypassing the encoder's
// collapse-to-cap step, so decoder enforcement of the cap can be tested.
func (s *Sketch) sketchEncodeUncapped() []byte {
	out := []byte{SketchEncodingVersion, s.scale, 0x00}
	out = binary.AppendUvarint(out, s.zeroCount)
	out = binary.AppendUvarint(out, s.count)
	out = binary.LittleEndian.AppendUint64(out, math.Float64bits(s.sum))
	out = binary.AppendUvarint(out, uint64(s.PopulatedBins()))
	first, _ := s.firstPopulated()
	out = binary.AppendVarint(out, int64(first))
	prev := first
	for i := first; i <= s.maxIdx; i++ {
		c := s.bins[i-s.minIdx]
		if c == 0 {
			continue
		}
		out = binary.AppendUvarint(out, uint64(i-prev))
		out = binary.AppendUvarint(out, uint64(c))
		prev = i
	}
	return out
}

func TestSketchCodecNegativeIndexes(t *testing.T) {
	// Sub-millisecond latencies produce negative bucket indexes, which the
	// zigzag-encoded first_index has to survive.
	s := NewSketch()
	for _, v := range []float64{1e-6, 5e-6, 2.5e-5, 1e-4} {
		s.Observe(v)
	}
	first, ok := s.firstPopulated()
	if !ok || first >= 0 {
		t.Fatalf("expected a negative first bucket index, got %d (ok=%v)", first, ok)
	}
	decoded, err := DecodeSketch(s.Encode())
	if err != nil {
		t.Fatalf("DecodeSketch: %v", err)
	}
	sketchAssertSameContent(t, s, decoded)
}

func TestSketchCodecCollapsedFlagRoundTrips(t *testing.T) {
	s := NewSketch()
	s.Observe(1e-9)
	s.Observe(1e-9 * math.Exp2(40))
	if !s.Collapsed() {
		t.Fatal("setup: expected a collapsed sketch")
	}
	encoded := s.Encode()
	if encoded[2]&sketchFlagCollapsed == 0 {
		t.Fatal("collapsed flag not written")
	}
	decoded, err := DecodeSketch(encoded)
	if err != nil {
		t.Fatalf("DecodeSketch: %v", err)
	}
	if !decoded.Collapsed() {
		t.Fatal("collapsed flag not restored")
	}
	if !bytes.Equal(encoded, decoded.Encode()) {
		t.Fatal("collapsed sketch does not re-encode identically")
	}
}
