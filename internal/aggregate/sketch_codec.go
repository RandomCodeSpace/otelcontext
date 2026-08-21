package aggregate

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Serialized sketch format (little-endian, deterministic):
//
//	offset  field
//	0       version      u8       = 0x01
//	1       scale        u8       (the OTel base-2 scale itself)
//	2       flags        u8       (bit0 = collapsed, remaining bits reserved)
//	3       zero_count   uvarint
//	.       count        uvarint  (total, including zero_count)
//	.       sum          f64      (8 bytes, IEEE 754)
//	.       num_bins     uvarint  (populated bins only)
//	.       first_index  svarint  (zigzag; bucket indexes are signed)
//	.       repeated num_bins times:
//	          index_delta uvarint (0 for the first bin, >= 1 afterwards)
//	          bin_count   uvarint (>= 1, <= MaxUint32)
//
// Only populated bins are written, so a typical latency sketch costs ~16 bytes
// of header plus 2-4 bytes per populated bin instead of the 2 KiB of the dense
// in-memory array. The version and scale bytes are the entire versioning story:
// a reader refuses anything it does not recognise.
//
// Encoding is a pure function of sketch state, so identical state always
// produces identical bytes and Encode(Decode(Encode(x))) == Encode(x).

// SketchEncodingVersion is the only encoding version this package writes or
// accepts.
const SketchEncodingVersion uint8 = 0x01

// SketchMaxSerializedBins caps the populated bins written by the encoder. It
// bounds a serialized sketch at roughly 1.5 KiB, which is what the 7-day
// worst-case disk arithmetic in issue #162 is budgeted against. A sketch with
// more populated bins is collapsed-lowest at encode time.
const SketchMaxSerializedBins = 256

// sketchFlagCollapsed marks a sketch whose lowest bins were merged.
const sketchFlagCollapsed uint8 = 0x01

// sketchHeaderLen is the size of the fixed version/scale/flags prefix.
const sketchHeaderLen = 3

// Codec errors. All decode failures wrap one of these.
var (
	// ErrSketchVersion reports an encoding version the decoder does not know.
	ErrSketchVersion = errors.New("aggregate: unsupported sketch encoding version")
	// ErrSketchTruncated reports input that ends inside a field.
	ErrSketchTruncated = errors.New("aggregate: truncated sketch encoding")
	// ErrSketchCorrupt reports input that is structurally invalid: bad varints,
	// non-canonical bins, impossible totals, or trailing bytes.
	ErrSketchCorrupt = errors.New("aggregate: corrupt sketch encoding")
)

// Encode returns the serialized form of the sketch.
func (s *Sketch) Encode() []byte {
	return s.AppendTo(nil)
}

// AppendTo appends the serialized form of the sketch to dst and returns the
// extended buffer, allowing callers to reuse a scratch buffer.
//
// The sketch is never mutated: if it holds more than SketchMaxSerializedBins
// populated bins, a copy is collapsed-lowest to the cap and that copy is
// written. Totals survive the collapse; only resolution below the new floor is
// lost, and the collapsed flag records it.
func (s *Sketch) AppendTo(dst []byte) []byte {
	src := s
	if s.PopulatedBins() > SketchMaxSerializedBins {
		capped := *s
		capped.collapseToBinCap(SketchMaxSerializedBins)
		src = &capped
	}

	flags := uint8(0)
	if src.collapsed {
		flags |= sketchFlagCollapsed
	}

	dst = append(dst, SketchEncodingVersion, src.scale, flags)
	dst = binary.AppendUvarint(dst, src.zeroCount)
	dst = binary.AppendUvarint(dst, src.count)
	dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(src.sum))
	dst = binary.AppendUvarint(dst, uint64(src.PopulatedBins())) // #nosec G115 -- bounded by SketchMaxBins

	first, ok := src.firstPopulated()
	if !ok {
		return binary.AppendVarint(dst, 0)
	}
	dst = binary.AppendVarint(dst, int64(first))

	prev := first
	for i := first; i <= src.maxIdx; i++ {
		c := src.bins[i-src.minIdx]
		if c == 0 {
			continue
		}
		dst = binary.AppendUvarint(dst, uint64(i-prev)) // #nosec G115 -- indexes ascend, delta is in [0, SketchMaxBins)
		dst = binary.AppendUvarint(dst, uint64(c))
		prev = i
	}
	return dst
}

// firstPopulated returns the lowest populated bucket index.
func (s *Sketch) firstPopulated() (int32, bool) {
	if !s.hasBins {
		return 0, false
	}
	for i := s.minIdx; i <= s.maxIdx; i++ {
		if s.bins[i-s.minIdx] != 0 {
			return i, true
		}
	}
	return 0, false
}

// DecodeSketch parses a serialized sketch. Unknown versions and scales are
// rejected with ErrSketchVersion and ErrSketchScale; anything structurally
// invalid is rejected with ErrSketchTruncated or ErrSketchCorrupt. A decoded
// sketch re-encodes to exactly the input bytes.
//
// The saturation counter is not part of the format and decodes to zero: it is
// an operational signal about a live sketch, not part of its value.
func DecodeSketch(data []byte) (*Sketch, error) {
	if len(data) < sketchHeaderLen {
		return nil, fmt.Errorf("%w: %d header bytes", ErrSketchTruncated, len(data))
	}
	if data[0] != SketchEncodingVersion {
		return nil, fmt.Errorf("%w: 0x%02x", ErrSketchVersion, data[0])
	}
	scale := data[1]
	if scale > SketchMaxScale {
		return nil, fmt.Errorf("%w: %d", ErrSketchScale, scale)
	}
	flags := data[2]
	if flags&^sketchFlagCollapsed != 0 {
		return nil, fmt.Errorf("%w: reserved flag bits 0x%02x", ErrSketchCorrupt, flags)
	}

	r := sketchReader{buf: data, pos: sketchHeaderLen}
	zeroCount, err := r.uvarint("zero_count")
	if err != nil {
		return nil, err
	}
	count, err := r.uvarint("count")
	if err != nil {
		return nil, err
	}
	bits, err := r.uint64("sum")
	if err != nil {
		return nil, err
	}
	sum := math.Float64frombits(bits)
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return nil, fmt.Errorf("%w: non-finite sum", ErrSketchCorrupt)
	}
	numBins, err := r.uvarint("num_bins")
	if err != nil {
		return nil, err
	}
	if numBins > SketchMaxSerializedBins {
		return nil, fmt.Errorf("%w: %d bins exceeds the %d cap", ErrSketchCorrupt, numBins, SketchMaxSerializedBins)
	}
	firstIndex, err := r.varint("first_index")
	if err != nil {
		return nil, err
	}

	s := &Sketch{
		scale:     scale,
		sum:       sum,
		count:     count,
		zeroCount: zeroCount,
		collapsed: flags&sketchFlagCollapsed != 0,
	}
	if err := decodeBins(&r, s, int(numBins), firstIndex); err != nil {
		return nil, err
	}
	if r.pos != len(data) {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrSketchCorrupt, len(data)-r.pos)
	}
	if count < zeroCount || count-zeroCount < s.binTotal {
		return nil, fmt.Errorf("%w: count %d below zero_count %d plus bin total %d", ErrSketchCorrupt, count, zeroCount, s.binTotal)
	}
	return s, nil
}

// decodeBins reads the bin block into s and sets the window bounds.
func decodeBins(r *sketchReader, s *Sketch, numBins int, firstIndex int64) error {
	if numBins == 0 {
		if firstIndex != 0 {
			return fmt.Errorf("%w: first_index %d with no bins", ErrSketchCorrupt, firstIndex)
		}
		return nil
	}
	if firstIndex < math.MinInt32 || firstIndex > math.MaxInt32 {
		return fmt.Errorf("%w: first_index %d out of range", ErrSketchCorrupt, firstIndex)
	}

	index := firstIndex
	for i := range numBins {
		delta, err := r.uvarint("index_delta")
		if err != nil {
			return err
		}
		switch {
		case i == 0 && delta != 0:
			return fmt.Errorf("%w: first index_delta %d must be 0", ErrSketchCorrupt, delta)
		case i > 0 && delta == 0:
			return fmt.Errorf("%w: index_delta 0 repeats bucket %d", ErrSketchCorrupt, index)
		}
		if delta >= uint64(SketchMaxBins) {
			return fmt.Errorf("%w: index_delta %d exceeds %d bins", ErrSketchCorrupt, delta, SketchMaxBins)
		}
		index += int64(delta) // #nosec G115 -- delta is bounded above
		if index-firstIndex >= int64(SketchMaxBins) {
			return fmt.Errorf("%w: bucket span %d exceeds %d bins", ErrSketchCorrupt, index-firstIndex+1, SketchMaxBins)
		}

		binCount, err := r.uvarint("bin_count")
		if err != nil {
			return err
		}
		if binCount == 0 {
			return fmt.Errorf("%w: empty bin at bucket %d", ErrSketchCorrupt, index)
		}
		if binCount > math.MaxUint32 {
			return fmt.Errorf("%w: bin count %d exceeds uint32", ErrSketchCorrupt, binCount)
		}

		s.bins[index-firstIndex] = uint32(binCount)
		s.binTotal += binCount
	}

	s.hasBins = true
	s.minIdx = int32(firstIndex)
	s.maxIdx = int32(index)
	return nil
}

// sketchReader is a cursor over an encoded sketch that turns stdlib varint
// failures into the package's typed errors.
type sketchReader struct {
	buf []byte
	pos int
}

func (r *sketchReader) uvarint(field string) (uint64, error) {
	v, n := binary.Uvarint(r.buf[r.pos:])
	switch {
	case n == 0:
		return 0, fmt.Errorf("%w: reading %s", ErrSketchTruncated, field)
	case n < 0:
		return 0, fmt.Errorf("%w: overlong varint for %s", ErrSketchCorrupt, field)
	}
	r.pos += n
	return v, nil
}

func (r *sketchReader) varint(field string) (int64, error) {
	v, n := binary.Varint(r.buf[r.pos:])
	switch {
	case n == 0:
		return 0, fmt.Errorf("%w: reading %s", ErrSketchTruncated, field)
	case n < 0:
		return 0, fmt.Errorf("%w: overlong varint for %s", ErrSketchCorrupt, field)
	}
	r.pos += n
	return v, nil
}

func (r *sketchReader) uint64(field string) (uint64, error) {
	if len(r.buf)-r.pos < 8 {
		return 0, fmt.Errorf("%w: reading %s", ErrSketchTruncated, field)
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v, nil
}
