package gatecore

import (
	"math"
	"testing"
	"time"
)

// synthSamples builds n samples growing by bytesPerWindow per window, with an
// optional per-sample charged-bytes figure.
func synthSamples(n int, base, bytesPerWindow int64, chargedPerWindow int64) []DiskSample {
	start := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	out := make([]DiskSample, 0, n)
	for i := 0; i < n; i++ {
		w := float64(i)
		s := DiskSample{
			At:            start.Add(time.Duration(i) * 5 * time.Minute),
			PhysicalBytes: base + int64(w*float64(bytesPerWindow)),
			Windows:       w,
		}
		if chargedPerWindow > 0 {
			s.ChargedBytes = int64(w*float64(chargedPerWindow)) + 1
		}
		out = append(out, s)
	}
	return out
}

func TestFitProjectionSlopeAndHorizon(t *testing.T) {
	const perWindow = 2 * MiB
	p := FitProjection(synthSamples(24, 100*MiB, perWindow, 0), 576, 2, 6)
	if !p.Evaluated {
		t.Fatalf("not evaluated: %s", p.Error)
	}
	if math.Abs(p.BytesPerWindow-float64(perWindow)) > 1 {
		t.Errorf("bytes/window = %v, want %d", p.BytesPerWindow, perWindow)
	}
	want := int64(576) * perWindow
	if p.ProjectedBytes != want {
		t.Errorf("projected = %d, want %d", p.ProjectedBytes, want)
	}
	// An exact fit has zero slope error, so the upper estimate equals the
	// point estimate rather than drifting somewhere convenient.
	if p.ProjectedUpperBytes != want {
		t.Errorf("projected upper = %d, want %d on an exact fit", p.ProjectedUpperBytes, want)
	}
	if p.HorizonWinds != 576 {
		t.Errorf("horizon = %d, want 576", p.HorizonWinds)
	}
	if p.Label == "" {
		t.Error("the projection must be labelled as a projection")
	}
}

func TestFitProjectionUpperEstimateExceedsPointEstimate(t *testing.T) {
	// Noisy samples: the conservative upper estimate must be strictly above
	// the point estimate, which is the whole reason it exists.
	s := synthSamples(20, 100*MiB, 1*MiB, 0)
	for i := range s {
		if i%3 == 0 {
			s[i].PhysicalBytes += 3 * MiB
		}
	}
	p := FitProjection(s, 576, 2, 6)
	if !p.Evaluated {
		t.Fatalf("not evaluated: %s", p.Error)
	}
	if p.Fit.SlopeStdErr <= 0 {
		t.Fatalf("noisy samples produced a zero slope std err")
	}
	if p.ProjectedUpperBytes <= p.ProjectedBytes {
		t.Errorf("upper estimate %d is not above the point estimate %d",
			p.ProjectedUpperBytes, p.ProjectedBytes)
	}
}

func TestFitProjectionAmplificationIsReportedNotApplied(t *testing.T) {
	// Physical grows 4x faster than charged. The amplification factor must
	// come out as 4.0 AND the projection must stay at physical slope x
	// horizon — multiplying by amplification again would charge the indexes
	// twice.
	const physical = 4 * MiB
	const charged = 1 * MiB
	p := FitProjection(synthSamples(24, 100*MiB, physical, charged), 576, 2, 6)
	if !p.Evaluated {
		t.Fatalf("not evaluated: %s", p.Error)
	}
	if !p.AmplificationMeasured {
		t.Fatal("amplification was not measured despite charged bytes being present")
	}
	if math.Abs(p.AmplificationFactor-4) > 0.01 {
		t.Errorf("amplification = %v, want 4", p.AmplificationFactor)
	}
	want := int64(576) * physical
	if p.ProjectedBytes != want {
		t.Errorf("projected = %d, want %d — amplification must NOT be multiplied in",
			p.ProjectedBytes, want)
	}
	if p.ProjectedBytes == want*4 {
		t.Fatal("the projection double-charged the amplification factor")
	}
}

func TestFitProjectionRefusesTooFewSamples(t *testing.T) {
	p := FitProjection(synthSamples(3, 100*MiB, 1*MiB, 0), 576, 2, 6)
	if p.Evaluated {
		t.Fatal("a 3-sample projection against a 6-sample minimum must not be evaluated")
	}
	if p.Error == "" {
		t.Error("the projection must say why it was refused")
	}
	if p.ProjectedUpperBytes != 0 {
		t.Errorf("a refused projection must not carry a number; got %d", p.ProjectedUpperBytes)
	}
}

func TestFitProjectionShrinkingTierProjectsZero(t *testing.T) {
	s := synthSamples(12, 500*MiB, 0, 0)
	for i := range s {
		s[i].PhysicalBytes -= int64(i) * MiB
	}
	p := FitProjection(s, 576, 2, 6)
	if !p.Evaluated {
		t.Fatalf("not evaluated: %s", p.Error)
	}
	if p.ProjectedBytes < 0 || p.ProjectedUpperBytes < 0 {
		t.Errorf("a shrinking tier projected a negative footprint: %d / %d",
			p.ProjectedBytes, p.ProjectedUpperBytes)
	}
}
