package gatecore

import (
	"encoding/json"
	"testing"
	"time"
)

func decode(t *testing.T, body string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return v
}

func TestScanTruncated(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		found, isOn bool
	}{
		{"absent", `{"nodes":[{"name":"a"}]}`, false, false},
		{"present false", `{"coverage":{"truncated":false}}`, true, false},
		{"present true", `{"coverage":{"truncated":true}}`, true, true},
		{"nested in an array", `{"traces":[{"exemplar":{"truncated":true}}]}`, true, true},
		{"deeply nested false", `[{"a":{"b":[{"truncated":false}]}}]`, true, false},
		{"mixed, one true", `[{"truncated":false},{"truncated":true}]`, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			found, isOn := ScanTruncated(decode(t, c.body))
			if found != c.found || isOn != c.isOn {
				t.Errorf("ScanTruncated = (%t, %t), want (%t, %t)", found, isOn, c.found, c.isOn)
			}
		})
	}
}

func TestFindStringField(t *testing.T) {
	v := decode(t, `{"a":{"b":{"coverage":"full"}}}`)
	got, ok := FindStringField(v, "coverage")
	if !ok || got != "full" {
		t.Errorf("FindStringField = %q, %t", got, ok)
	}
	if _, ok := FindStringField(v, "nope"); ok {
		t.Error("an absent field must report not-found")
	}
}

func TestTopLevelScalarsDistinguishesZeroFromAbsent(t *testing.T) {
	v := decode(t, `{"total_traces":0,"total_logs":42,"name":"x"}`)
	got := TopLevelScalars(v, []string{"total_traces", "total_logs", "total_errors"})
	if _, ok := got["total_traces"]; !ok {
		t.Error("a reported zero must be present in the scalar map")
	}
	if got["total_logs"] != 42 {
		t.Errorf("total_logs = %v", got["total_logs"])
	}
	if _, ok := got["total_errors"]; ok {
		t.Error("a field the surface never reported must be absent, not zero")
	}
}

func TestParseWindowPointsAndTotals(t *testing.T) {
	body := []byte(`[
      {"timestamp":"2026-08-22T00:00:00Z","count":10,"error_count":1,"requests":10,"spans":40,"span_errors":2},
      {"timestamp":"2026-08-22T00:05:00Z","count":12,"error_count":0,"requests":12,"spans":48,"span_errors":0},
      {"timestamp":"2026-08-22T00:05:30Z","count":1,"error_count":0,"requests":1,"spans":2,"span_errors":0}
    ]`)
	pts, err := ParseWindowPoints(body)
	if err != nil {
		t.Fatalf("ParseWindowPoints: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("points = %d", len(pts))
	}
	totals := WindowTotals(pts, "spans", 300)
	base := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC).Unix()
	if totals[base] != 40 {
		t.Errorf("window 0 spans = %d, want 40", totals[base])
	}
	// Two points inside the same window must sum, not overwrite.
	if totals[base+300] != 50 {
		t.Errorf("window 1 spans = %d, want 50", totals[base+300])
	}
}

func TestParseWindowPointsRejectsNonArray(t *testing.T) {
	if _, err := ParseWindowPoints([]byte(`{"points":[]}`)); err == nil {
		t.Error("an enveloped response must fail rather than silently read as zero windows")
	}
}

func TestWindowCoverage(t *testing.T) {
	base := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	pts := []WindowPoint{
		{Timestamp: base},
		{Timestamp: base.Add(10 * time.Minute)},
		{Timestamp: base.Add(30 * time.Minute)}, // outside the expected set
	}
	expected := []int64{base.Unix(), base.Add(5 * time.Minute).Unix(), base.Add(10 * time.Minute).Unix()}
	returned, missing, extra := WindowCoverage(pts, expected, 300)
	if returned != 2 || missing != 1 || extra != 1 {
		t.Errorf("coverage = (%d returned, %d missing, %d extra), want (2, 1, 1)", returned, missing, extra)
	}
}

func TestLoadsimReportPhaseExtraction(t *testing.T) {
	rep := LoadsimReport{
		FirstErr: "boom",
		Phases: []LoadsimPhase{
			{Phase: "settle", All: LoadsimLatency{Samples: 10, P99Ms: 900}, DurationSec: 120},
			{Phase: "sustained", All: LoadsimLatency{Samples: 500, P50Ms: 20, P99Ms: 120}, DurationSec: 10800,
				PointsSent: 108000000, PointsAcked: 108000000, PointsPerSec: 10000, Exhausted: 0},
			{Phase: "burst", All: LoadsimLatency{}, DurationSec: 60},
		},
	}
	s := rep.PhaseNamed("x.json", "sustained")
	if !s.Present || s.P99Ms != 120 || s.PointsAcked != 108000000 {
		t.Errorf("sustained phase = %+v", s)
	}
	if s.FirstErr != "boom" {
		t.Error("the first error must travel with the phase")
	}
	// A phase with no samples measured nothing and must not read as present.
	if b := rep.PhaseNamed("x.json", "burst"); b.Present {
		t.Error("a phase with zero ACK samples must not be treated as measured")
	}
	if m := rep.PhaseNamed("x.json", "no-such-phase"); m.Present {
		t.Error("a missing phase must not be present")
	}
}
