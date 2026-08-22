package aggregate

import (
	"strconv"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// DimsConfig maps metric names to their aggregation dimension keys.
// Keys are sorted canonically per ParseAggregateMetricDims; the lookup
// is exact on metric name and returns the dimension key list or nil
// if the metric is not configured for aggregation.
type DimsConfig map[string][]string

// Get returns the sorted dimension keys for a metric, or nil if the
// metric is not in the config.
func (d DimsConfig) Get(metricName string) []string {
	return d[metricName]
}

// ExtractDimensionValues extracts the values of configured dimension keys
// from a slice of OTLP attributes. Returns nil if the metric is not
// configured or if any configured key is missing from the attributes.
// Returned values are in the order of the configured keys.
func (d DimsConfig) ExtractDimensionValues(metricName string, attrs []*commonpb.KeyValue) []string {
	keys := d.Get(metricName)
	if keys == nil {
		return nil
	}

	// Build a map of attribute key to value for fast lookup.
	attrMap := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		if kv == nil || kv.Value == nil {
			continue
		}
		// Only extract string values for dimension aggregation.
		if sv := kv.Value.GetStringValue(); sv != "" {
			attrMap[kv.Key] = sv
		}
	}

	// Extract dimension values in the order of configured keys.
	// Return nil if any key is missing.
	values := make([]string, len(keys))
	for i, key := range keys {
		val, ok := attrMap[key]
		if !ok {
			return nil
		}
		values[i] = val
	}
	return values
}

// InternDimValues resolves configured dimension keys and their extracted
// values to dictionary IDs and returns the DimsID for a SeriesKey via
// Cache.InternDims. IDs come from the tenant-scoped dictionary, never from
// hashing: a hash collision would silently merge unrelated series.
// keys and values must be parallel slices (as returned by
// ExtractDimensionValues for the configured keys); an empty or mismatched
// input yields 0, the "no configured dims" sentinel.
func InternDimValues(c *Cache, tenantID uint32, keys, values []string) uint32 {
	if len(keys) == 0 || len(keys) != len(values) {
		return 0
	}
	pairs := make([]DimPair, len(keys))
	for i := range keys {
		pairs[i] = DimPair{
			KeyID:   c.Intern(tenantID, KindDimKey, keys[i]),
			ValueID: c.Intern(tenantID, KindDimValue, values[i]),
		}
	}
	return c.InternDims(tenantID, pairs)
}

// --- hot-path dimension extraction (#199 Q4) ---------------------------------
//
// ExtractDimensionValues above allocates a map[string]string per call. That is
// fine for the config-time helper it was written as and unacceptable per metric
// data point: configured dimensions are BOUNDED (an operator lists a handful of
// keys per metric), so a point's tuple resolves against a request-local scratch
// with no map, no per-point slice and one string allocation only for a non-string
// attribute value that has to be rendered.

// MaxDimensionKeys bounds one metric's configured dimension tuple. A tuple
// longer than this is refused from identity rather than silently truncated.
const MaxDimensionKeys = 16

// DimsRejectUnsupportedValue is the metric label for an attribute value that
// has no canonical scalar rendering (array, kvlist, bytes). The point is still
// aggregated -- under DimsID 0, the "no configured dims" sentinel.
const DimsRejectUnsupportedValue = "unsupported_value_type"

// dimScratch is one Export request's reusable dimension-extraction workspace.
// It is owned by a Reducer, which is request-local and single-goroutine, so no
// synchronization is needed and no allocation survives the request.
type dimScratch struct {
	vals  [MaxDimensionKeys]string
	found [MaxDimensionKeys]bool
	buf   []byte
}

// resolve extracts the configured tuple for keys out of attrs.
//
// It returns ok=false when any configured key is absent or carries an empty
// value -- the all-or-nothing contract, which exists because a partial tuple is
// a different series wearing the same name. rejected=true additionally reports
// that a key WAS present but its value had no scalar rendering, which is worth
// a counter: the operator configured a dimension that can never bind.
func (s *dimScratch) resolve(keys []string, attrs []*commonpb.KeyValue) (vals []string, rejected, ok bool) {
	n := len(keys)
	if n == 0 || n > MaxDimensionKeys {
		return nil, false, false
	}
	for i := 0; i < n; i++ {
		s.vals[i] = ""
		s.found[i] = false
	}
	remaining := n
	for _, kv := range attrs {
		if kv == nil || kv.Value == nil || remaining == 0 {
			continue
		}
		idx := -1
		for i := 0; i < n; i++ {
			if !s.found[i] && keys[i] == kv.Key {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		v, scalar := s.scalarValue(kv.Value)
		if !scalar {
			return nil, true, false
		}
		if v == "" {
			// An empty value cannot be interned: the dictionary refuses a
			// zero-length identity, and every empty value in a namespace would
			// collide. Treat it as absent.
			continue
		}
		s.vals[idx] = v
		s.found[idx] = true
		remaining--
	}
	if remaining != 0 {
		return nil, false, false
	}
	return s.vals[:n], false, true
}

// scalarValue renders an OTLP attribute value as its canonical dimension
// string. String, int, bool and double values are supported; array, kvlist and
// bytes values are not -- they have no stable canonical rendering, and hashing
// one into identity would make the series depend on element order.
func (s *dimScratch) scalarValue(v *commonpb.AnyValue) (string, bool) {
	switch tv := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return tv.StringValue, true
	case *commonpb.AnyValue_IntValue:
		s.buf = strconv.AppendInt(s.buf[:0], tv.IntValue, 10)
		return string(s.buf), true
	case *commonpb.AnyValue_BoolValue:
		if tv.BoolValue {
			return "true", true
		}
		return "false", true
	case *commonpb.AnyValue_DoubleValue:
		s.buf = strconv.AppendFloat(s.buf[:0], tv.DoubleValue, 'g', -1, 64)
		return string(s.buf), true
	case nil:
		// An AnyValue with no value set is an absent attribute, not a
		// rejection.
		return "", true
	default:
		return "", false
	}
}
