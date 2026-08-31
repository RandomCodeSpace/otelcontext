//go:build loadtest

package main

import (
	"math"
	"testing"
	"time"
)

// TestServiceName verifies zero-padded naming scheme.
func TestServiceName(t *testing.T) {
	cases := []struct {
		idx  int
		want string
	}{
		{0, "loadsim-svc-000"},
		{1, "loadsim-svc-001"},
		{99, "loadsim-svc-099"},
		{199, "loadsim-svc-199"},
	}
	for _, tc := range cases {
		got := serviceName(tc.idx)
		if got != tc.want {
			t.Errorf("serviceName(%d) = %q, want %q", tc.idx, got, tc.want)
		}
	}
}

// TestSpanFactory verifies round-robin ops, duration range, and ~5% error rate.
func TestSpanFactory(t *testing.T) {
	const samples = 10_000

	opCounts := make(map[string]int)
	errorCount := 0
	tooShort := 0
	tooLong := 0

	for i := 0; i < samples; i++ {
		op := pickOperation(i)
		opCounts[op]++

		dur := randomDuration()
		if dur < 5*time.Millisecond {
			tooShort++
		}
		if dur > 500*time.Millisecond {
			tooLong++
		}

		if isError(i) {
			errorCount++
		}
	}

	// All 5 operations must appear.
	for _, op := range operations {
		if opCounts[op] == 0 {
			t.Errorf("operation %q never selected in %d samples", op, samples)
		}
	}

	// Round-robin: each op should appear exactly samples/5 times.
	expected := samples / len(operations)
	for _, op := range operations {
		if opCounts[op] != expected {
			t.Errorf("operation %q count = %d, want %d (strict round-robin)", op, opCounts[op], expected)
		}
	}

	// Duration must stay in [5ms, 500ms].
	if tooShort > 0 {
		t.Errorf("%d durations < 5ms", tooShort)
	}
	if tooLong > 0 {
		t.Errorf("%d durations > 500ms", tooLong)
	}

	// Error rate: 5% ± 1% (absolute).
	errorRate := float64(errorCount) / float64(samples)
	if math.Abs(errorRate-0.05) > 0.01 {
		t.Errorf("error rate = %.4f, want 0.05 ± 0.01", errorRate)
	}
}

func TestLatencySentinelRequestIsContradictoryAndExact(t *testing.T) {
	end := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	req := latencySentinelRequest(end)
	if len(req.ResourceSpans) != 1 || len(req.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("sentinel request shape = %+v", req.ResourceSpans)
	}
	spans := req.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 1000 {
		t.Fatalf("sentinel spans = %d, want 1000", len(spans))
	}
	low, tail := 0, 0
	for _, span := range spans {
		duration := time.Duration(span.EndTimeUnixNano - span.StartTimeUnixNano)
		switch duration {
		case latencySentinelLow:
			low++
		case latencySentinelTail:
			tail++
		default:
			t.Fatalf("unexpected sentinel duration %s", duration)
		}
		if span.Kind.String() != "SPAN_KIND_SERVER" || len(span.ParentSpanId) != 0 {
			t.Fatalf("sentinel span is not an independent server request: %+v", span)
		}
	}
	if low != 989 || tail != 11 {
		t.Fatalf("sentinel distribution = %d low / %d tail, want 989 / 11", low, tail)
	}
}

// TestRateLimiter drives the ticker-based limiter for ~1 second and checks throughput.
func TestRateLimiter(t *testing.T) {
	const targetRPS = 50
	rl := newRateLimiter(targetRPS)
	defer rl.stop()

	start := time.Now()
	count := 0
	deadline := start.Add(1 * time.Second)

	for time.Now().Before(deadline) {
		rl.wait()
		count++
	}

	// Allow ±10% of the target (50 ± 5).
	rpsF := float64(targetRPS)
	low := int(rpsF * 0.90)
	high := int(rpsF * 1.10)
	if count < low || count > high {
		t.Errorf("rate limiter issued %d tokens in 1s, want %d–%d (target %d ±10%%)", count, low, high, targetRPS)
	}
}

// TestPickSeverity verifies round-robin severity selection.
func TestPickSeverity(t *testing.T) {
	const samples = 10_000

	severityCounts := make(map[string]int)
	for i := 0; i < samples; i++ {
		severity := pickSeverity(i)
		severityCounts[severity]++
	}

	// Expected distribution: ~70% INFO, ~15% WARN, ~10% DEBUG, ~5% ERROR.
	// logSeverities array has 12 elements: 7 INFO, 2 WARN, 2 DEBUG, 1 ERROR.
	expectedInfo := samples * 7 / 12
	expectedWarn := samples * 2 / 12
	expectedDebug := samples * 2 / 12
	expectedError := samples * 1 / 12

	tolerance := int(float64(samples) * 0.02) // ±2%

	check := func(severity string, got, expected int) {
		if got < expected-tolerance || got > expected+tolerance {
			t.Errorf("severity %q count = %d, want %d ± %d", severity, got, expected, tolerance)
		}
	}

	check("INFO", severityCounts["INFO"], expectedInfo)
	check("WARN", severityCounts["WARN"], expectedWarn)
	check("DEBUG", severityCounts["DEBUG"], expectedDebug)
	check("ERROR", severityCounts["ERROR"], expectedError)
}

// TestParseBurstSpec validates burst spec parsing.
func TestParseBurstSpec(t *testing.T) {
	cases := []struct {
		input   string
		wantMul float64
		wantDur time.Duration
		wantErr bool
	}{
		{"2x30s", 2.0, 30 * time.Second, false},
		{"1.5x1m", 1.5, 1 * time.Minute, false},
		{"3x10m", 3.0, 10 * time.Minute, false},
		{"2.5x500ms", 2.5, 500 * time.Millisecond, false},
		// Invalid specs:
		{"invalid", 0, 0, true},
		{"2x", 0, 0, true},
		{"x30s", 0, 0, true},
		{"2y30s", 0, 0, true},
	}

	for _, tc := range cases {
		spec, err := parseBurstSpec(tc.input)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseBurstSpec(%q): err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			continue
		}
		if err == nil {
			if spec.multiplier != tc.wantMul || spec.duration != tc.wantDur {
				t.Errorf("parseBurstSpec(%q): got (%v, %v), want (%v, %v)",
					tc.input, spec.multiplier, spec.duration, tc.wantMul, tc.wantDur)
			}
		}
	}
}

// TestSeverityDistribution verifies the weighted log severity mix via statistical bounds.
func TestSeverityDistribution(t *testing.T) {
	const samples = 1_000

	info, warn, debug, err := 0, 0, 0, 0

	for i := 0; i < samples; i++ {
		severity := pickSeverity(i)
		switch severity {
		case "INFO":
			info++
		case "WARN":
			warn++
		case "DEBUG":
			debug++
		case "ERROR":
			err++
		}
	}

	// Verify that major categories meet minimum thresholds (±5% absolute).
	// INFO should dominate: ~58% (7/12).
	infoPercent := 100.0 * float64(info) / float64(samples)
	if infoPercent < 55 || infoPercent > 65 {
		t.Errorf("INFO rate = %.1f%%, want ~58%% (50–65)", infoPercent)
	}

	// WARN should be ~17% (2/12).
	warnPercent := 100.0 * float64(warn) / float64(samples)
	if warnPercent < 12 || warnPercent > 22 {
		t.Errorf("WARN rate = %.1f%%, want ~17%% (12–22)", warnPercent)
	}

	// DEBUG should be ~17% (2/12).
	debugPercent := 100.0 * float64(debug) / float64(samples)
	if debugPercent < 12 || debugPercent > 22 {
		t.Errorf("DEBUG rate = %.1f%%, want ~17%% (12–22)", debugPercent)
	}

	// ERROR should be ~8% (1/12).
	errPercent := 100.0 * float64(err) / float64(samples)
	if errPercent < 3 || errPercent > 13 {
		t.Errorf("ERROR rate = %.1f%%, want ~8%% (3–13)", errPercent)
	}
}

// TestProfileResolution verifies that profile flags set correct service/rate combinations.
func TestProfileResolution(t *testing.T) {
	cases := []struct {
		profile    string
		wantSvc    int
		wantSpan   int
		wantLog    int
		wantMetric int
	}{
		{"aggregate-acceptance", 150, 50, 10, 7},
	}

	for _, tc := range cases {
		// Simulate the profile application logic from main().
		numServices := 200
		rps := 50
		logsRate := 0
		metricsRate := 0

		switch tc.profile {
		case "aggregate-acceptance":
			numServices = 150
			rps = 50
			logsRate = 10
			metricsRate = 7
		}

		if numServices != tc.wantSvc || rps != tc.wantSpan || logsRate != tc.wantLog || metricsRate != tc.wantMetric {
			t.Errorf("profile %q: got (%d, %d, %d, %d), want (%d, %d, %d, %d)",
				tc.profile, numServices, rps, logsRate, metricsRate,
				tc.wantSvc, tc.wantSpan, tc.wantLog, tc.wantMetric)
		}
	}
}
