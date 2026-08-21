package aggregate

import (
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

func TestDimsConfig_Get(t *testing.T) {
	cfg := DimsConfig{
		"http.requests":   {"method", "status_code"},
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

func TestDimsID(t *testing.T) {
	t.Run("empty values", func(t *testing.T) {
		id := DimsID([]string{})
		if id != 0 {
			t.Errorf("DimsID([]) = %d, want 0", id)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		values := []string{"method", "POST", "status_code", "200"}
		id1 := DimsID(values)
		id2 := DimsID(values)
		if id1 != id2 {
			t.Errorf("DimsID not deterministic: %d vs %d", id1, id2)
		}
	})

	t.Run("order-independent", func(t *testing.T) {
		// Different input order should produce same ID (values are re-sorted).
		values1 := []string{"a", "b", "c"}
		values2 := []string{"c", "a", "b"}
		id1 := DimsID(values1)
		id2 := DimsID(values2)
		if id1 != id2 {
			t.Errorf("DimsID not order-independent: %d vs %d", id1, id2)
		}
	})

	t.Run("non-zero result", func(t *testing.T) {
		// The hash must never collide with 0 (reserved).
		values := []string{"test_value"}
		id := DimsID(values)
		if id == 0 {
			t.Errorf("DimsID returned 0, want non-zero for non-empty values")
		}
	})
}
