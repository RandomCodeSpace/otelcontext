package aggregate

import (
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

func TestDimsConfig_Get(t *testing.T) {
	cfg := DimsConfig{
		"http.requests":    {"method", "status_code"},
		"db.query.latency": {"database", "operation"},
	}

	cases := []struct {
		name   string
		metric string
		want   []string
	}{
		{"http.requests", "http.requests", []string{"method", "status_code"}},
		{"db.query.latency", "db.query.latency", []string{"database", "operation"}},
		{"unmapped metric", "unknown.metric", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cfg.Get(c.metric)
			if len(got) != len(c.want) {
				t.Errorf("Get(%q) returned %v, want %v", c.metric, got, c.want)
				return
			}
			for i, v := range c.want {
				if got[i] != v {
					t.Errorf("Get(%q)[%d] = %q, want %q", c.metric, i, got[i], v)
				}
			}
		})
	}
}

func TestDimsConfig_ExtractDimensionValues(t *testing.T) {
	cfg := DimsConfig{
		"http.requests": {"method", "status_code"},
	}

	t.Run("missing metric", func(t *testing.T) {
		attrs := []*commonpb.KeyValue{
			{Key: "method", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}}},
		}
		got := cfg.ExtractDimensionValues("unknown.metric", attrs)
		if got != nil {
			t.Errorf("unmapped metric returned %v, want nil", got)
		}
	})

	t.Run("complete dimensions", func(t *testing.T) {
		attrs := []*commonpb.KeyValue{
			{Key: "method", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "POST"}}},
			{Key: "status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "200"}}},
			{Key: "ignored_attr", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "value"}}},
		}
		got := cfg.ExtractDimensionValues("http.requests", attrs)
		want := []string{"POST", "200"}
		if len(got) != len(want) {
			t.Errorf("Extract returned %v, want %v", got, want)
			return
		}
		for i, v := range want {
			if got[i] != v {
				t.Errorf("[%d] = %q, want %q", i, got[i], v)
			}
		}
	})

	t.Run("missing dimension key", func(t *testing.T) {
		attrs := []*commonpb.KeyValue{
			{Key: "method", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}}},
			// status_code is missing
		}
		got := cfg.ExtractDimensionValues("http.requests", attrs)
		if got != nil {
			t.Errorf("missing key returned %v, want nil", got)
		}
	})

	t.Run("non-string attribute ignored", func(t *testing.T) {
		attrs := []*commonpb.KeyValue{
			{Key: "method", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}}},
			{Key: "status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 200}}}, // int, not string
		}
		got := cfg.ExtractDimensionValues("http.requests", attrs)
		if got != nil {
			t.Errorf("non-string value returned %v, want nil", got)
		}
	})
}

func TestInternDimValues(t *testing.T) {
	newCache := func() *Cache {
		return NewCache(NewMemRegistrar(nil))
	}

	t.Run("empty keys", func(t *testing.T) {
		c := newCache()
		if id := InternDimValues(c, 1, nil, nil); id != 0 {
			t.Errorf("InternDimValues(nil) = %d, want 0", id)
		}
	})

	t.Run("mismatched lengths", func(t *testing.T) {
		c := newCache()
		if id := InternDimValues(c, 1, []string{"method"}, nil); id != 0 {
			t.Errorf("mismatched lengths = %d, want 0", id)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		c := newCache()
		keys := []string{"method", "status_code"}
		values := []string{"POST", "200"}
		id1 := InternDimValues(c, 1, keys, values)
		id2 := InternDimValues(c, 1, keys, values)
		if id1 == 0 || id1 != id2 {
			t.Errorf("not deterministic: %d vs %d", id1, id2)
		}
	})

	t.Run("pair-order independent", func(t *testing.T) {
		// The same key=value pairs in a different order canonicalize to the
		// same tuple and therefore the same ID.
		c := newCache()
		id1 := InternDimValues(c, 1, []string{"a", "b"}, []string{"x", "y"})
		id2 := InternDimValues(c, 1, []string{"b", "a"}, []string{"y", "x"})
		if id1 != id2 {
			t.Errorf("pair order changed the ID: %d vs %d", id1, id2)
		}
	})

	t.Run("distinct pairings get distinct IDs", func(t *testing.T) {
		// Same value multiset attached to different keys must NOT collide —
		// this is exactly what a value-only hash got wrong.
		c := newCache()
		id1 := InternDimValues(c, 1, []string{"a", "b"}, []string{"x", "y"})
		id2 := InternDimValues(c, 1, []string{"a", "b"}, []string{"y", "x"})
		if id1 == id2 {
			t.Errorf("distinct key/value pairings collided: %d", id1)
		}
	})

	t.Run("tenant scoped", func(t *testing.T) {
		c := newCache()
		id1 := InternDimValues(c, 1, []string{"method"}, []string{"GET"})
		id2 := InternDimValues(c, 2, []string{"method"}, []string{"GET"})
		if id1 == id2 {
			t.Errorf("tenants shared a dim tuple ID: %d", id1)
		}
	})
}
