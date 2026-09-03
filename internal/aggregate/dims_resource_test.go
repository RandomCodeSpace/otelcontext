package aggregate

import (
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// #279: a configured dimension key the point lacks falls back to the same key
// on the resource attributes. Only a key missing from BOTH yields DimsID 0.

// resourceDimsIDs reduces one point of each shape carrying attrs and
// resource, and returns the DimsID each landed on.
func resourceDimsIDs(t *testing.T, r *Reducer, now time.Time, attrs, resource []*commonpb.KeyValue) []uint32 {
	t.Helper()

	r.ReduceMetricPoint(MetricInput{Tenant: "acme", Service: "checkout", Name: histTestMetric,
		Value: 1, Timestamp: now, Temporality: TemporalityDelta, Attributes: attrs, ResourceAttributes: resource})

	hist := histPoint(histTestBounds, []uint64{0, 5, 7, 0})
	hist.Timestamp, hist.Attributes, hist.ResourceAttributes = now, attrs, resource
	if res := r.ReduceHistogramPoint(hist); res.Rejected() {
		t.Fatalf("histogram rejected: %v", res.Err)
	}

	exp := expPointFromValues(SketchDefaultScale, histTestValues)
	exp.Timestamp, exp.Attributes, exp.ResourceAttributes = now, attrs, resource
	if res := r.ReduceExponentialHistogramPoint(exp); res.Rejected() {
		t.Fatalf("exponential rejected: %v", res.Err)
	}

	ids := make([]uint32, 0, len(r.Deltas()))
	for swk := range r.Deltas() {
		ids = append(ids, swk.Key.DimsID)
	}
	return ids
}

func TestDimsFallBackToResourceAttributes(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := dimsEngine(t, now)

	fromPoint := e.NewReducer(now)
	pointIDs := resourceDimsIDs(t, fromPoint, now,
		[]*commonpb.KeyValue{attr("http.method", "GET"), intAttr("http.status", 200)}, nil)

	fromResource := e.NewReducer(now)
	resourceIDs := resourceDimsIDs(t, fromResource, now,
		[]*commonpb.KeyValue{attr("http.method", "GET")},
		[]*commonpb.KeyValue{attr("host.name", "node-a"), intAttr("http.status", 200)})

	if len(pointIDs) != 1 || len(resourceIDs) != 1 {
		t.Fatalf("series = %v / %v, want one each", pointIDs, resourceIDs)
	}
	if resourceIDs[0] == 0 {
		t.Fatal("resource-sourced dimension resolved to DimsID 0")
	}
	if resourceIDs[0] != pointIDs[0] {
		t.Fatalf("resource-sourced tuple interned to %d, point-sourced to %d; the same tuple must be the same series",
			resourceIDs[0], pointIDs[0])
	}
	if got := fromResource.Stats().DimsRejected; got != 0 {
		t.Errorf("dims rejected = %d, want 0", got)
	}
}

func TestDimsPointAttributeOutranksResource(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := dimsEngine(t, now)

	ok200 := resourceDimsIDs(t, e.NewReducer(now), now,
		[]*commonpb.KeyValue{attr("http.method", "GET"), intAttr("http.status", 200)}, nil)
	both := resourceDimsIDs(t, e.NewReducer(now), now,
		[]*commonpb.KeyValue{attr("http.method", "GET"), intAttr("http.status", 200)},
		[]*commonpb.KeyValue{intAttr("http.status", 500)})
	if len(both) != 1 || both[0] != ok200[0] {
		t.Fatalf("point+resource tuple = %v, want the point-only identity %v", both, ok200)
	}
}

func TestDimsMissingFromPointAndResourceIsZero(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	r := dimsEngine(t, now).NewReducer(now)

	ids := resourceDimsIDs(t, r, now,
		[]*commonpb.KeyValue{attr("http.method", "GET")},
		[]*commonpb.KeyValue{attr("host.name", "node-a")})
	if len(ids) != 1 || ids[0] != 0 {
		t.Fatalf("tuple missing from both produced %v, want a single DimsID 0", ids)
	}
	if got := r.Stats().DimsRejected; got != 0 {
		t.Errorf("dims rejected = %d, want 0 (absent is not rejected)", got)
	}
}

func TestDimsRejectsNonScalarResourceValue(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	r := dimsEngine(t, now).NewReducer(now)

	arrayAttr := &commonpb.KeyValue{Key: "http.status", Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{}}}}
	ids := resourceDimsIDs(t, r, now,
		[]*commonpb.KeyValue{attr("http.method", "GET")},
		[]*commonpb.KeyValue{arrayAttr})
	if len(ids) != 1 || ids[0] != 0 {
		t.Fatalf("array resource dimension produced %v, want a single DimsID 0", ids)
	}
	if got := r.Stats().DimsRejected; got != 3 {
		t.Errorf("dims rejected = %d, want 3 (one per point shape)", got)
	}
}

// TestDimsResourceFallbackDoesNotAllocatePerPoint keeps the #199 Q4 hot-path
// requirement with the resource scan in place.
func TestDimsResourceFallbackDoesNotAllocatePerPoint(t *testing.T) {
	keys := []string{"host.name", "http.method"}
	attrs := []*commonpb.KeyValue{attr("http.method", "GET"), attr("noise", "x")}
	resource := []*commonpb.KeyValue{attr("service.name", "checkout"), attr("host.name", "node-a")}
	var s dimScratch
	s.resolveWith(keys, attrs, resource)
	allocs := testing.AllocsPerRun(100, func() {
		if _, _, ok := s.resolveWith(keys, attrs, resource); !ok {
			t.Fatal("resolveWith failed")
		}
	})
	if allocs != 0 {
		t.Errorf("resolveWith allocated %v times per point, want 0", allocs)
	}
}
