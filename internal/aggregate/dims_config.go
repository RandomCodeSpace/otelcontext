package aggregate

import (
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
