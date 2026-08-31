package latency

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProvenanceJSONContract(t *testing.T) {
	value := Provenance{P99: &Percentile{
		Status:             StatusApproximate,
		Method:             MethodDDSketch,
		SampleCount:        1000,
		RelativeErrorBound: 0.0217,
	}}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, field := range []string{`"p99"`, `"status":"approximate"`, `"method":"ddsketch"`, `"sample_count":1000`, `"relative_error_bound":0.0217`} {
		if !strings.Contains(got, field) {
			t.Fatalf("%s missing %s", got, field)
		}
	}
	for _, omitted := range []string{`"p50"`, `"population_count"`, `"degraded"`, `"reason"`} {
		if strings.Contains(got, omitted) {
			t.Fatalf("%s unexpectedly contains %s", got, omitted)
		}
	}
}
