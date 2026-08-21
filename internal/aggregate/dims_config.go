package aggregate

import (
	"crypto/md5"
	"encoding/binary"
	"sort"

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

// DimsID computes a stable hash ID for a set of dimension values.
// The ID is deterministic and consistent across restarts.
func DimsID(values []string) uint32 {
	if len(values) == 0 {
		return 0
	}
	// Sort values for canonical hashing.
	sorted := make([]string, len(values))
	copy(sorted, values)
	sort.Strings(sorted)

	// Hash the concatenated sorted values using MD5 and take the low 32 bits.
	h := md5.New()
	for _, v := range sorted {
		h.Write([]byte(v))
		h.Write([]byte{0}) // null separator
	}
	digest := h.Sum(nil)
	// Use little-endian to match SeriesKey encoding.
	id := binary.LittleEndian.Uint32(digest[:4])
	// Ensure non-zero (0 is reserved for "no dimensions").
	if id == 0 {
		id = 1
	}
	return id
}
