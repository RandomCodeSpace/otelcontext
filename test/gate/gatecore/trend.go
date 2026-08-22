package gatecore

import (
	"errors"
	"math"
)

// ErrTooFewSamples is returned by every fit that was not given enough data to
// answer honestly. The gate turns it into a FAILED assertion, never a blank
// report cell.
var ErrTooFewSamples = errors.New("gatecore: too few samples to fit")

// Point is one (x, y) observation for the shared least-squares fit.
type Point struct {
	X float64
	Y float64
}

// Fit is an ordinary-least-squares line plus the uncertainty of its slope.
type Fit struct {
	N         int     `json:"n"`
	Slope     float64 `json:"slope"`
	Intercept float64 `json:"intercept"`
	// SlopeStdErr is the standard error of the slope estimate. Zero when the
	// fit is exact or when n == 2 (no residual degrees of freedom).
	SlopeStdErr float64 `json:"slope_std_err"`
	// R2 is the coefficient of determination. A projection quoted off a poor
	// fit is a guess wearing a number, so the report shows it.
	R2    float64 `json:"r2"`
	XSpan float64 `json:"x_span"`
	YMin  float64 `json:"y_min"`
	YMax  float64 `json:"y_max"`
}

// FitLine performs an ordinary-least-squares fit of y on x.
//
// It is the single implementation behind both the main-tier projection and the
// backlog-growth trend, so the two cannot drift apart.
func FitLine(pts []Point, minSamples int) (Fit, error) {
	if minSamples < 2 {
		minSamples = 2
	}
	if len(pts) < minSamples {
		return Fit{N: len(pts)}, ErrTooFewSamples
	}

	n := float64(len(pts))
	var sumX, sumY float64
	for _, p := range pts {
		sumX += p.X
		sumY += p.Y
	}
	meanX, meanY := sumX/n, sumY/n

	var sxx, sxy, syy float64
	for _, p := range pts {
		dx, dy := p.X-meanX, p.Y-meanY
		sxx += dx * dx
		sxy += dx * dy
		syy += dy * dy
	}
	if sxx == 0 {
		return Fit{N: len(pts)}, errors.New("gatecore: all samples share one x; slope is undefined")
	}

	f := Fit{N: len(pts)}
	f.Slope = sxy / sxx
	f.Intercept = meanY - f.Slope*meanX

	var ssRes float64
	for _, p := range pts {
		r := p.Y - (f.Intercept + f.Slope*p.X)
		ssRes += r * r
	}
	if syy > 0 {
		f.R2 = 1 - ssRes/syy
	} else {
		f.R2 = 1
	}
	if len(pts) > 2 {
		f.SlopeStdErr = math.Sqrt(ssRes / float64(len(pts)-2) / sxx)
	}

	minX, maxX := pts[0].X, pts[0].X
	f.YMin, f.YMax = pts[0].Y, pts[0].Y
	for _, p := range pts {
		minX = math.Min(minX, p.X)
		maxX = math.Max(maxX, p.X)
		f.YMin = math.Min(f.YMin, p.Y)
		f.YMax = math.Max(f.YMax, p.Y)
	}
	f.XSpan = maxX - minX
	return f, nil
}

// UpperSlope is the conservative slope the gate projects on: the point
// estimate pushed out by z standard errors. Never below the point estimate.
func (f Fit) UpperSlope(z float64) float64 {
	if z < 0 {
		z = 0
	}
	up := f.Slope + z*f.SlopeStdErr
	if up < f.Slope {
		return f.Slope
	}
	return up
}

// BacklogTrend is the writer-backlog growth verdict over the sustained phase.
//
// "No sustained backlog growth" cannot mean "the gauge never moved" — the
// delta log fills between finalize ticks by design. It means the trend over
// the phase does not walk upward: the fitted growth across the whole phase
// stays inside an allowance, and the last sample is not above the first by
// more than that allowance.
type BacklogTrend struct {
	Metric         string  `json:"metric"`
	Samples        int     `json:"samples"`
	First          float64 `json:"first"`
	Last           float64 `json:"last"`
	Min            float64 `json:"min"`
	Max            float64 `json:"max"`
	SlopePerMinute float64 `json:"slope_rows_per_minute"`
	SpanMinutes    float64 `json:"span_minutes"`
	// FittedGrowth is SlopePerMinute * SpanMinutes: what the trend says the
	// backlog grew by across the phase.
	FittedGrowth float64 `json:"fitted_growth_rows"`
	// EndpointGrowth is Last - First.
	EndpointGrowth float64 `json:"endpoint_growth_rows"`
	AllowanceRows  float64 `json:"allowance_rows"`
	R2             float64 `json:"r2"`
	Evaluated      bool    `json:"evaluated"`
	Flat           bool    `json:"flat"`
	Error          string  `json:"error,omitempty"`
}

// TimedValue is one metric sample on the gate's own clock, in seconds since
// the phase start.
type TimedValue struct {
	OffsetSec float64
	Value     float64
}

// EvaluateBacklog fits the backlog series and decides flatness.
//
// allowanceFraction is taken against the maximum observed value, floored at
// allowanceFloor rows, so the rule scales with whatever steady state the
// deployment actually runs at instead of a number someone remembered.
func EvaluateBacklog(metric string, samples []TimedValue, allowanceFraction, allowanceFloor float64, minSamples int) BacklogTrend {
	t := BacklogTrend{Metric: metric, Samples: len(samples)}
	if len(samples) < minSamples {
		t.Error = "too few backlog samples to establish a trend"
		return t
	}

	pts := make([]Point, 0, len(samples))
	for _, s := range samples {
		pts = append(pts, Point{X: s.OffsetSec / 60, Y: s.Value})
	}
	f, err := FitLine(pts, minSamples)
	if err != nil {
		t.Error = err.Error()
		return t
	}

	t.Evaluated = true
	t.First = samples[0].Value
	t.Last = samples[len(samples)-1].Value
	t.Min, t.Max = f.YMin, f.YMax
	t.SlopePerMinute = f.Slope
	t.SpanMinutes = f.XSpan
	t.FittedGrowth = f.Slope * f.XSpan
	t.EndpointGrowth = t.Last - t.First
	t.R2 = f.R2
	t.AllowanceRows = math.Max(allowanceFloor, allowanceFraction*f.YMax)
	t.Flat = t.FittedGrowth <= t.AllowanceRows && t.EndpointGrowth <= t.AllowanceRows
	return t
}
