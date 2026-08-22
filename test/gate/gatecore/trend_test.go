package gatecore

import (
	"math"
	"testing"
)

func TestFitLineExactSlope(t *testing.T) {
	// y = 1000x + 500, no noise: the fit must recover both coefficients and
	// report a zero residual.
	var pts []Point
	for i := 0; i < 10; i++ {
		x := float64(i)
		pts = append(pts, Point{X: x, Y: 1000*x + 500})
	}
	f, err := FitLine(pts, 6)
	if err != nil {
		t.Fatalf("FitLine: %v", err)
	}
	if math.Abs(f.Slope-1000) > 1e-6 {
		t.Errorf("slope = %v, want 1000", f.Slope)
	}
	if math.Abs(f.Intercept-500) > 1e-6 {
		t.Errorf("intercept = %v, want 500", f.Intercept)
	}
	if math.Abs(f.R2-1) > 1e-9 {
		t.Errorf("R2 = %v, want 1", f.R2)
	}
	if f.SlopeStdErr != 0 {
		t.Errorf("slope std err = %v, want 0 on an exact fit", f.SlopeStdErr)
	}
	if f.XSpan != 9 {
		t.Errorf("x span = %v, want 9", f.XSpan)
	}
}

func TestFitLineTooFewSamples(t *testing.T) {
	_, err := FitLine([]Point{{X: 0, Y: 0}, {X: 1, Y: 1}}, 6)
	if err == nil {
		t.Fatal("a 2-sample fit against a 6-sample minimum must be refused")
	}
}

func TestFitLineDegenerateX(t *testing.T) {
	pts := []Point{{X: 3, Y: 1}, {X: 3, Y: 2}, {X: 3, Y: 3}}
	if _, err := FitLine(pts, 2); err == nil {
		t.Fatal("a fit where every sample shares one x must be refused, not answered with Inf")
	}
}

func TestUpperSlopeNeverBelowPointEstimate(t *testing.T) {
	f := Fit{Slope: 100, SlopeStdErr: 10}
	if got := f.UpperSlope(2); got != 120 {
		t.Errorf("UpperSlope(2) = %v, want 120", got)
	}
	if got := f.UpperSlope(-5); got != 100 {
		t.Errorf("a negative z must not pull the estimate below the point estimate; got %v", got)
	}
}

func TestEvaluateBacklogFlat(t *testing.T) {
	// Oscillating backlog with no trend: flat.
	var s []TimedValue
	for i := 0; i < 40; i++ {
		v := 10000.0
		if i%2 == 0 {
			v += 500
		}
		s = append(s, TimedValue{OffsetSec: float64(i) * 15, Value: v})
	}
	tr := EvaluateBacklog("delta_log_rows", s, 0.10, 5000, 12)
	if !tr.Evaluated {
		t.Fatalf("not evaluated: %s", tr.Error)
	}
	if !tr.Flat {
		t.Errorf("oscillating backlog judged not flat: fitted growth %v, allowance %v",
			tr.FittedGrowth, tr.AllowanceRows)
	}
}

func TestEvaluateBacklogWalkingUpFails(t *testing.T) {
	// A backlog that walks up by 2000 rows a minute over ten minutes is
	// exactly the failure mode the threshold exists for.
	var s []TimedValue
	for i := 0; i < 40; i++ {
		off := float64(i) * 15
		s = append(s, TimedValue{OffsetSec: off, Value: 1000 + 2000*(off/60)})
	}
	tr := EvaluateBacklog("delta_log_rows", s, 0.10, 5000, 12)
	if !tr.Evaluated {
		t.Fatalf("not evaluated: %s", tr.Error)
	}
	if tr.Flat {
		t.Errorf("a backlog growing %.0f rows/min was judged flat", tr.SlopePerMinute)
	}
	if math.Abs(tr.SlopePerMinute-2000) > 1 {
		t.Errorf("slope = %v rows/min, want 2000", tr.SlopePerMinute)
	}
}

func TestEvaluateBacklogTooFewSamples(t *testing.T) {
	tr := EvaluateBacklog("delta_log_rows", []TimedValue{{OffsetSec: 0, Value: 1}}, 0.1, 5000, 12)
	if tr.Evaluated || tr.Flat {
		t.Fatal("a single sample must not produce a flatness verdict")
	}
	if tr.Error == "" {
		t.Error("the trend must say why it could not be evaluated")
	}
}
